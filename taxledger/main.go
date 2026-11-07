// taxledger extends the past-date balance ledger to heights the serving node
// no longer holds: it scans blocks from the archive block cache back from the
// start of the scanned span, then records validator rates for those days from
// an archive node's REST API. Both steps resume where the last run stopped.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS.
package main

import (
	"errors"
	"flag"
	"log"
	"os"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
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
