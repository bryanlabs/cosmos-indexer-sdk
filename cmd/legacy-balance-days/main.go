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
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

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
	flag.Parse()
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
