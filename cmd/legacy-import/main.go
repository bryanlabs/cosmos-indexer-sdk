// Command legacy-import is the offline prototype importer for Cosmos Hub 3.
// It reads retained block captures, decodes them with the legacy candidate,
// builds tax rows with the serving classifier and stores them through the
// inclusion store. It makes no network calls. It only accepts a loopback
// database whose name starts with "legacy_", and refuses port 15432, so it
// cannot be pointed at the serving database. Output is aggregate counts only:
// no addresses, amounts, memos or hashes are printed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	indexerdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/store"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	digits      = regexp.MustCompile(`\d+`)
	privateName = regexp.MustCompile(`^legacy_[a-z0-9_]{1,40}$`)
	safeText    = regexp.MustCompile(`^[A-Za-z0-9 _.,:/()'-]{1,90}$`)
)

func safe(err error) string {
	text := digits.ReplaceAllString(err.Error(), "N")
	if !safeText.MatchString(text) {
		return "error text withheld"
	}
	return text
}

func openPrivate(dsn string, createSchema bool) (*gorm.DB, error) {
	fields := map[string]string{}
	for _, part := range strings.Fields(dsn) {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	ip := net.ParseIP(fields["host"])
	if ip == nil || !ip.IsLoopback() || fields["port"] == "" || fields["port"] == "15432" || !privateName.MatchString(fields["dbname"]) {
		return nil, fmt.Errorf("refusing a database that is not a private loopback database with a legacy_ name")
	}
	quiet := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	if createSchema {
		// The name was checked above to be legacy_ plus identifier characters.
		admin := strings.Replace(dsn, "dbname="+fields["dbname"], "dbname=postgres", 1)
		if server, err := gorm.Open(postgres.Open(admin), quiet); err == nil {
			var exists bool
			server.Raw(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = ?)`, fields["dbname"]).Scan(&exists)
			if !exists {
				server.Exec("CREATE DATABASE " + fields["dbname"])
			}
			if sql, err := server.DB(); err == nil {
				_ = sql.Close()
			}
		}
	}
	db, err := gorm.Open(postgres.Open(dsn), quiet)
	if err != nil {
		return nil, fmt.Errorf("private database unavailable")
	}
	if createSchema {
		if err := indexerdb.MigrateModels(db); err != nil {
			return nil, err
		}
		if err := db.AutoMigrate(&tax.TaxableEvent{}); err != nil {
			return nil, err
		}
	}
	if createSchema {
		if err := store.Migrate(db); err != nil {
			return nil, err
		}
		if err := store.InstallCrossChainGuard(db); err != nil {
			return nil, err
		}
	}
	return db, store.Ready(db)
}

type counter map[string]int64

func (c counter) print(title string) {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if c[keys[a]] != c[keys[b]] {
			return c[keys[a]] > c[keys[b]]
		}
		return keys[a] < keys[b]
	})
	fmt.Println(title + ":")
	for _, k := range keys {
		fmt.Printf("  %-70s %d\n", k, c[k])
	}
}

func main() {
	root := flag.String("cache", "/cache", "block cache root")
	chain := flag.String("chain", "cosmoshub-3", "legacy chain to import")
	from := flag.Int64("from", 0, "first height")
	to := flag.Int64("to", 0, "last height")
	createSchema := flag.Bool("create-schema", false, "create the serving schema, the inclusion table and the guard first; run it once, alone, before parallel ranges")
	maxBody := flag.Int64("max-body", 64<<20, "largest decompressed capture accepted")
	resolutions := flag.String("resolutions", "", "file written by legacy-seqcheck: chain/height/index and a fee verdict per line")
	flag.Parse()
	verdicts := map[string]string{}
	if *resolutions != "" {
		text, err := os.ReadFile(*resolutions)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolutions file unavailable")
			os.Exit(2)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(text)), "\n") {
			key, verdict, ok := strings.Cut(line, " ")
			if !ok || !strings.HasPrefix(key, *chain+"/") || verdicts[key] != "" {
				fmt.Fprintln(os.Stderr, "resolutions file has a line for another chain, a repeated line or no verdict")
				os.Exit(2)
			}
			verdicts[key] = verdict
		}
	}
	if *from <= 0 || *to < *from {
		fmt.Fprintln(os.Stderr, "a height range is required")
		os.Exit(2)
	}
	db, err := openPrivate(os.Getenv("LEGACY_IMPORT_DSN"), *createSchema)
	if err != nil {
		fmt.Fprintln(os.Stderr, safe(err))
		os.Exit(2)
	}
	started := time.Now()
	heights := counter{}
	held := counter{}
	stored := counter{}
	rows := counter{}
	fees := counter{}
	messages := counter{}
	for height := *from; height <= *to; height++ {
		block, results, err := capture.Load(*root, *chain, height, *maxBody)
		if errors.Is(err, os.ErrNotExist) {
			heights["not in the cache"]++
			continue
		}
		if err != nil {
			heights["unreadable"]++
			held[safe(err)]++
			continue
		}
		heights["read"]++
		items, err := core.DecodeLegacyBlock(block, results)
		if err == nil {
			err = core.ResolveLegacyFees(items, verdicts)
		}
		if err == nil {
			var out store.Block
			if out, err = core.LegacyStoreBlock(block, items); err == nil {
				if err = store.ImportBlock(db, out); err == nil {
					heights["stored"]++
					stored["transactions"] += int64(len(out.Inclusions))
					if len(out.Inclusions) > 0 {
						heights["stored with transactions"]++
					}
					for _, item := range items {
						shape := "no fee coins signed"
						if len(item.Fee.Declared) > 0 {
							shape = "zero fee signed"
							if item.Fee.Declared.IsAllPositive() {
								shape = "positive fee signed"
							}
						}
						fees[item.Fee.Charge+", "+shape]++
					}
					for _, inc := range out.Inclusions {
						if inc.Code == 0 {
							stored["successful transactions"]++
						} else {
							stored["failed transactions"]++
						}
						if len(inc.Detail.Fee) > 0 {
							stored["transactions with a fee charged"]++
						}
						for _, m := range inc.Detail.Messages {
							messages[m.TypeURL]++
							if inc.Code == 0 && len(m.Rows) == 0 {
								rows["(executed message with no rows)"]++
							}
							for _, r := range m.Rows {
								rows[r.Category]++
							}
						}
					}
					continue
				}
			}
		}
		heights["held"]++
		stored["transactions in held heights"] += int64(len(block.Block.Txs))
		held[safe(err)]++
	}
	fmt.Printf("chain %s heights %d to %d in %s\n", *chain, *from, *to, time.Since(started).Round(time.Second))
	heights.print("heights")
	stored.print("transactions")
	fees.print("fee of stored transactions")
	messages.print("stored messages by type")
	rows.print("stored tax rows by category")
	held.print("held heights by reason")
	scope := ` JOIN tx_inclusions i ON i.tx_id = t.tx_id JOIN chains c ON c.id = i.chain_id WHERE c.chain_id = ?`
	fmt.Println("stored for this chain in the database:")
	for _, total := range []struct{ label, sql string }{
		{"blocks", `SELECT count(*) FROM blocks b JOIN chains c ON c.id = b.chain_id WHERE c.chain_id = ?`},
		{"inclusions", `SELECT count(*) FROM tx_inclusions i JOIN chains c ON c.id = i.chain_id WHERE c.chain_id = ?`},
		{"hashes included more than once", `SELECT count(*) FROM (SELECT i.hash FROM tx_inclusions i JOIN chains c ON c.id = i.chain_id WHERE c.chain_id = ? GROUP BY i.hash HAVING count(*) > 1) repeated`},
		{"signer links", `SELECT count(*) FROM tx_signer_addresses t` + scope},
		{"fee rows", `SELECT count(*) FROM fees t` + scope},
		{"messages", `SELECT count(*) FROM messages t` + scope},
		{"tax rows", `SELECT count(*) FROM taxable_events e JOIN messages t ON t.id = e.message_id` + scope},
	} {
		var n int64
		if err := db.Raw(total.sql, *chain).Scan(&n).Error; err != nil {
			fmt.Printf("  %-70s unavailable\n", total.label)
			continue
		}
		fmt.Printf("  %-70s %d\n", total.label, n)
	}
}
