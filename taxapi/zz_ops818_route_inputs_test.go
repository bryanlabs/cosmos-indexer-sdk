//go:build ops818harness

package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func routePrivate(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func TestOPS818CurrentTwoRouteInputs(t *testing.T) {
	var scope struct {
		Wallets []struct {
			Address     string
			Since, AsOf time.Time
			OldInput    string
		}
	}
	data, err := os.ReadFile(os.Getenv("H_CURRENT_TWO_SCOPE"))
	if err != nil || json.Unmarshal(data, &scope) != nil || len(scope.Wallets) == 0 || len(scope.Wallets) > 2 {
		t.Fatal("bounded exact private windows required")
	}
	out := os.Getenv("H_CURRENT_TWO_DIR")
	if os.Mkdir(out, 0700) != nil {
		t.Fatal("unique current-source output required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-current8704-two-route-inputs-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=0", "lock_timeout=2s")
	if err != nil {
		t.Fatal("readonly source connection failed")
	}
	s := NewServer(db, NewOracle("http://127.0.0.1:1", "http://127.0.0.1:1"), DefaultNativeAsset)
	s.readOnlyLedger = true
	s.privateReadSnapshotTimeout = time.Minute
	errors, exact := 0, 0
	for _, w := range scope.Wallets {
		oldBytes, err := os.ReadFile(w.OldInput)
		if err != nil {
			t.Fatal("retained input missing")
		}
		var prior reportInput
		if json.Unmarshal(oldBytes, &prior) != nil || len(prior.Wallets) != 1 {
			t.Fatal("retained input invalid")
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(w.Address)))
		began := time.Now()
		current, readErr := s.readReportInput(apiMainnet, []string{w.Address}, w.Since, w.AsOf, false)
		if err := routePrivate(filepath.Join(out, key+".input.private.json"), current); err != nil {
			t.Fatal("exclusive input save failed")
		}
		record := map[string]any{"wallet": key, "since": w.Since, "asof_exclusive": w.AsOf, "source_base_commit": "8704ae6c88b353a19d0bfc7abf5f0b708ac321d9", "source_diff_sha256": os.Getenv("H_SOURCE_DIFF_SHA256"), "harness_binary_sha256": os.Getenv("H_BUILD_SHA256"), "old_input_sha256": fmt.Sprintf("%x", sha256.Sum256(oldBytes)), "read_error": fmt.Sprint(readErr), "readonly_snapshot": true, "elapsed_ms": time.Since(began).Milliseconds(), "input_exact_excluding_only_fees": false, "financially_accepted": false}
		if readErr != nil {
			errors++
		} else {
			if len(current.Wallets) != 1 {
				t.Fatal("current wallet count mismatch")
			}
			oldFeeRows, newFeeRows := prior.Wallets[0].Fees, current.Wallets[0].Fees
			prior.Wallets[0].Fees = nil
			current.Wallets[0].Fees = nil
			same := reflect.DeepEqual(prior, current)
			if same {
				exact++
			}
			record["input_exact_excluding_only_fees"] = same
			record["events"] = len(current.Wallets[0].Events)
			record["old_fee_rows"] = len(oldFeeRows)
			record["current_fee_rows"] = len(newFeeRows)
			record["every_ordered_fee_coin_exact_retained"] = reflect.DeepEqual(oldFeeRows, newFeeRows)
		}
		if err := routePrivate(filepath.Join(out, key+".receipt.private.json"), record); err != nil {
			t.Fatal("exclusive receipt save failed")
		}
	}
	if err := routePrivate(filepath.Join(out, "summary-safe.json"), map[string]any{"wallets": len(scope.Wallets), "errors": errors, "input_exact_excluding_only_fees": exact, "source_base_commit": "8704ae6c88b353a19d0bfc7abf5f0b708ac321d9", "source_diff_sha256": os.Getenv("H_SOURCE_DIFF_SHA256"), "financially_accepted": false}); err != nil {
		t.Fatal("summary save failed")
	}
	t.Log("exact current-source snapshots", len(scope.Wallets), "errors", errors, "unchanged full nonfee inputs", exact)
	if errors > 0 || exact != len(scope.Wallets) {
		t.Fatal("current source input proof incomplete")
	}
}
