// balancecheck rebuilds wallets' balances at past dates with the API's ledger
// and prints one JSON line per wallet and date, for comparison against archive
// state.
//
//	balancecheck -dates 2025-12-31,2026-03-31 < addresses.txt
//
// Env: the same DB_* and NODE_REST_API as taxapid, ORACLE_URL for prices.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
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
	dates := flag.String("dates", "", "comma-separated YYYY-MM-DD days")
	flag.Parse()
	var days []time.Time
	for _, d := range strings.Split(*dates, ",") {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(d))
		if err != nil {
			log.Fatalf("bad date %q", d)
		}
		days = append(days, t)
	}
	db, err := indexerDB.PostgresDbConnect(env("DB_HOST", "localhost"), env("DB_PORT", "5432"), env("DB_NAME", "cosmostax"), env("DB_USER", "postgres"), env("DB_PASS", "postgres"), "warn")
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if err := taxapi.BalanceHistoryMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	srv := taxapi.NewServer(db, taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API")), taxapi.DefaultNativeAsset)
	enc := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		addr := strings.TrimSpace(sc.Text())
		if addr == "" {
			continue
		}
		start := time.Now()
		out, err := srv.HistoricalBalances("mainnet", addr, days)
		if err != nil {
			_ = enc.Encode(map[string]any{"address": addr, "error": err.Error()})
			continue
		}
		for _, b := range out {
			_ = enc.Encode(map[string]any{"address": addr, "balance": b, "seconds": time.Since(start).Seconds()})
		}
	}
}
