package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestWrapperPasswordIsOptionalNotInvented(t *testing.T) {
	required := requiredDBEnvironment()
	if strings.Join(required, ",") != "DB_HOST,DB_NAME,DB_USER" {
		t.Fatal("unexpected required environment contract")
	}
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", "/dev/null")
	t.Setenv("PGPASSWORD", "fixture-ambient-password-must-not-be-used")
	cfg, e := databaseConfig("127.0.0.1", "55473", "query_proof_scratch_harness_20261115", "danb", "")
	if e != nil || cfg.Password != "" {
		t.Fatal("driver did not preserve empty password input")
	}
}
func TestWrapperEmptyPasswordReadOnlyScratchConnection(t *testing.T) {
	dsn := os.Getenv("PROOF_SCRATCH_DSN")
	if dsn == "" {
		t.Skip("explicit disposable loopback scratch database required")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || cfg.Host != "127.0.0.1" || !strings.HasPrefix(cfg.Database, "query_proof_scratch_") {
		t.Fatal("unsafe scratch target refused")
	}
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", "/dev/null")
	db, e := connectDatabase(cfg.Host, "55473", cfg.Database, cfg.User, "")
	if e != nil {
		t.Fatal("empty-password scratch connection failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("scratch pool unavailable")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	var readOnly string
	if e = db.Raw("SELECT current_setting('transaction_read_only')").Scan(&readOnly).Error; e != nil || readOnly != "on" {
		t.Fatal("wrapper connection was not read-only")
	}
}
