// balancecheck rebuilds wallets' balances at past dates with the API's ledger
// and prints one JSON line per wallet and date, for comparison against archive
// state.
//
//	balancecheck -dates 2025-12-31,2026-03-31 < addresses.txt
//
// With -heights it instead rebuilds the raw holdings after each block and,
// with -expect, compares them with the state a balanceexpect file records
// for the same wallets and heights. It then writes each wallet's holdings
// and differences to -out (created 0600) and prints only counts, so no
// wallet or balance reaches the terminal.
//
//	balancecheck -no-migrate -pre-v6 -unnamed-staking -heights 5200790,8700000 \
//	    -expect expected.jsonl -out private/rebuilt.jsonl < addresses.txt
//
// -pre-v6 and -unnamed-staking switch on the Hub ledger before Gaia v6 and
// before Gaia v15 for this run only, as TAX_BALANCE_PRE_V6_TRANSFERS and
// TAX_BALANCE_UNNAMED_STAKING do for the API. -no-migrate skips the balance
// history migration, for a read only connection.
//
// Env: the same DB_* and NODE_REST_API as taxapid, ORACLE_URL for prices.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
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
	heightList := flag.String("heights", "", "comma-separated block heights; rebuilds raw holdings after each instead of -dates")
	expect := flag.String("expect", "", "with -heights: a balanceexpect JSONL file to compare against")
	outPath := flag.String("out", "", "with -heights: private JSONL output of the rebuilt holdings and differences")
	noMigrate := flag.Bool("no-migrate", false, "skip the balance history migration")
	preV6 := flag.Bool("pre-v6", false, "rebuild Hub balances before Gaia v6 from transfer events")
	unnamed := flag.Bool("unnamed-staking", false, "attribute Hub staking events before Gaia v15 from the staking rows")
	flag.Parse()
	if *heightList != "" {
		if *outPath == "" {
			log.Fatal("-heights needs -out")
		}
		var heights []int64
		for _, h := range strings.Split(*heightList, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
			if err != nil {
				log.Fatalf("bad height %q", h)
			}
			heights = append(heights, n)
		}
		srv := connect(*noMigrate, *preV6, *unnamed)
		checkHeights(srv, heights, *expect, *outPath)
		return
	}
	var days []time.Time
	for _, d := range strings.Split(*dates, ",") {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(d))
		if err != nil {
			log.Fatalf("bad date %q", d)
		}
		days = append(days, t)
	}
	srv := connect(*noMigrate, *preV6, *unnamed)
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

func connect(noMigrate, preV6, unnamed bool) *taxapi.Server {
	db, err := indexerDB.PostgresDbConnect(env("DB_HOST", "localhost"), env("DB_PORT", "5432"), env("DB_NAME", "cosmostax"), env("DB_USER", "postgres"), env("DB_PASS", "postgres"), "warn")
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if !noMigrate {
		if err := taxapi.BalanceHistoryMigrate(db); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	srv := taxapi.NewServer(db, taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API")), taxapi.DefaultNativeAsset)
	if preV6 || unnamed {
		srv.SetEarlyHubLedger(preV6, unnamed)
	}
	return srv
}

// checkHeights rebuilds every wallet on stdin at each height, compares with
// the expected states and prints counts only. Each wallet is numbered in the
// order read, and the counts name wallets by that number.
func checkHeights(srv *taxapi.Server, heights []int64, expectPath, outPath string) {
	want := map[string]taxapi.HeightHoldings{}
	if expectPath != "" {
		f, err := os.Open(expectPath)
		if err != nil {
			log.Fatalf("expect: %v", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var h taxapi.HeightHoldings
			if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
				log.Fatalf("expect line: %v", err)
			}
			want[fmt.Sprintf("%s@%d", h.Address, h.Height)] = h
		}
		if err := sc.Err(); err != nil {
			log.Fatalf("expect: %v", err)
		}
		_ = f.Close()
	}
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Fatalf("out: %v", err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	var matched, differ, refused, unexpected, failed int
	sc := bufio.NewScanner(os.Stdin)
	n := 0
	for sc.Scan() {
		addr := strings.TrimSpace(sc.Text())
		if addr == "" {
			continue
		}
		n++
		start := time.Now()
		got, err := srv.HoldingsAtHeights("mainnet", addr, heights)
		if err != nil {
			failed++
			_ = enc.Encode(map[string]any{"wallet": n, "address": addr, "error": err.Error()})
			fmt.Printf("wallet %d: error (see -out)\n", n)
			continue
		}
		for _, g := range got {
			line := map[string]any{"wallet": n, "rebuilt": g, "seconds": time.Since(start).Seconds()}
			verdict := "no expected state"
			w, ok := want[fmt.Sprintf("%s@%d", addr, g.Height)]
			switch {
			case g.Status != "":
				refused++
				verdict = "refused"
			case !ok:
				unexpected++
			default:
				line["expected"] = w
				if d := taxapi.HoldingsMismatch(g, w); len(d) > 0 {
					differ++
					line["differences"] = d
					verdict = fmt.Sprintf("%d differences", len(d))
				} else {
					matched++
					verdict = "match"
				}
			}
			_ = enc.Encode(line)
			fmt.Printf("wallet %d height %d: %s\n", n, g.Height, verdict)
		}
	}
	fmt.Printf("matched %d, differ %d, refused %d, no expected state %d, errors %d\n", matched, differ, refused, unexpected, failed)
}
