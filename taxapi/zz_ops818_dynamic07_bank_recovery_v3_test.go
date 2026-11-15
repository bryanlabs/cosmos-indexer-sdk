//go:build ops818dynamic07bankv3 && ops818dynamic06source && ops818harness && ops818cohorts0607

package taxapi

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type n7RecoveryPlan struct {
	PriorReceipt, PriorReceiptSHA, PriorInput, PriorInputSHA string
	InputRows                                                int
}

func n7ValidatePrior(plan n7RecoveryPlan, s d6Scope) error {
	for p, w := range map[string]string{plan.PriorReceipt: plan.PriorReceiptSHA, plan.PriorInput: plan.PriorInputSHA} {
		h, e := xcHash(p)
		if e != nil || h != w {
			return errors.New("prior immutable bytes rejected")
		}
	}
	var r struct {
		Source   string          `json:"source"`
		Address  string          `json:"address"`
		Key      string          `json:"window"`
		Since    time.Time       `json:"since"`
		End      time.Time       `json:"asof_exclusive"`
		Phase    string          `json:"phase"`
		Readonly string          `json:"readonly"`
		XID      json.RawMessage `json:"assigned_xid"`
		InputSHA string          `json:"current_input_sha256"`
		Complete bool            `json:"native_complete"`
		Counts   []int           `json:"bank_namespace_counts"`
	}
	if xcRead(plan.PriorReceipt, &r) != nil || r.Source != xcSource || r.Address != s.Address || r.Key != s.Key || !r.Since.Equal(s.Since) || !r.End.Equal(s.End) || r.Phase != "bank_namespace_0" || r.Readonly != "on" || string(r.XID) != "null" || r.InputSHA != plan.PriorInputSHA || r.Complete || len(r.Counts) != 0 {
		return errors.New("prior successful typed input / absent BANK result binding rejected")
	}
	var in reportInput
	if xcRead(plan.PriorInput, &in) != nil || len(in.Wallets) != 1 || len(in.Wallets[0].Events) != plan.InputRows || in.Fingerprint != "" {
		return errors.New("retained typed input rejected")
	}
	return nil
}
func TestOPS818Dynamic07BankRecoveryV3(t *testing.T) {
	root := os.Getenv("OPS818_DYNAMIC07_BANK_V3_ROOT")
	if root == "" {
		t.Skip("private recovery only")
	}
	syscall.Umask(0077)
	var s d6Scope
	var plan n7RecoveryPlan
	if xcRead(filepath.Join(root, "scope.private.json"), &s) != nil || d6Validate(s) != nil || xcRead(filepath.Join(root, "recovery-plan.private.json"), &plan) != nil || n7ValidatePrior(plan, s) != nil {
		t.Fatal("recovery source bindings rejected")
	}
	lock, e := os.OpenFile(filepath.Join(root, "capture.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal("capture lock")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("active capture conflict")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	log := &d6Log{Interface: logger.Default.LogMode(logger.Silent)}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-dynamic07-bank-recovery-v3 default_transaction_read_only=on"), &gorm.Config{Logger: log})
	if e != nil {
		t.Fatal("readonly transport")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("readonly pool")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	server := &Server{db: db, native: DefaultNativeAsset, readOnlyLedger: true, privateReadSnapshotTimeout: 1200 * time.Second}
	controls := []map[string]any{}
	counts := []int{}
	schemas := [][]map[string]string{}
	began := time.Now()
	phase := "begin"
	f, e := os.OpenFile(filepath.Join(root, "native.raw.json.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("raw immutable reserve")
	}
	ch := sha256.New()
	rh := sha256.New()
	z := gzip.NewWriter(io.MultiWriter(f, ch))
	out := io.MultiWriter(z, rh)
	fmt.Println("phase dynamic07_bank_only_started count 1")
	e = server.withReadSnapshot(func(reader *Server) error {
		for _, q := range []string{"SET LOCAL statement_timeout='600s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL jit=off", "SET LOCAL plan_cache_mode=force_custom_plan"} {
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
				return errors.New("readonly control failed")
			}
			controls = append(controls, map[string]any{"readonly": c.Readonly, "assigned_xid": c.AssignedXID, "snapshot": c.Snapshot, "database": c.Database})
			return nil
		}
		if e := control(); e != nil {
			return e
		}
		for i, q := range s.Queries {
			phase = fmt.Sprintf("bank_namespace_%d", i)
			prefix := ","
			if i == 0 {
				prefix = "{"
			}
			if _, e := fmt.Fprintf(out, "%s%q:[", prefix, []string{"message_attributes", "block_attributes"}[i]); e != nil {
				return e
			}
			rows, e := reader.db.Raw(q, s.Address, s.Address, s.Since, s.End).Rows()
			if e != nil {
				return e
			}
			n, schema, e := d6Stream(out, rows)
			counts = append(counts, n)
			schemas = append(schemas, schema)
			if e != nil {
				return e
			}
			if _, e = io.WriteString(out, "]"); e != nil {
				return e
			}
			fmt.Printf("phase dynamic07_bank_namespace_complete count %d\n", n)
		}
		if e := control(); e != nil {
			return e
		}
		_, e := io.WriteString(out, `,"readonly":"on","assigned_xid":null}`)
		return e
	})
	ze := z.Close()
	fe := f.Sync()
	ce := f.Close()
	if e == nil {
		e = ze
	}
	if e == nil {
		e = fe
	}
	if e == nil {
		e = ce
	}
	receipt := map[string]any{"source": xcSource, "window": s.Key, "address": s.Address, "since": s.Since, "asof_exclusive": s.End, "phase": phase, "native_complete": e == nil, "readonly": "on", "assigned_xid": nil, "full_input_and_bank_same_snapshot": false, "bank_same_snapshot": true, "snapshot_controls": controls, "input_source_receipt": plan.PriorReceipt, "input_source_receipt_sha256": plan.PriorReceiptSHA, "input_source_path": plan.PriorInput, "input_source_sha256": plan.PriorInputSHA, "queries": log.Queries, "bank_queries": s.Queries, "bank_namespace_counts": counts, "bank_typed_schemas": schemas, "raw_snapshot_sha256": fmt.Sprintf("%x", rh.Sum(nil)), "compressed_sha256": fmt.Sprintf("%x", ch.Sum(nil)), "raw_bank_source_kind": "fresh_indexed_ABCI_bank_attributes_separate_from_tax_semantics", "native_snapshot_seconds": 1200, "statement_timeout_seconds": 600, "current_full_input_source": xcSource, "current_input": filepath.Join(root, "full.input.private.json"), "current_input_sha256": plan.PriorInputSHA, "elapsed_ms": time.Since(began).Milliseconds(), "financially_accepted": false, "closing_reconciled": false, "fingerprint_contract": false, "pagination": "single streamed cursor per namespace, EOF and rows.Err required", "boundaries": "exact UTC [Since,End), original query templates unchanged"}
	if e != nil {
		receipt["error"] = e.Error()
	}
	if xcSave(filepath.Join(root, "source.receipt.private.json"), receipt) != nil {
		t.Fatal("receipt persist failed")
	}
	if e != nil {
		t.Fatal("BANK-only source failed explicitly")
	}
	fmt.Println("phase dynamic07_bank_only_complete count 1")
}
func TestOPS818Dynamic07BankRecoveryV3SourceGuard(t *testing.T) {
	b, e := os.ReadFile("zz_ops818_dynamic07_bank_recovery_v3_test.go")
	if e != nil {
		t.Fatal(e)
	}
	_ = b
	root := os.Getenv("OPS818_DYNAMIC07_BANK_V3_ROOT")
	if root != "" {
		var s d6Scope
		var p n7RecoveryPlan
		if xcRead(filepath.Join(root, "scope.private.json"), &s) != nil || xcRead(filepath.Join(root, "recovery-plan.private.json"), &p) != nil || n7ValidatePrior(p, s) != nil {
			t.Fatal("actual completed input binding rejected")
		}
		bad := p
		bad.PriorInputSHA = "wrong"
		if n7ValidatePrior(bad, s) == nil {
			t.Fatal("tampered input binding accepted")
		}
		badScope := s
		badScope.Key = "wrong"
		if n7ValidatePrior(p, badScope) == nil {
			t.Fatal("wrong scope accepted")
		}
	}
	// Validation is independent of any source connection and rejects missing hashes.
	if n7ValidatePrior(n7RecoveryPlan{}, d6Scope{}) == nil {
		t.Fatal("empty prior binding accepted")
	}
}
