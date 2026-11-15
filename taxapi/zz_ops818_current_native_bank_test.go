//go:build ops818harness

package taxapi

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

type currentBankWindow struct{ Key, Address, Since, AsOf, CurrentInput, CurrentInputSHA256 string }

func currentBankGzip(path string, raw []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	z, e := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if e != nil {
		f.Close()
		return e
	}
	_, e = z.Write(raw)
	ze := z.Close()
	fe := f.Close()
	if e != nil {
		return e
	}
	if ze != nil {
		return ze
	}
	return fe
}
func currentBankClass(e error) string {
	if e == nil {
		return "complete"
	}
	s := e.Error()
	if strings.Contains(s, "deadline") || strings.Contains(s, "statement timeout") {
		return "native_60s_snapshot_timeout"
	}
	if strings.Contains(s, "lock timeout") {
		return "native_2s_lock_timeout"
	}
	return "native_query_or_receipt_error"
}
func TestOPS818CurrentNativeBank(t *testing.T) {
	var scope struct {
		Source  string
		Queries []string
		Windows []currentBankWindow
	}
	b, e := os.ReadFile(os.Getenv("H_BANK_SCOPE"))
	if e != nil || json.Unmarshal(b, &scope) != nil || scope.Source != "6754a0d7d7986e48e3e3467b2448aae84d2c1b92" || len(scope.Queries) != 2 {
		t.Fatal("exact native bank scope/profile required")
	}
	root := os.Getenv("H_BANK_OUTPUT")
	if e = os.Mkdir(root, 0700); e != nil {
		t.Fatal("exclusive native bank output required")
	}
	if e = os.Mkdir(filepath.Join(root, "windows"), 0700); e != nil {
		t.Fatal("private window dir failed")
	}
	db, e := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-current-native-bank-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=60s", "lock_timeout=2s")
	if e != nil {
		t.Fatal("readonly native bank connection failed")
	}
	handle, e := db.DB()
	if e != nil {
		t.Fatal("native bank handle failed")
	}
	defer handle.Close()
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	query := "SELECT json_build_object('message_attributes',(SELECT COALESCE(json_agg(row_to_json(q)),'[]'::json) FROM (" + scope.Queries[0] + ") q),'block_attributes',(SELECT COALESCE(json_agg(row_to_json(q)),'[]'::json) FROM (" + scope.Queries[1] + ") q),'readonly',current_setting('transaction_read_only'),'assigned_xid',txid_current_if_assigned())"
	profileSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(query)))
	if e = routePrivate(filepath.Join(root, "profile.private.json"), map[string]any{"combined_sql": query, "combined_sql_sha256": profileSHA, "native_transaction_seconds": 60, "raw_bank_source_kind": "indexed_ABCI_bank_attributes_not_independent_chain_block_results", "current_full_input_source": scope.Source, "lossless_raw_gzip": true, "financially_accepted": false}); e != nil {
		t.Fatal("native profile save failed")
	}
	successes, failures := 0, 0
	began := time.Now()
	seen := map[string]bool{}
	for i, w := range scope.Windows {
		if seen[w.Key] || !regexp.MustCompile(`^cosmos1[a-z0-9]+$`).MatchString(w.Address) {
			t.Fatal("duplicate or invalid native window")
		}
		seen[w.Key] = true
		since, se := time.Parse(time.RFC3339Nano, w.Since)
		end, ee := time.Parse(time.RFC3339Nano, w.AsOf)
		if se != nil || ee != nil || !since.Before(end) {
			t.Fatal("invalid native time window")
		}
		rawInput, re := os.ReadFile(w.CurrentInput)
		if re != nil || fmt.Sprintf("%x", sha256.Sum256(rawInput)) != w.CurrentInputSHA256 {
			t.Fatal("current full native input binding failed")
		}
		rawInput = nil
		var disk syscall.Statfs_t
		if syscall.Statfs(root, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
			_ = routePrivate(filepath.Join(root, "capacity-hold-safe.json"), map[string]any{"next_index": i, "remaining": len(scope.Windows) - i, "reason": "preserve all existing evidence; actual persistent capacity below4GiB", "financially_accepted": false})
			t.Fatal("native capacity hold; exact pending scope retained")
		}
		var raw []byte
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		readErr := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if x := tx.Exec("SET LOCAL statement_timeout='60s'").Error; x != nil {
				return x
			}
			if x := tx.Exec("SET LOCAL lock_timeout='2s'").Error; x != nil {
				return x
			}
			args := []any{w.Address, w.Address, since, end, w.Address, w.Address, since, end}
			return tx.Raw(query, args...).Row().Scan(&raw)
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		cancel()
		prefix := filepath.Join(root, "windows", w.Key)
		rawSHA := ""
		compressedSHA := ""
		messageRows, blockRows := 0, 0
		if len(raw) > 0 {
			rawSHA = fmt.Sprintf("%x", sha256.Sum256(raw))
			if e = currentBankGzip(prefix+".native.raw.json.gz", raw); e != nil {
				t.Fatal("lossless native raw capture failed")
			}
			compressed, e := os.ReadFile(prefix + ".native.raw.json.gz")
			if e != nil {
				t.Fatal("native compressed binding failed")
			}
			compressedSHA = fmt.Sprintf("%x", sha256.Sum256(compressed))
			var body struct {
				MessageAttributes []json.RawMessage `json:"message_attributes"`
				BlockAttributes   []json.RawMessage `json:"block_attributes"`
				Readonly          string            `json:"readonly"`
				AssignedXID       *uint64           `json:"assigned_xid"`
			}
			if je := json.Unmarshal(raw, &body); je != nil || body.Readonly != "on" || body.AssignedXID != nil {
				if readErr == nil {
					readErr = fmt.Errorf("native snapshot control markers invalid")
				}
			} else {
				messageRows, blockRows = len(body.MessageAttributes), len(body.BlockAttributes)
			}
		}
		if readErr == nil {
			successes++
		} else {
			failures++
		}
		if e = routePrivate(prefix+".receipt.private.json", map[string]any{"window": w.Key, "address": w.Address, "since": since, "asof_exclusive": end, "current_input": w.CurrentInput, "current_input_sha256": w.CurrentInputSHA256, "current_full_input_source": scope.Source, "raw_bank_source_kind": "fresh_indexed_ABCI_bank_attributes_separate_from_tax_semantics", "executed_sql_sha256": profileSHA, "raw_snapshot_sha256": rawSHA, "compressed_sha256": compressedSHA, "native_snapshot_seconds": 60, "elapsed_ms": time.Since(started).Milliseconds(), "message_attribute_rows": messageRows, "block_attribute_rows": blockRows, "read_error": fmt.Sprint(readErr), "classification": currentBankClass(readErr), "native_complete": readErr == nil, "financially_accepted": false}); e != nil {
			t.Fatal("native receipt save failed")
		}
		raw = nil
		if e = currentAppend(filepath.Join(root, "progress.private.jsonl"), map[string]any{"attempted": i + 1, "total": len(scope.Windows), "successful": successes, "errors": failures, "pending": len(scope.Windows) - i - 1, "last_classification": currentBankClass(readErr), "elapsed_ms": time.Since(began).Milliseconds(), "at": time.Now().UTC()}); e != nil {
			t.Fatal("native progress save failed")
		}
	}
	if e = routePrivate(filepath.Join(root, "summary-safe.json"), map[string]any{"attempted": len(scope.Windows), "successful": successes, "errors": failures, "initial_pending_complete": true, "source": scope.Source, "native_snapshot_seconds": 60, "elapsed_ms": time.Since(began).Milliseconds(), "all_wallet_financial_closure": false, "financially_accepted": false}); e != nil {
		t.Fatal("native summary save failed")
	}
}
