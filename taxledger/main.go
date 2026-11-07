// taxledger extends the past-date balance ledger to heights the serving node
// no longer holds: it scans blocks from the archive block cache back from the
// start of the scanned span, then records validator rates for those days from
// an archive node's REST API. Both steps resume where the last run stopped.
//
// With -days-from it instead stores every address's holdings at the end of
// each day from -days-from to -days-to, walking back from the serving node
// (NODE_REST_API) the way the API does. It resumes an interrupted build of
// the same span.
//
// With -days-append it extends a finished build with the days after it that
// the index, the ledger scan and the validator rates cover, up to yesterday,
// rebuilding only the wallets those days can have changed.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, and NODE_REST_API for
// -days-from and -days-append.
package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"gorm.io/gorm"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	chain := flag.String("chain", "mainnet", "chain the ledger scan belongs to")
	cache := flag.String("cache", "/cache", "archive block cache directory for this chain")
	archive := flag.String("archive-rest", os.Getenv("ARCHIVE_REST"), "archive node REST origin for validator state")
	floor := flag.Int64("floor", 0, "lowest height to scan back to")
	chunk := flag.Int64("chunk", 5000, "blocks recorded per database transaction")
	workers := flag.Int("workers", 16, "blocks read in parallel")
	rates := flag.Bool("rates", true, "record validator rates for the scanned days afterwards")
	daysFrom := flag.String("days-from", "", "first day (YYYY-MM-DD) of a stored balance build")
	daysTo := flag.String("days-to", "", "last day of the build, default yesterday (UTC)")
	daysBatch := flag.Int("days-batch", 2000, "addresses read per batch of the build")
	daysWorkers := flag.Int("days-workers", 8, "addresses built in parallel")
	daysAppend := flag.Bool("days-append", false, "append the finished days after the stored balance build")
	flag.Parse()

	db, err := indexerDB.PostgresDbConnect(
		env("DB_HOST", "localhost"),
		env("DB_PORT", "5432"),
		env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"),
		env("DB_PASS", "postgres"),
		env("DB_LOG_LEVEL", "warn"),
	)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if *daysAppend {
		appendDays(db, *chain, *daysWorkers)
		return
	}
	if *daysFrom != "" {
		buildDays(db, *chain, *daysFrom, *daysTo, *daysBatch, *daysWorkers)
		return
	}
	node, err := taxapi.NewArchiveLedgerNode(*cache, *archive)
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	from, err := srv.ExtendLedgerScanBack(*chain, node, *floor, *chunk, *workers, log.Printf)
	switch {
	case errors.Is(err, taxapi.ErrLedgerScanUnindexed):
		log.Printf("ledger scan back: stopped at the span start %d: %v", from, err)
	case err != nil:
		log.Fatalf("ledger scan back: stopped at the span start %d: %v", from, err)
	default:
		log.Printf("ledger scan back: span starts at the floor %d", from)
	}
	if *rates {
		if err := srv.BackfillValidatorRates(*chain, node, log.Printf); err != nil {
			log.Fatalf("validator rates: %v", err)
		}
		log.Printf("validator rates: recorded for every scanned day")
	}
}

func buildDays(db *gorm.DB, chain, fromText, toText string, batch, workers int) {
	from, err := time.Parse(time.DateOnly, fromText)
	if err != nil {
		log.Fatalf("-days-from: %v", err)
	}
	to := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if toText != "" {
		if to, err = time.Parse(time.DateOnly, toText); err != nil {
			log.Fatalf("-days-to: %v", err)
		}
	}
	if err := taxapi.BalanceHistoryMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	if err := srv.BuildBalanceDays(chain, "cosmos", node, from, to, batch, workers, log.Printf); err != nil {
		log.Fatalf("balance days: %v", err)
	}
}

func appendDays(db *gorm.DB, chain string, workers int) {
	if err := taxapi.BalanceHistoryMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	through := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if err := srv.AppendBalanceDays(chain, "cosmos", node, through, workers, log.Printf); err != nil {
		log.Fatalf("balance days: %v", err)
	}
}
