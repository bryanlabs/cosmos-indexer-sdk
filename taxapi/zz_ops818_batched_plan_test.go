//go:build ops818harness

package taxapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func TestOPS818BatchedPlan(t *testing.T) {
	var scope struct {
		Wallets []struct {
			Address     string
			Since, AsOf time.Time
		}
	}
	b, e := os.ReadFile(os.Getenv("H_CURRENT_TWO_SCOPE"))
	if e != nil || json.Unmarshal(b, &scope) != nil || len(scope.Wallets) != 1 {
		t.Fatal("bounded private scope required")
	}
	db, e := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-batched-metadata-plan-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=60s", "lock_timeout=2s")
	if e != nil {
		t.Fatal("source connection failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Begin()
	defer tx.Rollback()
	if tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error != nil {
		t.Fatal("readonly transaction failed")
	}
	w := scope.Wallets[0]
	var plan string
	started := time.Now()
	if tx.Raw("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+privateWalletTaxEventsSQL, w.Address, w.Address, w.Since, w.AsOf, chainIDs(apiMainnet)).Row().Scan(&plan) != nil {
		t.Fatal("bounded actual plan failed")
	}
	if routePrivate(filepath.Join(os.Getenv("H_CURRENT_TWO_DIR"), "actual-plan.private.json"), json.RawMessage(plan)) != nil {
		t.Fatal("exclusive plan save failed")
	}
	var unassigned bool
	if tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error != nil || !unassigned {
		t.Fatal("read-only proof failed")
	}
	if routePrivate(filepath.Join(os.Getenv("H_CURRENT_TWO_DIR"), "plan-safe.json"), map[string]any{"elapsed_ms": time.Since(started).Milliseconds(), "unassigned_xid": unassigned}) != nil {
		t.Fatal("plan receipt failed")
	}
}
