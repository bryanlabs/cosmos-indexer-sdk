// taxstatement installs, fills, refreshes and checks the validator statement
// summary (taxapi/statementsummary.go): per-day sums and per-month reward
// recipients for every finished month, which the API reads instead of tens of
// millions of taxable_events rows. The API never writes it; this Job does.
//
//	-install  create the tables and attach the change triggers to taxable_events
//	-fill     build every finished month (add -rebuild to start over)
//	-refresh  recompute the months the change log names, then build a month that has ended
//	-check N  compare N sampled (validator, month) cells with taxable_events; 0 compares all
//	-status   print the one-line status only
//	-drop     remove the triggers and the tables; the API reads live rows again
//
// Every run ends with the status line. The exit code is non-zero when the
// action failed, when a check found a difference, and when -require-trusted
// is set and the summary is not in use.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS.
package main

import (
	"context"
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
	install := flag.Bool("install", false, "create the summary tables and attach the change triggers")
	fill := flag.Bool("fill", false, "build every finished month")
	rebuild := flag.Bool("rebuild", false, "with -fill: empty a built summary and build it again")
	refresh := flag.Bool("refresh", false, "recompute changed months and build a month that has ended")
	check := flag.Int("check", -1, "compare this many sampled cells with taxable_events (0 = every cell)")
	seed := flag.Int64("seed", time.Now().UnixNano(), "sample seed for -check")
	status := flag.Bool("status", false, "print the status line only")
	drop := flag.Bool("drop", false, "remove the triggers and the summary tables")
	requireTrusted := flag.Bool("require-trusted", false, "exit non-zero when the summary is not in use")
	lockTimeout := flag.Duration("lock-timeout", 3*time.Second, "longest wait for the lock on taxable_events per attempt (-install, -drop)")
	attempts := flag.Int("attempts", 20, "attempts to get that lock (-install)")
	drain := flag.Duration("drain-timeout", 20*time.Minute, "longest wait for open transactions before a month is built")
	asOf := flag.String("as-of", "", "build only the months before this day (YYYY-MM-DD, not in the future; for tests)")
	flag.Parse()
	if !*install && !*fill && !*refresh && *check < 0 && !*status && !*drop {
		flag.Usage()
		os.Exit(2)
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
		log.Fatalf("db connect: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := taxapi.StatementSummaryOptions{DrainTimeout: *drain, Logf: log.Printf}
	if *asOf != "" {
		day, err := time.Parse("2006-01-02", *asOf)
		if err != nil || day.After(time.Now()) {
			log.Fatalf("-as-of must be a past day as YYYY-MM-DD, got %q", *asOf)
		}
		opts.Now = func() time.Time { return day }
	}

	failed := false
	fail := func(what string, err error) {
		failed = true
		log.Printf("statement-summary %s FAILED: %v", what, err)
	}
	began := time.Now()
	switch {
	case *drop:
		if err := taxapi.DropStatementSummary(ctx, db, *lockTimeout); err != nil {
			fail("drop", err)
		} else {
			log.Printf("statement-summary drop: triggers and tables removed")
		}
	case *install:
		if err := taxapi.InstallStatementSummary(ctx, db, *lockTimeout, *attempts, log.Printf); err != nil {
			fail("install", err)
		} else {
			log.Printf("statement-summary install: tables and triggers in place in %s", time.Since(began).Round(time.Millisecond))
		}
	}
	if !failed && *fill {
		res, err := taxapi.FillStatementSummary(ctx, db, *rebuild, opts)
		if err != nil {
			fail("fill", err)
		} else {
			log.Printf("statement-summary fill: %d validators, %d day rows, %d delegator months, built before %s, %s",
				res.Validators, res.DayRows, res.Pairs, res.BuiltBefore.Format("2006-01-02"), time.Since(began).Round(time.Second))
		}
	}
	if !failed && *refresh {
		res, err := taxapi.RefreshStatementSummary(ctx, db, opts)
		if err != nil {
			fail("refresh", err)
		} else {
			log.Printf("statement-summary refresh: %d changed month(s) of %d validator(s) recomputed, advanced=%t, built before %s, %s",
				res.Cells, res.Validators, res.Advanced, res.BuiltBefore.Format("2006-01-02"), time.Since(began).Round(time.Millisecond))
		}
	}
	if !failed && *check >= 0 {
		res, err := taxapi.CheckStatementSummary(ctx, db, *check, *seed, opts)
		switch {
		case err != nil:
			fail("check", err)
		case len(res.Mismatches) > 0:
			failed = true
			log.Printf("statement-summary check FAILED: %d of %d cells differ from taxable_events (seed %d)", len(res.Mismatches), res.Checked, *seed)
		default:
			log.Printf("statement-summary check: %d cells equal taxable_events (seed %d), %s", res.Checked, *seed, time.Since(began).Round(time.Millisecond))
		}
	}
	st, err := taxapi.ReadStatementSummaryStatus(context.Background(), db)
	if err != nil {
		fail("status", err)
	} else {
		log.Print(st.Line())
		if *requireTrusted && !st.Trusted {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
