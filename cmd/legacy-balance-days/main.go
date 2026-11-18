// Command legacy-balance-days builds legacy.balance_days for a legacy chain
// from its genesis file and stored history, and compares the state it
// reaches at the export height with the next chain's genesis file.
//
// Without --record it only reports. With --record it replaces the chain's
// rows in legacy.balance_days and its delegation and unbonding lines in
// legacy.reconciliation in one transaction, and only when every gate passed.
// It reads and writes the legacy schema through a connection confined to it.
// Output is counts and totals only: no address is printed.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"github.com/DefiantLabs/cosmos-indexer/legacy/balancedays"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fail(text string) {
	fmt.Fprintln(os.Stderr, text)
	os.Exit(2)
}

func open() (*sql.DB, error) {
	db, err := gorm.Open(postgres.Open(os.Getenv("LEGACY_IMPORT_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	findings, err := schema.Confined(db)
	if err != nil {
		return nil, fmt.Errorf("catalog not readable")
	}
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Fprintln(os.Stderr, finding)
		}
		return nil, fmt.Errorf("refusing a connection that is not confined to the legacy schema")
	}
	return db.DB()
}

func gate(name string, g balancedays.Gate) {
	fmt.Printf("%s: pairs %d, exact %d, within share rounding %d, within slash bound %d, outside %d (uatom %s); pairs with a bound %d, largest bound %s uatom\n",
		name, g.Pairs, g.Exact, g.Rounding, g.WithinBound, g.Outside, g.OutsideAmount, g.PairsBounded, g.MaxBound)
}

func run(ctx context.Context, db *sql.DB, chain string, record bool) error {
	started := time.Now()
	var tx *sql.Tx
	var pending []balancedays.Row
	sink := func(balancedays.Row) error { return nil }
	if record {
		var err error
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM legacy.balance_days WHERE chain_id = $1`, chain); err != nil {
			return err
		}
		sink = func(row balancedays.Row) error {
			pending = append(pending, row)
			if len(pending) < 20000 {
				return nil
			}
			err := balancedays.Append(ctx, tx, chain, pending)
			pending = pending[:0]
			return err
		}
	}
	result, err := balancedays.Build(ctx, db, chain, sink)
	if err != nil {
		return err
	}
	fmt.Printf("balance days chain=%s against=%s through_height=%d days=%d days_with_rows=%d seconds=%.0f\n",
		result.Chain, result.Next, result.Through, result.Days, result.DaysStored, time.Since(started).Seconds())
	if result.AddedMoves > 0 {
		fmt.Printf("movements the stored rows do not hold, replayed from the movement file %d\n", result.AddedMoves)
	}
	fmt.Printf("rows %d for %d addresses; stored rows applied %d, charged fees applied %d, rows and fees in another denomination %d, genesis change rows left out %d, module accounts left out %d\n",
		result.Rows, result.Wallets, result.EventRows, result.Fees, result.OtherDenomRows, result.ChangeRows, result.ModuleAccounts)
	fmt.Printf("spendable: addresses %d, exact %d, governance accounts the next chain did not carry %d, recorded genesis change %d, open %d\n",
		result.LiquidCompared, result.LiquidExact, result.LiquidDeposit, result.LiquidChange, result.LiquidOpen)
	fmt.Printf("spendable lines already in legacy.reconciliation %d, equal to this build %d\n", result.StoredLiquidLines, result.StoredLiquidEqual)
	fmt.Printf("validators whose share price moved %d, validators the next genesis does not hold %d, of those with a price estimated from lost tokens %d, jailed with an unchanged price %d\n",
		result.ValidatorsPriceMoved, result.ValidatorsRemoved, result.ValidatorsRemovedSlashed, result.ValidatorsJailedSamePrice)
	fmt.Printf("delegations bounded back from the next genesis %d: movements narrowed %d, kept within rounding %d, not explained %d\n",
		result.AnchoredPairs, result.AnchorNarrowed, result.AnchorRounding, result.AnchorConflicts)
	gate("delegations", result.Delegations)
	gate("unbonding", result.Unbonding)
	fmt.Printf("completed unbondings: due entries %d (of those within share rounding %d), paid in the same block %d, paid by the undelegation itself %d, other entries %d, paid less after a slash %d (uatom %s), not matched %d\n",
		result.CompletesExact, result.CompletesRounding, result.CompletesSameBlock, result.CompletesInstant, result.CompletesEarly, result.CompletesSlashed, result.CompletesSlashedAmount, result.CompletesUnmatched)
	if result.ExactReplay {
		fmt.Printf("exact replay: slashes applied %d, unbonding entries slashed %d, redelegations slashed %d, validators equal to the next genesis %d, differing %d, delegation shares equal %d, differing %d, movements the chain would refuse %d\n",
			result.SlashesApplied, result.SlashedEntries, result.SlashedRedelegations, result.ValidatorsExact, result.ValidatorsDiffer,
			result.SharesExact, result.SharesDiffer, result.ExactAnomalies)
	}
	fmt.Printf("day ends below zero %d\n", result.NegativeDayEnds)
	statuses := map[string]int{}
	for _, line := range result.Lines {
		statuses[line.Component+" "+line.Status]++
	}
	keys := make([]string, 0, len(statuses))
	for k := range statuses {
		keys = append(keys, k)
	}
	fmt.Printf("reconciliation lines by status: %s\n", strings.Join(sorted(keys, statuses), ", "))
	if !result.Passed() {
		return fmt.Errorf("the history does not explain the next genesis file; nothing written")
	}
	fmt.Println("gates passed")
	if !record {
		return nil
	}
	if len(pending) > 0 {
		if err := balancedays.Append(ctx, tx, chain, pending); err != nil {
			return err
		}
	}
	if err := balancedays.Record(ctx, tx, result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("recorded %d rows in legacy.balance_days and %d lines in legacy.reconciliation\n", result.Rows, len(result.Lines))
	return nil
}

func readShares(data []byte) error {
	for n, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			return fmt.Errorf("shares file line %d has fewer than six fields", n+1)
		}
		var key [3]int64
		for i := range key {
			v, err := strconv.ParseInt(f[1+i], 10, 64)
			if err != nil {
				return fmt.Errorf("shares file line %d is not readable", n+1)
			}
			key[i] = v
		}
		// A decimal names shares (Hub 1 messages), a whole number names
		// tokens (Hub 2 messages).
		named := balancedays.MessageShares
		if !strings.Contains(f[5], ".") {
			named = balancedays.MessageTokens
		}
		if named[f[0]] == nil {
			named[f[0]] = map[[3]int64]string{}
		}
		named[f[0]][key] = f[5]
	}
	return nil
}

// readMoves reads movements the stored rows do not hold. Each line is chain,
// height, transaction, message, kind (undelegate or redelegate), and the
// sha256 in hex of the delegator, the validator and the destination validator
// (redelegations), then the tokens the message named and the completion
// time, tab separated.
func readMoves(data []byte) error {
	for n, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 10 {
			return fmt.Errorf("movement file line %d does not have ten fields", n+1)
		}
		height, err1 := strconv.ParseInt(f[1], 10, 64)
		tx, err2 := strconv.Atoi(f[2])
		msg, err3 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil || err3 != nil || (f[4] != "undelegate" && f[4] != "redelegate") || len(f[5]) != 64 || len(f[6]) != 64 || (f[4] == "redelegate" && len(f[7]) != 64) {
			return fmt.Errorf("movement file line %d is not readable", n+1)
		}
		if _, err := strconv.ParseUint(f[8], 10, 64); err != nil {
			return fmt.Errorf("movement file line %d names no whole token amount", n+1)
		}
		balancedays.ExtraMoves[f[0]] = append(balancedays.ExtraMoves[f[0]], balancedays.MoveRecord{
			Height: height, Tx: tx, Msg: msg, Kind: f[4], Delegator: f[5], Validator: f[6], Destination: f[7], Tokens: f[8], Completion: f[9],
		})
		if balancedays.MessageTokens[f[0]] == nil {
			balancedays.MessageTokens[f[0]] = map[[3]int64]string{}
		}
		balancedays.MessageTokens[f[0]][[3]int64{height, int64(tx), int64(msg)}] = f[8]
	}
	return nil
}

// The slashes the stored block events of cosmoshub-1 and cosmoshub-2 do not
// hold, the amounts their undelegation and redelegation messages named, and
// the movements their stored rows do not hold, read from each chain's block
// results and blocks. A flag naming a file replaces the embedded copy.
var (
	//go:embed data/slashes.tsv
	embeddedSlashes []byte
	//go:embed data/msgamounts.tsv
	embeddedShares []byte
	//go:embed data/moves.tsv
	embeddedMoves []byte
)

func readSlashes(data []byte) error {
	for n, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return fmt.Errorf("slash file line %d does not have six fields", n+1)
		}
		height, err1 := strconv.ParseInt(f[1], 10, 64)
		power, err2 := strconv.ParseInt(f[3], 10, 64)
		infraction, err3 := strconv.ParseInt(f[5], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || (f[4] != "downtime" && f[4] != "double") {
			return fmt.Errorf("slash file line %d is not readable", n+1)
		}
		balancedays.ExtraSlashes[f[0]] = append(balancedays.ExtraSlashes[f[0]], balancedays.SlashRecord{
			Height: height, Operator: f[2], Power: power, Double: f[4] == "double", Infraction: infraction,
		})
	}
	return nil
}

func sorted(keys []string, counts map[string]int) []string {
	out := make([]string, 0, len(keys))
	for len(keys) > 0 {
		best := 0
		for i := range keys {
			if keys[i] < keys[best] {
				best = i
			}
		}
		out = append(out, fmt.Sprintf("%s %d", keys[best], counts[keys[best]]))
		keys = append(keys[:best], keys[best+1:]...)
	}
	return out
}

func main() {
	chains := flag.String("chains", "cosmoshub-1,cosmoshub-2,cosmoshub-3", "legacy chains to build, in order")
	record := flag.Bool("record", false, "replace each chain's rows and lines when its gates pass")
	trace := flag.Bool("trace", false, "print each pair that does not close exactly, addresses shortened to a hash")
	exactChains := flag.String("exact", "", "further chains to replay exactly, comma separated")
	slashFile := flag.String("slashes", "", "file of added slashes, in place of the embedded one: chain, height, validator, power, kind (downtime or double), infraction height (0 for the downtime rule), tab separated")
	sharesFile := flag.String("shares", "", "file of the amounts undelegation and redelegation messages named, in place of the embedded one: chain, height, transaction, message, kind, shares (with a decimal point) or tokens, tab separated")
	movesFile := flag.String("moves", "", "file of undelegations and redelegations the chain ran that the stored rows hold no row for (see readMoves), in place of the embedded one")
	checkpoints := flag.String("checkpoints", "", "write every validator's tokens and shares at each multiple of 10000 to this file (exact replays only)")
	flag.Parse()
	for _, chain := range strings.Split(*exactChains, ",") {
		if chain = strings.TrimSpace(chain); chain != "" {
			balancedays.ForceExact[chain] = true
		}
	}
	for _, in := range []struct {
		path     string
		embedded []byte
		read     func([]byte) error
	}{{*slashFile, embeddedSlashes, readSlashes}, {*sharesFile, embeddedShares, readShares}, {*movesFile, embeddedMoves, readMoves}} {
		data := in.embedded
		if in.path != "" {
			var err error
			if data, err = os.ReadFile(in.path); err != nil {
				fail("an input file is not readable")
			}
		}
		if err := in.read(data); err != nil {
			fail(err.Error())
		}
	}
	if *checkpoints != "" {
		f, err := os.Create(*checkpoints)
		if err != nil {
			fail("checkpoint file not writable")
		}
		// Written unbuffered: a chain that is not built exits the program.
		// Operators are written as the sha256 of the address, in hex.
		balancedays.Checkpoint = func(height int64, operator, tokens, shares string) {
			sum := sha256.Sum256([]byte(operator))
			fmt.Fprintf(f, "%d\t%s\t%s\t%s\n", height, hex.EncodeToString(sum[:]), tokens, shares)
		}
	}
	if *trace {
		balancedays.Trace = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	ctx := context.Background()
	db, err := open()
	if err != nil {
		fail(err.Error())
	}
	failed := false
	for _, chain := range strings.Split(*chains, ",") {
		if err := run(ctx, db, strings.TrimSpace(chain), *record); err != nil {
			fmt.Printf("chain %s not built: %s\n", chain, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
