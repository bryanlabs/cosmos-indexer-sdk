// taxcoverage keeps the coverage rollup (taxapi/coveragerollup.go) current:
// message counts by UTC day, message type and id range, with how many the tax
// layer classified, and transaction counts by day. GET /coverage sums it
// instead of counting every message in the period. The API never writes it;
// this Job does.
//
//	-refresh  build to the indexed frontier, rebuild changed chunks and the oldest ones
//	-check N  recount N sampled chunks live and compare (0 = every chunk)
//	-status   print the one-line status only
//
// The tables are created once by the database owner (CoverageRollupDDL). Every
// run ends with the status line. The exit code is non-zero when the action
// failed or a check found a difference; a run that finds another holding the
// lock exits zero.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	os.Exit(run())
}

func run() int {
	refresh := flag.Bool("refresh", false, "build to the frontier and rebuild changed chunks")
	check := flag.Int("check", -1, "recount this many sampled chunks live (0 = every chunk)")
	seed := flag.Int64("seed", time.Now().UnixNano(), "sample seed for -check")
	status := flag.Bool("status", false, "print the status line only")
	budget := flag.Duration("budget", 4*time.Minute, "stop a refresh after this long with its progress kept (0 = until current)")
	rolling := flag.Int("rolling", 1, "oldest chunks per source rebuilt each refresh")
	fingerprint := flag.Bool("fingerprint", false, "start a fingerprint pass now")
	flag.Parse()
	if !*refresh && *check < 0 && !*status {
		flag.Usage()
		return 2
	}
	db, err := indexerDB.PostgresDbConnect(
		env("DB_HOST", "localhost"),
		env("DB_PORT", "5432"),
		env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"),
		env("DB_PASS", "postgres"),
		env("DB_LOG_LEVEL", "warn"),
		"jit=off",
	)
	if err != nil {
		log.Printf("db connect: %v", err)
		return 1
	}
	pool, err := db.DB()
	if err != nil {
		log.Printf("db pool: %v", err)
		return 1
	}
	// One connection: the run's advisory lock is held by its session.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	defer func() { _ = pool.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := false
	began := time.Now()
	if *refresh {
		res, err := taxapi.RefreshCoverageRollup(ctx, db, taxapi.CoverageRollupOptions{
			Budget: *budget, Rolling: *rolling, ForceFingerprint: *fingerprint, Logf: log.Printf,
		})
		switch {
		case errors.Is(err, taxapi.ErrCoverageRollupBusy):
			log.Printf("coverage-rollup refresh: another run holds the lock")
		case err != nil:
			failed = true
			log.Printf("coverage-rollup refresh FAILED: %v", err)
		default:
			log.Printf("coverage-rollup refresh: built %d, dirtied %d, rebuilt %d (rolled %d), fingerprinted %d, current=%t, %s",
				res.Built, res.Dirtied, res.Rebuilt, res.Rolled, res.Fingerprinted, res.Current, time.Since(began).Round(time.Millisecond))
		}
	}
	if !failed && *check >= 0 {
		diffs, err := taxapi.CheckCoverageRollup(ctx, db, *check, *seed, log.Printf)
		switch {
		case err != nil:
			failed = true
			log.Printf("coverage-rollup check FAILED: %v", err)
		case len(diffs) > 0:
			failed = true
			log.Printf("coverage-rollup check: %d chunk(s) differ: %v", len(diffs), diffs)
		}
	}
	line, err := taxapi.CoverageRollupStatus(context.Background(), db)
	if err != nil {
		log.Printf("coverage-rollup status FAILED: %v", err)
		return 1
	}
	log.Print(line)
	if failed {
		return 1
	}
	return 0
}
