// Command legacy-close checks that the stored history of a legacy chain
// explains the balances the next chain started with: for every address, the
// spendable balance in the chain's genesis file plus every stored movement
// and charged fee up to the export height, against the spendable balance in
// the next chain's genesis file.
//
// It reads the legacy schema through a connection confined to it. With
// --record it replaces the chain's lines in legacy.reconciliation. Output is
// counts and totals only: no address is printed.
//
// --genesis-change names a copy of the public source file with which the
// next genesis rewrote balances in code (for cosmoshub-3, the Proposal 29
// fund recovery). Only the file with the recorded sha256 is accepted. The
// wallets it lists are counted on their own line, never as closing exactly,
// and with --record the tax rows of the change are stored once.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/DefiantLabs/cosmos-indexer/legacy/books"
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

func table(title string, counts map[string]int, limit int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if counts[keys[a]] != counts[keys[b]] {
			return counts[keys[a]] > counts[keys[b]]
		}
		return keys[a] < keys[b]
	})
	fmt.Println(title + ":")
	for i, k := range keys {
		if limit > 0 && i == limit {
			rest := 0
			for _, other := range keys[i:] {
				rest += counts[other]
			}
			fmt.Printf("  %8d  in %d more groups\n", rest, len(keys)-i)
			break
		}
		fmt.Printf("  %8d  %s\n", counts[k], k)
	}
}

func main() {
	chain := flag.String("chain", "cosmoshub-3", "legacy chain to close against the genesis file of the chain after it")
	record := flag.Bool("record", false, "replace the chain's lines in legacy.reconciliation with this result")
	groups := flag.Int("groups", 40, "largest number of cause groups printed")
	changeFile := flag.String("genesis-change", "", "copy of the public source file of the balances the next genesis changed in code")
	flag.Parse()
	ctx := context.Background()
	var change *books.GenesisChange
	if *changeFile != "" {
		source, err := os.ReadFile(*changeFile)
		if err != nil {
			fail("close not run: the genesis change file is not readable")
		}
		if change, err = books.LoadProp29(source); err != nil {
			fail("close not run: " + err.Error())
		}
	}
	db, err := open()
	if err != nil {
		fail(err.Error())
	}
	result, err := books.CloseWith(ctx, db, *chain, change)
	if err != nil {
		fail("close not run: " + err.Error())
	}
	fmt.Printf("close chain=%s against=%s through_height=%d heights_stored=%d complete=%t\n",
		result.Chain, result.Next, result.Through, result.HeightsStored, result.Complete)
	if !result.Complete {
		fmt.Println("not every height up to the export height is stored, so an address that does not close may only be missing history")
	}
	fmt.Printf("stored rows applied %d, charged fees applied %d, rows and fees in another denomination %d\n", result.Rows, result.Fees, result.OtherDenomRows)
	fmt.Printf("module accounts left out %d\n", result.ModuleAccounts)
	fmt.Printf("addresses compared %d\n", result.Addresses)
	fmt.Printf("addresses that close exactly %d\n", result.Closed)
	if change != nil {
		listed := change.Listed()
		fmt.Printf("addresses that close with the %s %d\n", change.Name, result.ClosedWithChange)
		fmt.Printf("%s: source wallets that pass their test %d of %d, recovery wallets %d of %d\n",
			change.Name, result.ChangeSources, listed, result.ChangeRecoveries, listed)
		fmt.Printf("%s: uatom out of the source wallets %s, into the recovery wallets %s, left to the community pool %s\n",
			change.Name, result.ChangeOut, result.ChangeIn, result.ChangePool)
		fmt.Printf("%s: community pool change recorded from the chain state %s, equal to what the list leaves %t\n",
			change.Name, change.Pool(), result.ChangePoolExact)
		fmt.Printf("%s: list read from %s %s %s sha256 %s\n", change.Name, change.Repo, change.Tag, change.Path, change.SHA256)
		fmt.Printf("%s: pool evidence, fee pool answer after the export height sha256 %s, next genesis file sha256 %s\n",
			change.Name, change.PoolStateSHA256, change.NextGenesisSHA256)
		if !result.ChangeWhole {
			fmt.Printf("%s: list refused as a whole; every listed wallet is counted as a close without the list counts it\n", change.Name)
		}
	}
	if result.ChangeRows != 0 {
		fmt.Printf("stored rows of the genesis change %d (they move no balance in this check)\n", result.ChangeRows)
	}
	fmt.Printf("addresses that do not close %d\n", result.Open)
	fmt.Printf("uatom the next genesis holds above the history %s, below it %s\n", result.Short, result.Over)
	table("addresses that do not close, by size of the difference", result.OpenBySize, 0)
	rates := map[string]int{}
	for kind, all := range result.Touched {
		rates[fmt.Sprintf("%s: %d of %d addresses", kind, result.TouchedOpen[kind], all)] = result.TouchedOpen[kind]
	}
	table("addresses that do not close, by what touched the address", rates, 0)
	table("addresses that do not close, by side and everything that touched the address", result.OpenByCause, *groups)
	if *record {
		if err := books.Record(ctx, db, result); err != nil {
			fail("not recorded: " + err.Error())
		}
		fmt.Printf("recorded %d lines in legacy.reconciliation\n", len(result.Lines))
		switch {
		case change == nil:
		case !result.ChangeWhole:
			fmt.Println("rows of the genesis change not recorded: the close refused the list")
		default:
			added, err := books.RecordChange(ctx, db, result)
			if err != nil {
				fail("genesis change not recorded: " + err.Error())
			}
			if added == 0 {
				fmt.Println("rows of the genesis change already stored and equal to this close")
			} else {
				fmt.Printf("recorded %d rows of the genesis change in legacy.taxable_events\n", added)
			}
		}
	}
	if !result.Complete || result.Open != 0 || (change != nil && !result.ChangeWhole) {
		os.Exit(1)
	}
}
