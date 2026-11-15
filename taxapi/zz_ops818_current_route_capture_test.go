//go:build ops818harness

package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func currentPartHash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		panic("private part encoding failed")
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func TestOPS818CurrentAllRouteInputs(t *testing.T) {
	var scope struct {
		Source  string
		Windows []struct {
			Key, Address, OldInput, OldSHA256, ReuseInput, ReuseSHA256 string
			Since, AsOf                                                time.Time
		}
	}
	raw, err := os.ReadFile(os.Getenv("H_CURRENT_ALL_SCOPE"))
	if err != nil || json.Unmarshal(raw, &scope) != nil || scope.Source != "6754a0d7d7986e48e3e3467b2448aae84d2c1b92" || len(scope.Windows) == 0 {
		t.Fatal("bound complete private scope required")
	}
	out := os.Getenv("H_CURRENT_ALL_DIR")
	if os.Mkdir(out, 0700) != nil {
		t.Fatal("exclusive output required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-current6754-all-route-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=0", "lock_timeout=2s")
	if err != nil {
		t.Fatal("readonly connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal("connection handle failed")
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	s := NewServer(db, NewOracle("http://127.0.0.1:1", "http://127.0.0.1:1"), DefaultNativeAsset)
	s.readOnlyLedger = true
	s.privateReadSnapshotTimeout = time.Minute
	errors, exact, reused := 0, 0, 0
	for i, w := range scope.Windows {
		var space syscall.Statfs_t
		if syscall.Statfs(out, &space) != nil || uint64(space.Bavail)*uint64(space.Bsize) < 1024*1024*1024 {
			t.Fatal("private capacity exhausted, remaining manifest preserved")
		}
		var old reportInput
		var e error
		if w.OldInput != "" {
			oldBytes, readOldErr := os.ReadFile(w.OldInput)
			if readOldErr != nil || fmt.Sprintf("%x", sha256.Sum256(oldBytes)) != w.OldSHA256 {
				t.Fatal("old immutable input binding failed")
			}
			if json.Unmarshal(oldBytes, &old) != nil || len(old.Wallets) != 1 {
				t.Fatal("old input invalid")
			}
		}
		started := time.Now()
		var current reportInput
		var readErr error
		if w.ReuseInput != "" {
			b, e := os.ReadFile(w.ReuseInput)
			if e != nil || fmt.Sprintf("%x", sha256.Sum256(b)) != w.ReuseSHA256 || json.Unmarshal(b, &current) != nil {
				t.Fatal("retained current native proof binding failed")
			}
			if e = os.Link(w.ReuseInput, filepath.Join(out, w.Key+".input.private.json")); e != nil {
				t.Fatal("retained current link failed")
			}
			reused++
		} else {
			current, readErr = s.readReportInput(apiMainnet, []string{w.Address}, w.Since, w.AsOf, false)
			if e = routePrivate(filepath.Join(out, w.Key+".input.private.json"), current); e != nil {
				t.Fatal("exclusive current input save failed")
			}
		}
		receipt := map[string]any{"index": i, "window": w.Key, "source": scope.Source, "since": w.Since, "asof_exclusive": w.AsOf, "old_input_sha256": w.OldSHA256, "current_input_sha256": currentPartHash(current), "build_sha256": os.Getenv("H_BUILD_SHA256"), "source_binding_sha256": os.Getenv("H_SOURCE_BINDING_SHA256"), "elapsed_ms": time.Since(started).Milliseconds(), "read_error": fmt.Sprint(readErr), "native_snapshot_seconds": 60, "reused": w.ReuseInput != "", "financially_accepted": false}
		if readErr != nil {
			errors++
		} else {
			if len(current.Wallets) != 1 {
				t.Fatal("current wallet count invalid")
			}
			b := current.Wallets[0]
			var a walletRows
			if len(old.Wallets) == 1 {
				a = old.Wallets[0]
			}
			receipt["old_input_available"] = len(old.Wallets) == 1
			receipt["old_parts_sha256"] = map[string]string{"events": currentPartHash(a.Events), "memos": currentPartHash(a.Memos), "fees": currentPartHash(a.Fees), "fingerprint": currentPartHash(old.Fingerprint)}
			receipt["current_parts_sha256"] = map[string]string{"events": currentPartHash(b.Events), "memos": currentPartHash(b.Memos), "fees": currentPartHash(b.Fees), "fingerprint": currentPartHash(current.Fingerprint)}
			receipt["events"] = len(b.Events)
			receipt["old_fee_rows"] = len(a.Fees)
			receipt["current_fee_rows"] = len(b.Fees)
			receipt["ordered_fee_rows_exact"] = reflect.DeepEqual(a.Fees, b.Fees)
			if len(old.Wallets) == 1 {
				old.Wallets[0].Fees = nil
			}
			current.Wallets[0].Fees = nil
			equal := len(old.Wallets) == 1 && reflect.DeepEqual(old, current)
			receipt["nonfee_input_exact"] = equal
			if equal {
				exact++
			}
		}
		if e = routePrivate(filepath.Join(out, w.Key+".receipt.private.json"), receipt); e != nil {
			t.Fatal("exclusive receipt save failed")
		}
		progress := map[string]any{"completed": i + 1, "total": len(scope.Windows), "errors": errors, "nonfee_exact": exact, "reused": reused, "at": time.Now().UTC(), "last_elapsed_ms": time.Since(started).Milliseconds(), "last_read_failed": readErr != nil}
		bytes, e := json.Marshal(progress)
		if e != nil {
			t.Fatal("progress encoding failed")
		}
		f, e := os.OpenFile(filepath.Join(out, "progress.private.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal("progress open failed")
		}
		_, e = f.Write(append(bytes, '\n'))
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			t.Fatal("progress save failed")
		}
	}
	if err = routePrivate(filepath.Join(out, "summary-safe.json"), map[string]any{"windows": len(scope.Windows), "errors": errors, "nonfee_exact": exact, "reused": reused, "financially_accepted": false}); err != nil {
		t.Fatal("summary save failed")
	}
	t.Log("complete scope captured", len(scope.Windows), "errors", errors, "nonfee exact", exact)
}
