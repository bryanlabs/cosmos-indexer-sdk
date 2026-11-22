package taxapi

// Staking tables (INF-748). The taxstaking Job seeds these once from the full
// extraction (the same data as the embedded inflation and validator income
// files) and extends them each day after 00:00 UTC by whole days, from our
// node's block results. GET /staking/inflation and /staking/validator-income
// serve them once they cover at least what the embedded files cover:
// tax_staking_state exists with the current version, its last day is at or
// past the embedded files' last day, and every day from the embedded first
// day is present with no height gap. Otherwise the embedded files are served,
// with the same result for the same data.
//
// The API only reads. It checks tax_staking_state at most every ten minutes
// and reloads the rows only when updated_at (or the frontier) moved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	// StakingTablesVersion is the row format taxstaking writes and the API reads.
	StakingTablesVersion = 1
	// StakingChain is the chain the staking views cover.
	StakingChain = "cosmoshub-4"
	// StakingLockKey serializes taxstaking writers (session advisory lock).
	StakingLockKey int64 = 748_000_001

	stakingCacheTTL    = 10 * time.Minute
	stakingLoadTimeout = 90 * time.Second
)

// StakingTablesDDL creates the staking tables. The database owner runs the
// copy in bare-metal cluster/apps/cosmos-tax-staking-daily/staking.sql; this
// one is for tests.
const StakingTablesDDL = `
CREATE TABLE IF NOT EXISTS tax_staking_state (
    chain text PRIMARY KEY,
    first_day date NOT NULL,
    first_height bigint NOT NULL,
    first_block_time timestamptz NOT NULL,
    last_day date NOT NULL,
    last_height bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    source text NOT NULL,
    version integer NOT NULL
);
CREATE TABLE IF NOT EXISTS tax_staking_days (
    chain text NOT NULL,
    day date NOT NULL,
    first_height bigint NOT NULL,
    last_height bigint NOT NULL,
    blocks bigint NOT NULL,
    minted_uatom numeric NOT NULL,
    community_pool_uatom numeric NOT NULL,
    stakers_uatom numeric NOT NULL,
    commission_uatom numeric NOT NULL,
    rewards_events_uatom numeric NOT NULL,
    commission_events_uatom numeric NOT NULL,
    supply_start_uatom numeric NOT NULL,
    inflation_param_start numeric NOT NULL,
    inflation_param_avg numeric NOT NULL,
    bonded_ratio_avg numeric NOT NULL,
    community_tax_start numeric NOT NULL,
    withdrawn_commission_uatom numeric NOT NULL,
    withdrawn_rewards_uatom numeric NOT NULL,
    proposals jsonb NOT NULL,
    PRIMARY KEY (chain, day),
    CHECK (blocks = last_height - first_height + 1)
);
CREATE TABLE IF NOT EXISTS tax_staking_validator_days (
    chain text NOT NULL,
    day date NOT NULL,
    operator text NOT NULL,
    rewards_uatom numeric NOT NULL,
    commission_uatom numeric NOT NULL,
    PRIMARY KEY (chain, day, operator)
);
CREATE TABLE IF NOT EXISTS tax_staking_unbonding (
    chain text NOT NULL,
    day date NOT NULL,
    completion_day date NOT NULL,
    amount_uatom numeric NOT NULL,
    PRIMARY KEY (chain, day, completion_day)
);
`

// StakingState is the one tax_staking_state row of a chain.
type StakingState struct {
	Chain          string
	FirstDay       string
	LastDay        string
	FirstHeight    int64
	LastHeight     int64
	FirstBlockTime time.Time
	UpdatedAt      time.Time
	Source         string
	Version        int
}

// StakingDayRow is a tax_staking_days row with every numeric column as its
// exact decimal text.
type StakingDayRow struct {
	Day                 string
	FirstHeight         int64
	LastHeight          int64
	Blocks              int64
	Minted              string
	CommunityPool       string
	Stakers             string
	Commission          string
	RewardsEvents       string
	CommissionEvents    string
	SupplyStart         string
	InflationStart      string
	InflationAvg        string
	BondedAvg           string
	CommunityTax        string
	WithdrawnCommission string
	WithdrawnRewards    string
	Proposals           string
}

// StakingValidatorRow is a tax_staking_validator_days row.
type StakingValidatorRow struct {
	Day        string
	Operator   string
	Rewards    string
	Commission string
}

var errStakingTablesUnavailable = errors.New("staking tables unavailable")

// ParseFix18 reads an exact non-negative or negative decimal with at most 18
// fractional digits (the precision of chain DecCoins) as an integer scaled by
// 1e18. "5.", ".5" and "5" are accepted; anything else is an error.
func ParseFix18(s string) (*big.Int, error) {
	neg := false
	t := s
	if strings.HasPrefix(t, "-") {
		neg, t = true, t[1:]
	}
	ip, fp, _ := strings.Cut(t, ".")
	if ip == "" && fp == "" || len(fp) > 18 || strings.Contains(fp, ".") {
		return nil, fmt.Errorf("bad decimal %q", s)
	}
	for _, c := range ip + fp {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("bad decimal %q", s)
		}
	}
	v, ok := new(big.Int).SetString("0"+ip+fp+strings.Repeat("0", 18-len(fp)), 10)
	if !ok {
		return nil, fmt.Errorf("bad decimal %q", s)
	}
	if neg {
		v.Neg(v)
	}
	return v, nil
}

var fix18One = big.NewInt(1_000_000_000_000_000_000)

// Fix18String writes a 1e18-scaled integer as the shortest exact decimal.
func Fix18String(v *big.Int) string {
	a := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(a, fix18One, new(big.Int))
	out := q.String()
	if r.Sign() != 0 {
		f := r.String()
		f = strings.Repeat("0", 18-len(f)) + f
		out += "." + strings.TrimRight(f, "0")
	}
	if v.Sign() < 0 {
		out = "-" + out
	}
	return out
}

// Fix18Floor is the floor of a 1e18-scaled amount as a whole number.
func Fix18Floor(v *big.Int) *big.Int {
	return new(big.Int).Div(v, fix18One)
}

type stakingSet struct {
	infl *inflationData
	inc  *incomeData
}

type stakingTables struct {
	db  *gorm.DB
	now func() time.Time
	// load reads the state and, when wanted, every row (tests replace it).
	load func(ctx context.Context, rows bool) (st StakingState, ok bool, days []StakingDayRow, vals []StakingValidatorRow, err error)

	mu      sync.Mutex
	checked time.Time
	key     string
	set     *stakingSet
	reason  string
}

func newStakingTables(db *gorm.DB) *stakingTables {
	if db == nil {
		return nil
	}
	t := &stakingTables{db: db, now: time.Now}
	t.load = t.loadFromDB
	return t
}

// stakingInflation is the inflation series the staking views serve.
func (s *Server) stakingInflation(ctx context.Context) (*inflationData, error) {
	if set := s.staking.current(ctx); set != nil {
		return set.infl, nil
	}
	return loadInflation()
}

// stakingSets is the inflation series and validator income the income views serve.
func (s *Server) stakingSets(ctx context.Context) (*inflationData, *incomeData, error) {
	if set := s.staking.current(ctx); set != nil {
		return set.infl, set.inc, nil
	}
	infl, err := loadInflation()
	if err != nil {
		return nil, nil, err
	}
	inc, err := loadIncome()
	if err != nil {
		return nil, nil, err
	}
	return infl, inc, nil
}

// current returns the table-built data, or nil to serve the embedded files.
func (t *stakingTables) current(_ context.Context) *stakingSet {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.checked.IsZero() && now.Sub(t.checked) < stakingCacheTTL {
		return t.set
	}
	t.checked = now
	// A request that ends early must not leave a half load cached for ten minutes.
	ctx, cancel := context.WithTimeout(context.Background(), stakingLoadTimeout)
	defer cancel()
	set, key, err := t.refresh(ctx)
	switch {
	case err == nil:
		t.set, t.key = set, key
		t.note("")
	case errors.Is(err, errStakingTablesUnavailable):
		t.set, t.key = nil, ""
		t.note(err.Error())
	default:
		// A failed read keeps the last good tables until the next check.
		t.note("staking tables read failed, keeping the previous data: " + err.Error())
	}
	return t.set
}

func (t *stakingTables) note(reason string) {
	if reason != t.reason {
		if reason == "" {
			log.Printf("staking views: serving the staking tables (%s)", t.key)
		} else {
			log.Printf("staking views: %s", reason)
		}
		t.reason = reason
	}
}

func (t *stakingTables) refresh(ctx context.Context) (*stakingSet, string, error) {
	embInfl, err := loadInflation()
	if err != nil {
		return nil, "", fmt.Errorf("%w: embedded inflation file: %v", errStakingTablesUnavailable, err)
	}
	embInc, err := loadIncome()
	if err != nil {
		return nil, "", fmt.Errorf("%w: embedded income file: %v", errStakingTablesUnavailable, err)
	}
	st, ok, _, _, err := t.load(ctx, false)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("%w: no readable state row", errStakingTablesUnavailable)
	}
	if err := stakingStateUsable(st, embInfl, embInc); err != nil {
		return nil, "", err
	}
	key := fmt.Sprintf("%s|%d|%s|%d|%d", st.UpdatedAt.UTC().Format(time.RFC3339Nano), st.Version, st.LastDay, st.LastHeight, st.FirstHeight)
	if key == t.key && t.set != nil {
		return t.set, key, nil
	}
	st, ok, days, vals, err := t.load(ctx, true)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("%w: no readable state row", errStakingTablesUnavailable)
	}
	if err := stakingStateUsable(st, embInfl, embInc); err != nil {
		return nil, "", err
	}
	key = fmt.Sprintf("%s|%d|%s|%d|%d", st.UpdatedAt.UTC().Format(time.RFC3339Nano), st.Version, st.LastDay, st.LastHeight, st.FirstHeight)
	set, err := buildStakingSet(st, days, vals, embInfl, embInc)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errStakingTablesUnavailable, err)
	}
	return set, key, nil
}

// stakingStateUsable is the rule for serving the tables instead of the
// embedded files; the full rows are checked again when built.
func stakingStateUsable(st StakingState, embInfl *inflationData, embInc *incomeData) error {
	if st.Version != StakingTablesVersion {
		return fmt.Errorf("%w: state version %d, want %d", errStakingTablesUnavailable, st.Version, StakingTablesVersion)
	}
	if st.Chain != embInfl.Chain {
		return fmt.Errorf("%w: state chain %s", errStakingTablesUnavailable, st.Chain)
	}
	first, last := embeddedStakingSpan(embInfl, embInc)
	if st.LastDay < last {
		return fmt.Errorf("%w: tables end %s, before the embedded files' %s", errStakingTablesUnavailable, st.LastDay, last)
	}
	if st.FirstDay > first {
		return fmt.Errorf("%w: tables start %s, after the embedded files' %s", errStakingTablesUnavailable, st.FirstDay, first)
	}
	return nil
}

func embeddedStakingSpan(embInfl *inflationData, embInc *incomeData) (string, string) {
	first, last := embInfl.Days[0].Date, embInfl.Days[len(embInfl.Days)-1].Date
	if embInc != nil {
		if embInc.File.FirstDay < first {
			first = embInc.File.FirstDay
		}
		if embInc.File.LastDay > last {
			last = embInc.File.LastDay
		}
	}
	return first, last
}

func (t *stakingTables) loadFromDB(ctx context.Context, rows bool) (st StakingState, ok bool, days []StakingDayRow, vals []StakingValidatorRow, err error) {
	err = t.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var readable bool
		if err := tx.Raw(`SELECT coalesce(has_table_privilege(to_regclass('tax_staking_state'), 'SELECT')
 AND has_table_privilege(to_regclass('tax_staking_days'), 'SELECT')
 AND has_table_privilege(to_regclass('tax_staking_validator_days'), 'SELECT'), false)`).Scan(&readable).Error; err != nil {
			return err
		}
		if !readable {
			return nil
		}
		var found bool
		var err error
		st, found, err = ReadStakingState(tx, StakingChain)
		if err != nil || !found {
			return err
		}
		ok = true
		if !rows {
			return nil
		}
		days, err = ReadStakingDays(tx, st.Chain, st.FirstDay, st.LastDay)
		if err != nil {
			return err
		}
		vals, err = ReadStakingValidatorDays(tx, st.Chain, st.FirstDay, st.LastDay)
		return err
	}, rollupReadOnly())
	return st, ok, days, vals, err
}

// ReadStakingState reads a chain's state row.
func ReadStakingState(db *gorm.DB, chain string) (StakingState, bool, error) {
	var st StakingState
	rows, err := db.Raw(`SELECT chain, first_day::text, last_day::text, first_height, last_height, first_block_time, updated_at, source, version
 FROM tax_staking_state WHERE chain = ?`, chain).Rows()
	if err != nil {
		return st, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return st, false, rows.Err()
	}
	if err := rows.Scan(&st.Chain, &st.FirstDay, &st.LastDay, &st.FirstHeight, &st.LastHeight, &st.FirstBlockTime, &st.UpdatedAt, &st.Source, &st.Version); err != nil {
		return st, false, err
	}
	return st, true, rows.Err()
}

// ReadStakingDays reads day rows from..to (inclusive) in date order.
func ReadStakingDays(db *gorm.DB, chain, from, to string) ([]StakingDayRow, error) {
	rows, err := db.Raw(`SELECT day::text, first_height, last_height, blocks, minted_uatom::text, community_pool_uatom::text,
 stakers_uatom::text, commission_uatom::text, rewards_events_uatom::text, commission_events_uatom::text,
 supply_start_uatom::text, inflation_param_start::text, inflation_param_avg::text, bonded_ratio_avg::text,
 community_tax_start::text, withdrawn_commission_uatom::text, withdrawn_rewards_uatom::text, proposals::text
 FROM tax_staking_days WHERE chain = ? AND day BETWEEN ?::date AND ?::date ORDER BY day`, chain, from, to).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StakingDayRow
	for rows.Next() {
		var r StakingDayRow
		if err := rows.Scan(&r.Day, &r.FirstHeight, &r.LastHeight, &r.Blocks, &r.Minted, &r.CommunityPool, &r.Stakers,
			&r.Commission, &r.RewardsEvents, &r.CommissionEvents, &r.SupplyStart, &r.InflationStart, &r.InflationAvg,
			&r.BondedAvg, &r.CommunityTax, &r.WithdrawnCommission, &r.WithdrawnRewards, &r.Proposals); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadStakingValidatorDays reads validator rows from..to (inclusive) by day and operator.
func ReadStakingValidatorDays(db *gorm.DB, chain, from, to string) ([]StakingValidatorRow, error) {
	rows, err := db.Raw(`SELECT day::text, operator, rewards_uatom::text, commission_uatom::text
 FROM tax_staking_validator_days WHERE chain = ? AND day BETWEEN ?::date AND ?::date ORDER BY day, operator`, chain, from, to).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StakingValidatorRow
	for rows.Next() {
		var r StakingValidatorRow
		if err := rows.Scan(&r.Day, &r.Operator, &r.Rewards, &r.Commission); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// buildStakingSet turns the table rows into the same structures the embedded
// files parse into, with the validator income built the way the extraction's
// export (export_v.py) builds its file: per validator, from its first to its
// last day with a row, daily commission as the change in the floor of the
// running exact total, rewards as the floor of each month's exact total, and
// withdrawals as the floor of each day's exact total.
func buildStakingSet(st StakingState, days []StakingDayRow, vals []StakingValidatorRow, embInfl *inflationData, embInc *incomeData) (*stakingSet, error) {
	if len(days) == 0 {
		return nil, errors.New("no day rows")
	}
	if days[0].Day != st.FirstDay || days[len(days)-1].Day != st.LastDay {
		return nil, fmt.Errorf("day rows run %s to %s, state says %s to %s", days[0].Day, days[len(days)-1].Day, st.FirstDay, st.LastDay)
	}
	if days[0].FirstHeight != st.FirstHeight || days[len(days)-1].LastHeight != st.LastHeight {
		return nil, fmt.Errorf("day rows run heights %d to %d, state says %d to %d", days[0].FirstHeight, days[len(days)-1].LastHeight, st.FirstHeight, st.LastHeight)
	}
	asOf := st.UpdatedAt.UTC().Format(time.RFC3339)
	firstTime := st.FirstBlockTime.UTC()
	infl := &inflationData{Chain: st.Chain, AsOf: asOf, FirstBlockTime: firstTime.Format(time.RFC3339),
		FirstHeight: days[0].FirstHeight, LastHeight: days[len(days)-1].LastHeight, ProposalHeights: map[string]int64{}}
	order := make([]string, len(days))
	index := make(map[string]int, len(days))
	withdrawn := make([][2]int64, len(days))
	for i, r := range days {
		if i > 0 {
			prev, _ := time.Parse(time.DateOnly, days[i-1].Day)
			if prev.AddDate(0, 0, 1).Format(time.DateOnly) != r.Day {
				return nil, fmt.Errorf("day %s does not follow %s", r.Day, days[i-1].Day)
			}
			if r.FirstHeight != days[i-1].LastHeight+1 {
				return nil, fmt.Errorf("day %s starts at height %d, not %d", r.Day, r.FirstHeight, days[i-1].LastHeight+1)
			}
		}
		if r.Blocks != r.LastHeight-r.FirstHeight+1 {
			return nil, fmt.Errorf("day %s counts %d blocks for heights %d to %d", r.Day, r.Blocks, r.FirstHeight, r.LastHeight)
		}
		day := inflationDay{Date: r.Day, FirstHeight: r.FirstHeight, LastHeight: r.LastHeight, Blocks: r.Blocks, CommunityTax: r.CommunityTax}
		for _, it := range []struct {
			name, v string
			dst     **big.Int
		}{{"minted_uatom", r.Minted, &day.Minted}, {"community_pool_uatom", r.CommunityPool, &day.CommunityPool},
			{"stakers_uatom", r.Stakers, &day.Stakers}, {"commission_uatom", r.Commission, &day.Commission},
			{"rewards_events_uatom", r.RewardsEvents, &day.RewardsEvents}, {"commission_events_uatom", r.CommissionEvents, &day.CommissionEvents},
			{"supply_start_uatom", r.SupplyStart, &day.SupplyStart}} {
			v, ok := new(big.Int).SetString(it.v, 10)
			if !ok {
				return nil, fmt.Errorf("day %s: %s %q is not a whole number", r.Day, it.name, it.v)
			}
			*it.dst = v
		}
		var err error
		if day.InflationAvg, err = strconv.ParseFloat(r.InflationAvg, 64); err != nil {
			return nil, fmt.Errorf("day %s: inflation_param_avg: %v", r.Day, err)
		}
		if day.BondedAvg, err = strconv.ParseFloat(r.BondedAvg, 64); err != nil {
			return nil, fmt.Errorf("day %s: bonded_ratio_avg: %v", r.Day, err)
		}
		if day.Seconds, err = inflationDaySeconds(r.Day, firstTime); err != nil {
			return nil, err
		}
		var props [][3]json.RawMessage
		if err := json.Unmarshal([]byte(r.Proposals), &props); err != nil {
			return nil, fmt.Errorf("day %s: proposals: %v", r.Day, err)
		}
		for _, p := range props {
			var h int64
			var pid string
			if err := json.Unmarshal(p[0], &h); err != nil {
				return nil, fmt.Errorf("day %s: proposal height: %v", r.Day, err)
			}
			if err := json.Unmarshal(p[1], &pid); err != nil {
				return nil, fmt.Errorf("day %s: proposal id: %v", r.Day, err)
			}
			infl.ProposalHeights[pid] = h
		}
		infl.BlocksCounted += r.Blocks
		infl.Days = append(infl.Days, day)
		order[i] = r.Day
		index[r.Day] = i
		for j, s := range []string{r.WithdrawnCommission, r.WithdrawnRewards} {
			v, err := ParseFix18(s)
			if err != nil {
				return nil, fmt.Errorf("day %s: withdrawn: %v", r.Day, err)
			}
			f := Fix18Floor(v)
			if !f.IsInt64() {
				return nil, fmt.Errorf("day %s: withdrawn %s out of range", r.Day, s)
			}
			withdrawn[i][j] = f.Int64()
		}
	}
	infl.BlocksExpected = infl.LastHeight - infl.FirstHeight + 1
	if infl.BlocksCounted != infl.BlocksExpected {
		return nil, fmt.Errorf("days count %d blocks for %d heights", infl.BlocksCounted, infl.BlocksExpected)
	}

	type entry struct {
		day  int
		r, c *big.Int
	}
	byOp := map[string][]entry{}
	for _, v := range vals {
		i, ok := index[v.Day]
		if !ok {
			return nil, fmt.Errorf("validator row on %s outside the days", v.Day)
		}
		r, err := ParseFix18(v.Rewards)
		if err != nil {
			return nil, fmt.Errorf("validator row %s: rewards: %v", v.Day, err)
		}
		c, err := ParseFix18(v.Commission)
		if err != nil {
			return nil, fmt.Errorf("validator row %s: commission: %v", v.Day, err)
		}
		es := byOp[v.Operator]
		if n := len(es); n > 0 && es[n-1].day >= i {
			return nil, fmt.Errorf("validator rows out of day order on %s", v.Day)
		}
		byOp[v.Operator] = append(es, entry{i, r, c})
	}
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	f := incomeFile{Chain: st.Chain, AsOf: asOf, FirstDay: order[0], LastDay: order[len(order)-1],
		FirstHeight: infl.FirstHeight, LastHeight: infl.LastHeight, BlocksCounted: infl.BlocksCounted,
		BlocksExpected: infl.BlocksExpected, Withdrawn: withdrawn, ExchangeRun: []int{}}
	idx := make(map[string]int, len(ops))
	for _, op := range ops {
		es := byOp[op]
		first, last := es[0].day, es[len(es)-1].day
		daily := make([]int64, 0, last-first+1)
		monthly := map[string]*big.Int{}
		cum, prev := new(big.Int), new(big.Int)
		k := 0
		for d := first; d <= last; d++ {
			if k < len(es) && es[k].day == d {
				cum.Add(cum, es[k].c)
				m := order[d][:7]
				if monthly[m] == nil {
					monthly[m] = new(big.Int)
				}
				monthly[m].Add(monthly[m], es[k].r)
				k++
			}
			fl := Fix18Floor(cum)
			step := new(big.Int).Sub(fl, prev)
			if !step.IsInt64() {
				return nil, fmt.Errorf("validator commission out of range on %s", order[d])
			}
			daily = append(daily, step.Int64())
			prev = fl
		}
		rewards := make(map[string]int64, len(monthly))
		for m, v := range monthly {
			fl := Fix18Floor(v)
			if !fl.IsInt64() {
				return nil, fmt.Errorf("validator rewards out of range in %s", m)
			}
			rewards[m] = fl.Int64()
		}
		idx[op] = len(f.Validators)
		f.Validators = append(f.Validators, incomeFileValidator{Operator: op, FirstDay: order[first], Commission: daily, Rewards: rewards})
	}
	if embInc != nil {
		f.ExchangeRunRule = embInc.File.ExchangeRunRule
		for _, i := range embInc.File.ExchangeRun {
			if j, ok := idx[embInc.File.Validators[i].Operator]; ok {
				f.ExchangeRun = append(f.ExchangeRun, j)
			}
		}
		sort.Ints(f.ExchangeRun)
	}
	inc, err := buildIncome(f)
	if err != nil {
		return nil, err
	}
	return &stakingSet{infl: infl, inc: inc}, nil
}
