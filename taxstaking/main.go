// taxstaking keeps the Cosmos Hub staking tables (taxapi/stakingtables.go)
// behind /tax/staking: issuance, community pool and commission per UTC day,
// rewards and commission per validator per day, and unbonding by completion
// day. The API reads them; this command is their only writer.
//
//	-seed-merged F -seed-byval F[,F]  load the extraction's exact history once, into empty tables
//	-refresh                          add every whole UTC day since the last one, from our node
//	-check FROM TO                    recompute days FROM..TO from the node and compare with the tables
//	-compute FROM TO                  print days FROM..TO computed from the node (no database)
//	-parse LO HI -bounds F            print per-day sums of heights LO..HI (proof runs, no database)
//
// Refresh writes whole days only, each with the state row in one
// transaction, and refuses a day with a missing, unreadable or mint-less
// block. It is safe to rerun: the state says where to start. Every
// refresh ends with one line, "staking-daily ok ..." or
// "staking-daily error ...", which the alert rules read.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS DB_LOG_LEVEL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"gorm.io/gorm"
)

const defaultRPC = "http://gaia-cosmoshub-4-fullnode-rpc.fullnodes.svc.cluster.local:26657"

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	log.SetFlags(0)
	os.Exit(run())
}

func connect() (*gorm.DB, func(), error) {
	db, err := indexerDB.PostgresDbConnect(env("DB_HOST", "localhost"), env("DB_PORT", "5432"), env("DB_NAME", "cosmostax"),
		env("DB_USER", "postgres"), env("DB_PASS", "postgres"), env("DB_LOG_LEVEL", "warn"), "jit=off")
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	return db, func() { _ = pool.Close() }, nil
}

func run() int {
	seedMerged := flag.String("seed-merged", "", "merged daily file (merge.py output) to seed from")
	seedByval := flag.String("seed-byval", "", "per-validator pass files (comma separated jsonl) to seed from")
	dryRun := flag.Bool("dry-run", false, "with -seed-*: run every check, write nothing")
	refresh := flag.Bool("refresh", false, "add every whole UTC day since the last one")
	check := flag.Bool("check", false, "recompute days FROM TO (args) from the node and compare with the tables")
	compute := flag.Bool("compute", false, "print days FROM TO (args) computed from the node")
	parse := flag.Bool("parse", false, "print per-day sums of heights LO HI (args)")
	bounds := flag.String("bounds", "", "with -parse: day start heights (boundaries.py output)")
	cache := flag.String("cache", "", "with -parse: read raw block results from this cache folder instead of the node")
	rpc := flag.String("rpc", env("STAKING_RPC", defaultRPC), "our node's CometBFT RPC")
	chain := flag.String("chain", taxapi.StakingChain, "chain id")
	workers := flag.Int("workers", 4, "parallel block reads")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n := newNode(*rpc, *workers)
	if *compute || *check {
		progress = func(f string, a ...any) { log.Printf("staking-compute "+f, a...) }
	}
	switch {
	case *seedMerged != "" || *seedByval != "":
		return runSeed(*chain, *seedMerged, *seedByval, *dryRun)
	case *refresh:
		return runRefresh(ctx, n, *chain, *workers)
	case *check:
		return runCheck(ctx, n, *chain, *workers, flag.Args())
	case *compute:
		return runCompute(ctx, n, *chain, *workers, flag.Args())
	case *parse:
		return runParse(ctx, n, *workers, *bounds, *cache, flag.Args())
	}
	flag.Usage()
	return 2
}

func runSeed(chain, merged, byval string, dry bool) int {
	if merged == "" || byval == "" {
		log.Printf("staking-seed error: -seed-merged and -seed-byval are both required")
		return 2
	}
	began := time.Now()
	sd, err := prepareSeed(chain, merged, strings.Split(byval, ","))
	if err != nil {
		log.Printf("staking-seed error: %v", err)
		return 1
	}
	summary := fmt.Sprintf("chain=%s days=%d first_day=%s last_day=%s last_height=%d validator_rows=%d unbonding_rows=%d",
		chain, len(sd.Days), sd.State.FirstDay, sd.State.LastDay, sd.State.LastHeight, len(sd.Vals), len(sd.Unbond))
	if dry {
		log.Printf("staking-seed checked (dry run, nothing written) %s", summary)
		return 0
	}
	db, closeDB, err := connect()
	if err != nil {
		log.Printf("staking-seed error: db: %v", err)
		return 1
	}
	defer closeDB()
	if err := loadSeed(db, sd); err != nil {
		log.Printf("staking-seed error: %v", err)
		return 1
	}
	log.Printf("staking-seed ok %s secs=%d", summary, int(time.Since(began).Seconds()))
	return 0
}

func lagDays(lastDay string, now time.Time) int {
	d, err := time.Parse(time.DateOnly, lastDay)
	if err != nil {
		return -1
	}
	yesterday := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	return int(yesterday.Sub(d).Hours() / 24)
}

func runRefresh(ctx context.Context, n *node, chain string, workers int) int {
	began := time.Now()
	// at= is the run's unix time: the failed alert compares the newest error
	// line with the newest ok line, so a successful rerun clears it.
	at := began.Unix()
	db, closeDB, err := connect()
	if err != nil {
		log.Printf("staking-daily error chain=%s at=%d err=%q", chain, at, "db: "+err.Error())
		return 1
	}
	defer closeDB()
	st, added, err := refreshDays(ctx, db, n, chain, workers, time.Now())
	if errors.Is(err, errStateMoved) {
		// Another writer extended the tables while this run computed the
		// same day; its rows stand. Report the state it left.
		log.Printf("staking-daily busy chain=%s another writer extended the tables", chain)
		var ok bool
		st, ok, err = readState(db, chain)
		if err == nil && !ok {
			err = errors.New("state row vanished")
		}
	}
	secs := int(time.Since(began).Seconds())
	if err != nil {
		log.Printf("staking-daily error chain=%s at=%d last_day=%s last_height=%d lag_days=%d added=%d secs=%d err=%q",
			chain, at, st.LastDay, st.LastHeight, lagDays(st.LastDay, time.Now()), added, secs, err.Error())
		return 1
	}
	log.Printf("staking-daily ok chain=%s last_day=%s last_height=%d lag_days=%d at=%d added=%d secs=%d",
		chain, st.LastDay, st.LastHeight, lagDays(st.LastDay, time.Now()), at, added, secs)
	return 0
}

// refreshDays adds each whole UTC day after the state's last day, up to
// yesterday, one transaction per day. It returns the state it ended at.
func refreshDays(ctx context.Context, db *gorm.DB, n *node, chain string, workers int, now time.Time) (taxapi.StakingState, int, error) {
	st, ok, err := readState(db, chain)
	if err != nil {
		return st, 0, fmt.Errorf("read state: %v", err)
	}
	if !ok {
		return st, 0, errors.New("not seeded: no state row")
	}
	if st.Version != taxapi.StakingTablesVersion {
		return st, 0, fmt.Errorf("tables are version %d, this build writes %d", st.Version, taxapi.StakingTablesVersion)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	last, err := time.Parse(time.DateOnly, st.LastDay)
	if err != nil {
		return st, 0, err
	}
	next := last.AddDate(0, 0, 1)
	if !next.Before(today) {
		return st, 0, nil
	}
	ns, err := n.status(ctx)
	if err != nil {
		return st, 0, fmt.Errorf("node status: %v", err)
	}
	if ns.Network != chain {
		return st, 0, fmt.Errorf("node serves %q, not %s", ns.Network, chain)
	}
	start := st.LastHeight + 1
	if start <= ns.EarliestHeight {
		// The first block of a day must be checked against the block before it.
		return st, 0, fmt.Errorf("node no longer holds height %d (earliest %d); refresh needs an archive source", start-1, ns.EarliestHeight)
	}
	days, err := planDays(ctx, n, start, next.Format(time.DateOnly), today, ns.LatestHeight)
	if err != nil {
		return st, 0, fmt.Errorf("plan days: %v", err)
	}
	if len(days) == 0 {
		return st, 0, fmt.Errorf("node has no block after %s yet (latest %d at %s)", next.AddDate(0, 0, 1).Format(time.DateOnly),
			ns.LatestHeight, ns.LatestTime.Format(time.RFC3339))
	}
	added := 0
	for _, r := range days {
		cs, err := computeDays(ctx, n, []dayRange{r}, workers, true)
		if err != nil {
			return st, added, err
		}
		if err := appendDay(db, chain, st, cs[0]); err != nil {
			return st, added, err
		}
		log.Printf("staking-daily added day=%s heights=%d-%d validators=%d", r.Day, r.First, r.Last, len(cs[0].Vals))
		st.LastDay, st.LastHeight = r.Day, r.Last
		added++
	}
	return st, added, nil
}

func dayArgs(args []string) (time.Time, time.Time, error) {
	if len(args) != 2 {
		return time.Time{}, time.Time{}, errors.New("want FROM TO (YYYY-MM-DD)")
	}
	a, err1 := time.Parse(time.DateOnly, args[0])
	b, err2 := time.Parse(time.DateOnly, args[1])
	if err1 != nil || err2 != nil || b.Before(a) {
		return time.Time{}, time.Time{}, errors.New("want FROM TO (YYYY-MM-DD, FROM <= TO)")
	}
	return a, b, nil
}

// nodeDays plans days from..to from the node's own headers.
func nodeDays(ctx context.Context, n *node, chain string, from, to time.Time) ([]dayRange, error) {
	ns, err := n.status(ctx)
	if err != nil {
		return nil, err
	}
	if ns.Network != chain {
		return nil, fmt.Errorf("node serves %q, not %s", ns.Network, chain)
	}
	start, err := n.firstHeightAtOrAfter(ctx, from, ns.EarliestHeight, ns.LatestHeight)
	if err != nil {
		return nil, err
	}
	if start <= ns.EarliestHeight {
		return nil, fmt.Errorf("node holds blocks from %d only; %s may start earlier", ns.EarliestHeight, from.Format(time.DateOnly))
	}
	days, err := planDays(ctx, n, start, from.Format(time.DateOnly), to.AddDate(0, 0, 1), ns.LatestHeight)
	if err != nil {
		return nil, err
	}
	if len(days) == 0 || days[len(days)-1].Day != to.Format(time.DateOnly) {
		return nil, fmt.Errorf("node does not hold every whole day to %s", to.Format(time.DateOnly))
	}
	return days, nil
}

func runCompute(ctx context.Context, n *node, chain string, workers int, args []string) int {
	from, to, err := dayArgs(args)
	if err != nil {
		log.Printf("staking-compute error: %v", err)
		return 2
	}
	days, err := nodeDays(ctx, n, chain, from, to)
	if err != nil {
		log.Printf("staking-compute error: %v", err)
		return 1
	}
	cs, err := computeDays(ctx, n, days, workers, false)
	enc := json.NewEncoder(os.Stdout)
	for _, c := range cs {
		_ = enc.Encode(dayReport(c))
	}
	if err != nil {
		log.Printf("staking-compute error: %v", err)
		return 1
	}
	return 0
}

// dayReport is a day's raw sums in the extraction's own layout, plus the
// row it makes, for comparing with extract_v.py output.
func dayReport(c computedDay) map[string]any {
	d := c.Agg
	out := map[string]any{"day": c.Range.Day, "first_height": c.Range.First, "last_height": c.Range.Last}
	if d == nil {
		return out
	}
	snapJSON := func(s *snap) any {
		if s == nil {
			return nil
		}
		return []any{s.Height, s.Inflation, s.Bonded, s.Provision, s.Amount}
	}
	props := make([][3]any, 0, len(d.Proposals))
	for _, p := range d.Proposals {
		props = append(props, [3]any{p.Height, p.ID, p.Result})
	}
	vals := map[string][2]string{}
	for op, v := range d.Vals {
		vals[op] = [2]string{fixString(v[0]), fixString(v[1])}
	}
	unb := map[string]string{}
	for k, v := range d.Unbond {
		unb[k] = fixString(v)
	}
	out["blocks"] = d.Blocks
	out["minted"] = d.Minted
	out["rewards"] = fixString(d.Rewards)
	out["commission"] = fixString(d.Commission)
	out["first"] = snapJSON(d.First)
	out["last"] = snapJSON(d.Last)
	if d.Blocks > 0 {
		out["infl_avg8"] = meanPlaces(d.InflSum, d.Blocks, 8)
		out["bonded_avg8"] = meanPlaces(d.BondedSum, d.Blocks, 8)
	}
	out["proposals"] = props
	out["validators"] = vals
	out["withdrawn_commission"] = fixString(d.WithdrawC)
	out["withdrawn_rewards"] = fixString(d.WithdrawR)
	out["unbond_by_completion"] = unb
	out["missing"] = d.Missing
	out["no_mint"] = d.NoMint
	out["bad"] = d.Bad
	if c.Row.Day != "" {
		out["row"] = c.Row
	}
	return out
}

func runParse(ctx context.Context, n *node, workers int, boundsPath, cacheDir string, args []string) int {
	if len(args) != 2 || boundsPath == "" {
		log.Printf("staking-parse error: want -bounds F LO HI")
		return 2
	}
	lo, err1 := strconv.ParseInt(args[0], 10, 64)
	hi, err2 := strconv.ParseInt(args[1], 10, 64)
	if err1 != nil || err2 != nil || hi < lo {
		log.Printf("staking-parse error: bad heights")
		return 2
	}
	b, err := os.ReadFile(boundsPath)
	if err != nil {
		log.Printf("staking-parse error: %v", err)
		return 1
	}
	var bf struct {
		Days [][2]json.RawMessage `json:"days"`
	}
	if err := json.Unmarshal(b, &bf); err != nil {
		log.Printf("staking-parse error: bounds: %v", err)
		return 1
	}
	var starts []int64
	var names []string
	for _, d := range bf.Days {
		var k string
		var h int64
		if json.Unmarshal(d[0], &k) != nil || json.Unmarshal(d[1], &h) != nil {
			log.Printf("staking-parse error: bounds row")
			return 1
		}
		names, starts = append(names, k), append(starts, h)
	}
	var src blockSource = n
	if cacheDir != "" {
		src = cacheSource{root: cacheDir}
	}
	aggs, err := aggregate(ctx, src, lo, hi, dayOfFunc(starts, names), workers)
	if err != nil {
		log.Printf("staking-parse error: %v", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	for i, k := range names {
		d, ok := aggs[k]
		if !ok {
			continue
		}
		r := dayRange{Day: k, First: max(starts[i], lo), Last: hi}
		if i+1 < len(starts) {
			r.Last = min(starts[i+1]-1, hi)
		}
		_ = enc.Encode(dayReport(computedDay{Range: r, Agg: d}))
	}
	return 0
}
