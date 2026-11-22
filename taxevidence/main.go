// taxevidence installs, repairs, trims or removes the report evidence journal
// (taxapi/evidencejournal.go), which lets a repeat view of a cached report
// skip rereading the wallet's whole history. The API only reads it.
//
//	-install  create the journal and attach its triggers (also repairs it and
//	          restores trust after a write with foreign keys off)
//	-print    print the installation SQL for -schema instead of running it
//	-prune    trim journal rows past the report age limit
//	-drop     remove the triggers and the journal; every view runs the full check
//
// Run it as the owner of the indexer tables. Env: DB_HOST DB_PORT DB_NAME
// DB_USER DB_PASS.
package main

import (
	"context"
	"flag"
	"fmt"
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
	install := flag.Bool("install", false, "create or repair the journal")
	printSQL := flag.Bool("print", false, "print the installation SQL")
	schema := flag.String("schema", "public", "schema of the indexer tables, for -print")
	prune := flag.Bool("prune", false, "trim rows past the report age limit")
	drop := flag.Bool("drop", false, "remove the triggers and the journal")
	lockTimeout := flag.Duration("lock-timeout", 3*time.Second, "longest wait for each table lock per attempt")
	attempts := flag.Int("attempts", 20, "attempts to get the locks (-install)")
	flag.Parse()
	if *printSQL {
		for _, sql := range taxapi.EvidenceJournalSQL(*schema) {
			fmt.Printf("%s;\n", sql)
		}
		return 0
	}
	if !*install && !*prune && !*drop {
		flag.Usage()
		return 2
	}
	db, err := indexerDB.PostgresDbConnect(env("DB_HOST", "localhost"), env("DB_PORT", "5432"), env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"), env("DB_PASS", "postgres"), env("DB_LOG_LEVEL", "warn"))
	if err != nil {
		log.Printf("db connect: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case *drop:
		err = taxapi.DropEvidenceJournal(ctx, db, *lockTimeout)
	case *install:
		err = taxapi.InstallEvidenceJournal(ctx, db, *lockTimeout, *attempts, log.Printf)
	case *prune:
		var n int64
		n, err = taxapi.PruneEvidenceJournal(db)
		log.Printf("evidence journal prune: %d rows removed", n)
	}
	if err != nil {
		log.Printf("evidence journal: %v", err)
		return 1
	}
	log.Printf("evidence journal: done")
	return 0
}
