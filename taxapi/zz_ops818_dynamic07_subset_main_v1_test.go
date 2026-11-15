//go:build ops818dynamicsubset07m1 && ops818dynamic06source && ops818harness && ops818cohorts0607

package taxapi

import (
	"errors"
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOPS818Dynamic07SubsetMainTypedInput(t *testing.T) {
	syscall.Umask(0077)
	root := os.Getenv("OPS818_SUBSET07_MAIN_ROOT")
	if root == "" {
		t.Skip("private actual request only")
	}
	var scope d6Scope
	if xcRead(filepath.Join(root, "scope.private.json"), &scope) != nil || d6Validate(scope) != nil {
		t.Fatal("actual exact scope rejected")
	}
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != xcSource || exec.Command("git", "diff", "--quiet", xcSource, "--").Run() != nil {
		t.Fatal("source tree differs")
	}
	want, e := xcHash(filepath.Join(root, "uniqueness.private.json"))
	if e != nil || want != scope.UniquenessSHA {
		t.Fatal("scope request proof mismatch")
	}
	rec := &d6Log{Interface: logger.Default.LogMode(logger.Silent)}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-subset07-typed-main-v1 default_transaction_read_only=on"), &gorm.Config{Logger: rec})
	if e != nil {
		t.Fatal("readonly transport unavailable")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("readonly pool unavailable")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	server := &Server{db: db, native: DefaultNativeAsset, readOnlyLedger: true, privateReadSnapshotTimeout: 180 * time.Second}
	controls := []map[string]any{}
	var input reportInput
	began := time.Now()
	phase := "begin"
	e = server.withReadSnapshot(func(reader *Server) error {
		for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL jit=off", "SET LOCAL plan_cache_mode=force_custom_plan"} {
			if e := reader.db.Exec(q).Error; e != nil {
				return e
			}
		}
		control := func() error {
			var c struct {
				Readonly           string
				AssignedXID        *int64
				Snapshot, Database string
			}
			if e := reader.db.Raw("SELECT current_setting('transaction_read_only') AS readonly,txid_current_if_assigned() AS assigned_x_id,txid_current_snapshot()::text AS snapshot,current_database() AS database").Scan(&c).Error; e != nil {
				return e
			}
			if c.Readonly != "on" || c.AssignedXID != nil || c.Database != "cosmostax" {
				return errors.New("source controls rejected")
			}
			controls = append(controls, map[string]any{"readonly": c.Readonly, "assigned_xid": c.AssignedXID, "snapshot": c.Snapshot, "database": c.Database})
			return nil
		}
		if e := control(); e != nil {
			return e
		}
		phase = "exact_target_readiness"
		refunds, e := reader.reportReadiness(apiMainnet, []string{scope.Address}, scope.Since, scope.End)
		if e != nil {
			return e
		}
		phase = "exact_full_typed_input"
		rows, e := reader.loadWalletRows(apiMainnet, scope.Address, scope.Since, scope.End, refunds)
		if e != nil {
			return e
		}
		input = reportInput{Wallets: []walletRows{rows}, Fingerprint: ""}
		for _, r := range rows.Events {
			if r.Timestamp.Before(scope.Since) || !r.Timestamp.Before(scope.End) {
				return errors.New("typed event outside actual target")
			}
		}
		for _, r := range rows.Fees {
			if r.Ts.Before(scope.Since) || !r.Ts.Before(scope.End) {
				return errors.New("fee outside actual target")
			}
		}
		return control()
	})
	receipt := map[string]any{"source": xcSource, "address": scope.Address, "since": scope.Since, "asof_exclusive": scope.End, "canonical_window": scope.Key, "phase": phase, "complete": e == nil, "snapshot_controls": controls, "queries": rec.Queries, "elapsed_ms": time.Since(began).Milliseconds(), "typed_input_same_snapshot": true, "BANK_source_queries": 0, "fingerprint_contract": false, "financially_accepted": false}
	if e != nil {
		receipt["error"] = e.Error()
		_ = xcSave(filepath.Join(root, "typed-input-failure.private.json"), receipt)
		t.Fatal("exact typed input failed, no BANK recaptured")
	}
	if len(controls) != 2 || !xcEqual(controls[0], controls[1]) {
		t.Fatal("snapshot controls did not match")
	}
	if e = xcSave(filepath.Join(root, "full.input.private.json"), input); e != nil {
		t.Fatal("input evidence write failed")
	}
	ih, e := xcHash(filepath.Join(root, "full.input.private.json"))
	if e != nil {
		t.Fatal("input hash failed")
	}
	receipt["input_path"] = filepath.Join(root, "full.input.private.json")
	receipt["input_sha256"] = ih
	receipt["readonly"] = "on"
	receipt["assigned_xid"] = nil
	receipt["input_tax_rows"] = len(input.Wallets[0].Events)
	if e = xcSave(filepath.Join(root, "typed-input.receipt.private.json"), receipt); e != nil {
		t.Fatal("typed receipt write failed")
	}
	fmt.Printf("phase exact_subset_typed_input_complete count %d input_sha256 %s\n", len(input.Wallets[0].Events), ih)
}
func TestOPS818Dynamic07SubsetMainSourceGuard(t *testing.T) {
	b, e := os.ReadFile("zz_ops818_dynamic07_subset_main_v1_test.go")
	if e != nil {
		t.Fatal(e)
	}
	body := string(b)
	body = body[strings.Index(body, "func TestOPS818Dynamic07SubsetMainTypedInput("):strings.Index(body, "func TestOPS818Dynamic07SubsetMainSourceGuard(")]
	if strings.Count(body, "reader.loadWalletRows(") != 1 || strings.Count(body, "reader.reportReadiness(") != 1 || strings.Contains(body, ".Rows()") || strings.Contains(body, "message_event_attributes") {
		t.Fatal("typed-only source profile changed")
	}
}
