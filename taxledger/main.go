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
// With -commission-flows it traces every validator's withdrawn commission
// from -commission-flows-from to today and stores where it went (OPS-818).
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, and NODE_REST_API for
// -days-from, -days-append and -commission-flows (which also reads
// ORACLE_URL for denoms).
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"github.com/spf13/viper"
	"gorm.io/gorm"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func commissionRuntimeParams(flows bool, version string) []string {
	if !flows {
		return nil
	}
	params := []string{"jit=off"}
	if version != "" {
		// Private full-history bitmap reads benefit from page prefetch. This
		// is a connection startup parameter, never a global database change.
		params = append(params, "effective_io_concurrency=200")
	}
	return params
}

func validateCommissionReadTimeout(flows bool, version string, timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("commission read snapshot timeout must not be negative")
	}
	if timeout != 0 && (!flows || strings.TrimSpace(version) == "") {
		return fmt.Errorf("commission read snapshot timeout requires private versioned commission collection")
	}
	return nil
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
	flows := flag.Bool("commission-flows", false, "trace where each validator's commission went")
	flowsFrom := flag.String("commission-flows-from", "2023-01-01", "first day of the commission trace")
	flowsOnly := flag.String("commission-validators", "", "comma-separated valoper addresses to trace, default every validator")
	flowsWorkers := flag.Int("commission-workers", 4, "validators traced in parallel")
	flowsVersion := flag.String("commission-output-version", "", "required immutable output version, leaves existing serving rows untouched")
	flowsBuild := flag.String("commission-build", "", "required image digest or source build for this output version")
	flowsAsOf := flag.String("commission-as-of", "", "required exclusive end date (YYYY-MM-DD UTC) for this output version")
	flowsArchive := flag.String("commission-archive-config", "", "optional Secret-mounted probe.toml containing exact historical archive RPC")
	flowsReadTimeout := flag.Duration("commission-read-snapshot-timeout", 0, "opt-in private versioned read snapshot deadline (0 keeps 30s; does not change public API reads)")
	recheck := flag.Bool("recheck-holes", false, "read every recorded index hole's block again and drop the holes that are no longer short")
	flag.Parse()
	if err := validateCommissionReadTimeout(*flows, *flowsVersion, *flowsReadTimeout); err != nil {
		log.Fatal(err)
	}

	// The commission trace runs thousands of small per-wallet queries whose
	// plans cross jit_above_cost; JIT compiling each took about 1 s for 40 ms
	// of work. Session-only, so other jobs and the API keep their settings.
	runtime := commissionRuntimeParams(*flows, *flowsVersion)
	db, err := indexerDB.PostgresDbConnect(
		env("DB_HOST", "localhost"),
		env("DB_PORT", "5432"),
		env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"),
		env("DB_PASS", "postgres"),
		env("DB_LOG_LEVEL", "warn"),
		runtime...,
	)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if *flows {
		commissionFlows(db, *chain, *flowsFrom, *flowsOnly, *flowsWorkers, *flowsVersion, *flowsBuild, *flowsAsOf, *flowsArchive, *flowsReadTimeout)
		return
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
	if *recheck {
		if _, _, err := srv.RecheckIndexHoles(*chain, node, log.Printf); err != nil {
			log.Fatalf("index holes: %v", err)
		}
		return
	}
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

func commissionFlows(db *gorm.DB, chain, fromText, only string, workers int, version, build, asOfText, archiveConfig string, readTimeout time.Duration) {
	if strings.TrimSpace(version) == "" || strings.TrimSpace(build) == "" || asOfText == "" {
		log.Fatal("commission flows require --commission-output-version, --commission-build and --commission-as-of; replacement of prior output is disabled")
	}
	asOf, err := time.Parse(time.DateOnly, asOfText)
	if err != nil {
		log.Fatalf("-commission-as-of: %v", err)
	}
	from, err := time.Parse(time.DateOnly, fromText)
	if err != nil {
		log.Fatalf("-commission-flows-from: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	oracle := taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API"))
	srv := taxapi.NewServer(db, oracle, taxapi.DefaultNativeAsset)
	var validators []string
	for _, v := range strings.Split(only, ",") {
		if v = strings.TrimSpace(v); v != "" {
			validators = append(validators, v)
		}
	}
	archiveRPC := ""
	if archiveConfig != "" {
		config := viper.New()
		config.SetConfigFile(archiveConfig)
		if err := config.ReadInConfig(); err != nil {
			log.Fatal("cannot read commission archive configuration")
		}
		archiveRPC = config.GetString("probe.rpc")
		if archiveRPC == "" {
			log.Fatal("commission archive configuration has no RPC endpoint")
		}
	}
	if err := srv.BuildCommissionFlows(chain, node, from, validators, workers, log.Printf, taxapi.CommissionFlowBuildOptions{Version: version, Build: build, AsOf: asOf, ArchiveRPC: archiveRPC, ReadSnapshotTimeout: readTimeout}); err != nil {
		log.Fatalf("commission flows: %v", err)
	}
}
