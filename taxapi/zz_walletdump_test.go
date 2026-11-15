//go:build walletdump

package taxapi

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

// Read-only per-wallet row dump for before/after comparisons. Not committed.
func TestWalletDump(t *testing.T) {
	db, err := dbpkg.PostgresDbConnect(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"), os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASS"), "silent")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	if err := db.Exec("SET default_transaction_read_only = on; SET statement_timeout='300s'").Error; err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), ""), DefaultNativeAsset)
	start, _ := time.Parse("2006-01-02", os.Getenv("START"))
	end, _ := time.Parse("2006-01-02", os.Getenv("END"))
	in, err := os.Open(os.Getenv("WALLETS"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(os.Getenv("OUT"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		addr := strings.TrimSpace(sc.Text())
		if addr == "" {
			continue
		}
		t0 := time.Now()
		var rows walletRows
		err := s.db.Transaction(func(tx *gorm.DB) error {
			var err error
			rows, err = (&Server{db: tx}).loadWalletRows("cosmoshub", addr, start, end)
			return err
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		rec := map[string]any{"a": addr, "ms": time.Since(t0).Milliseconds(), "events": rows.Events, "fees": len(rows.Fees)}
		if err != nil {
			rec["err"] = err.Error()
		}
		if err := enc.Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
}
