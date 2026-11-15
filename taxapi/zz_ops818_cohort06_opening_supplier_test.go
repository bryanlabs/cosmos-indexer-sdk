//go:build ops818opening06

package taxapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const o6Source = "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const o6Root = "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/validator-06"

var o6Dir = os.Getenv("OPS818_OPENING06_ROOT")

type o6Log struct {
	logger.Interface
	Queries []string
}

func (l *o6Log) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.Queries = append(l.Queries, s)
}
func o6Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func o6Read(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func o6Write(p string, v any, replace bool) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if !replace {
		if _, e := os.Stat(p); e == nil {
			return errors.New("immutable path exists")
		}
	}
	f, e := os.OpenFile(p+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(p+".tmp", p); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

type o6Opening struct {
	Address                                  string
	Day                                      time.Time
	Holdings                                 flowHoldings
	Known                                    bool
	Error                                    string
	SourcePath, SourceSHA256, OriginalSource string
	IndependentlyDecoded                     bool
}
type o6Request struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Request struct {
		Address string    `json:"address"`
		Day     time.Time `json:"day"`
	} `json:"request"`
}
type o6Controls struct {
	Readonly    string `json:"readonly"`
	AssignedXID *int64 `json:"assigned_xid"`
	Snapshot    string `json:"snapshot"`
	Database    string `json:"database"`
}
type o6Proof struct {
	Source     string            `json:"source"`
	Controls   o6Controls        `json:"controls"`
	Address    string            `json:"address"`
	Day        time.Time         `json:"day"`
	Holdings   flowHoldings      `json:"holdings"`
	Known      bool              `json:"known"`
	Error      string            `json:"error"`
	Queries    []string          `json:"queries"`
	CodeHashes map[string]string `json:"code_sha256"`
}

func o6Validate(p o6Proof, r o6Request) error {
	if p.Source != o6Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Snapshot == "" {
		return errors.New("source/read controls rejected")
	}
	if p.Address != r.Request.Address || !p.Day.Equal(r.Request.Day) || !p.Known || p.Error != "<nil>" {
		return errors.New("exact opening identity rejected")
	}
	if p.Holdings.Source.Kind != "ledger_exact" || p.Holdings.Source.Height <= 0 {
		return errors.New("exact ledger provenance rejected")
	}
	if p.Holdings.Liquid.IsNegative() {
		return errors.New("negative opening rejected")
	}
	for _, m := range []map[string]decimal.Decimal{p.Holdings.Staked, p.Holdings.Unbonding} {
		for _, v := range m {
			if v.IsNegative() {
				return errors.New("negative stake rejected")
			}
		}
	}
	return nil
}
func o6Publish(r o6Request, p o6Proof, n int) error {
	if e := o6Validate(p, r); e != nil {
		return e
	}
	proofPath := filepath.Join(o6Dir, fmt.Sprintf("opening-%06d.proof.private.json", n))
	if e := o6Write(proofPath, p, false); e != nil {
		return e
	}
	pb, e := os.ReadFile(proofPath)
	if e != nil {
		return e
	}
	bindPath := filepath.Join(o6Root, "source-evidence.private.json")
	before, e := os.ReadFile(bindPath)
	if e != nil {
		return e
	}
	var bind map[string]any
	if e = json.Unmarshal(before, &bind); e != nil {
		return e
	}
	rp, ok := bind["raw_path"].(string)
	if !ok {
		return errors.New("prior binding path invalid")
	}
	raw, e := os.ReadFile(rp)
	if e != nil {
		return e
	}
	if bind["source"] != o6Source || bind["raw_sha256"] != o6Hash(raw) {
		return errors.New("prior proof hash rejected")
	}
	var cumulative map[string]any
	if e = json.Unmarshal(raw, &cumulative); e != nil {
		return e
	}
	if cumulative["source"] != o6Source || cumulative["readonly"] != "on" || cumulative["assigned_xid"] != nil {
		return errors.New("prior source controls rejected")
	}
	channels, ok := cumulative["channels"].([]any)
	if !ok || len(channels) < 44 {
		return errors.New("prior channel evidence missing")
	}
	entries, _ := cumulative["openings"].([]any)
	for _, v := range entries {
		x := v.(map[string]any)
		if x["Address"] == r.Request.Address && x["Day"] == r.Request.Day.Format(time.RFC3339Nano) {
			return errors.New("opening already supplied")
		}
	}
	entry := o6Opening{Address: p.Address, Day: p.Day, Holdings: p.Holdings, Known: true, Error: "<nil>", SourcePath: proofPath, SourceSHA256: o6Hash(pb), OriginalSource: o6Source, IndependentlyDecoded: true}
	cumulative["openings"] = append(entries, entry)
	refs, _ := cumulative["external_proofs"].([]any)
	cumulative["external_proofs"] = append(refs, map[string]any{"path": rp, "sha256": o6Hash(raw)}, map[string]any{"path": proofPath, "sha256": o6Hash(pb)})
	cumulative["readonly"] = p.Controls.Readonly
	cumulative["assigned_xid"] = p.Controls.AssignedXID
	rawPath := filepath.Join(o6Dir, fmt.Sprintf("cumulative-%06d.private.json", n))
	if e = o6Write(rawPath, cumulative, false); e != nil {
		return e
	}
	newraw, e := os.ReadFile(rawPath)
	if e != nil {
		return e
	}
	now, e := os.ReadFile(bindPath)
	if e != nil || !bytes.Equal(now, before) {
		return errors.New("reply changed concurrently; no replacement")
	}
	bind["raw_path"] = rawPath
	bind["raw_sha256"] = o6Hash(newraw)
	if e = o6Write(bindPath, bind, true); e != nil {
		return e
	}
	fmt.Printf("phase opening_supplied count %d proof_sha256 %s\n", n, o6Hash(pb))
	return nil
}
func o6Capture(db *gorm.DB, r o6Request, n int) (ret error) {
	log := &o6Log{Interface: logger.Default.LogMode(logger.Silent)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Session(&gorm.Session{Logger: log}).Begin(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	defer func() {
		if ret != nil {
			_ = o6Write(filepath.Join(o6Dir, fmt.Sprintf("failed-%06d.private.json", n)), map[string]any{"error": ret.Error(), "request": r, "queries": log.Queries}, false)
		}
	}()
	for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL max_parallel_workers_per_gather=0"} {
		if e := tx.Exec(q).Error; e != nil {
			return e
		}
	}
	var controls o6Controls
	control := func() error {
		if e := tx.Raw("SELECT current_setting('transaction_read_only') AS readonly,txid_current_if_assigned() AS assigned_x_id,txid_current_snapshot()::text AS snapshot,current_database() AS database").Scan(&controls).Error; e != nil {
			return e
		}
		if controls.Readonly != "on" || controls.AssignedXID != nil {
			return errors.New("readonly snapshot rejected")
		}
		return nil
	}
	if e := control(); e != nil {
		return e
	}
	// No Server constructor, migration or background startup. Price metadata is
	// irrelevant to holdings and disabled; production HistoricalBalances runs.
	oracle := NewOracle("", "")
	oracle.denoms[apiMainnet] = map[string]DenomMeta{DefaultNativeAsset.Denom: {Symbol: DefaultNativeAsset.Symbol, Decimals: DefaultNativeAsset.Decimals}}
	oracle.at[apiMainnet] = time.Now()
	s := &Server{db: tx, oracle: oracle, native: DefaultNativeAsset, readOnlyLedger: true, privateReadSnapshotTimeout: 180 * time.Second}
	l := &dbFlowLoader{s: s, chain: apiMainnet, private: true, mu: &sync.Mutex{}}
	holdings, known, e := l.opening(r.Request.Address, r.Request.Day)
	if e != nil {
		return e
	}
	if e = control(); e != nil {
		return e
	}
	hashes := map[string]string{}
	for _, name := range []string{"commissionflows.go", "balancehistory.go", "balancedays.go", "server.go", "zz_ops818_cohort06_opening_supplier_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = o6Hash(b)
	}
	p := o6Proof{Source: o6Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Holdings: holdings, Known: known, Error: "<nil>", Queries: log.Queries, CodeHashes: hashes}
	return o6Publish(r, p, n)
}
func TestOPS818Cohort06OpeningSupplier(t *testing.T) {
	if o6Dir == "" {
		t.Skip("private run only")
	}
	syscall.Umask(0077)
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != o6Source {
		t.Fatal("source mismatch")
	}
	cmd := exec.Command("git", "diff", "--quiet", o6Source, "--", "taxapi", ":(exclude)taxapi/zz_*")
	if cmd.Run() != nil {
		t.Fatal("production source changed")
	}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-opening06 default_transaction_read_only=on"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("read-only transport failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("pool failed")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	seen := map[string]bool{}
	n := 0
	for {
		var stat syscall.Statfs_t
		if syscall.Statfs(o6Dir, &stat) != nil || uint64(stat.Bavail)*uint64(stat.Bsize) < 4*1024*1024*1024 {
			t.Fatal("reserve stop")
		}
		b, e := os.ReadFile(filepath.Join(o6Root, "extra-bank-request.private.jsonl"))
		if e != nil {
			t.Fatal("request unavailable")
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var r o6Request
			if json.Unmarshal(line, &r) != nil {
				t.Fatal("malformed request")
			}
			if r.Kind != "retained-opening" {
				continue
			}
			if r.Source != o6Source || r.Request.Address == "" || r.Request.Day.IsZero() {
				t.Fatal("request identity rejected")
			}
			key := o6Hash([]byte(r.Source + "/" + r.Request.Address + "/" + r.Request.Day.Format(time.RFC3339Nano)))
			if seen[key] {
				continue
			}
			n++
			if e = o6Capture(db, r, n); e != nil {
				t.Fatal("opening capture failed explicitly; private failure retained")
			}
			seen[key] = true
		}
		time.Sleep(time.Second)
	}
}
