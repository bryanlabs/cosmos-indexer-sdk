//go:build ops818opening06storedv3

package taxapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

const v6Source = "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const v6Root = "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/validator-06"

var v6MissingStored = errors.New("exact stored opening absent; no ledger fallback")

var v6Dir = os.Getenv("OPS818_OPENING06_STORED_V3_ROOT")

type v6Log struct {
	logger.Interface
	Queries []string
}

func (l *v6Log) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.Queries = append(l.Queries, s)
}
func v6Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func v6Read(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func v6Write(p string, v any, replace bool) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if !replace {
		if _, e := os.Stat(p); e == nil {
			return errors.New("immutable path exists")
		}
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".opening06-data-*.tmp")
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
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

type v6Opening struct {
	Address                                  string
	Day                                      time.Time
	Holdings                                 flowHoldings
	Known                                    bool
	Error                                    string
	SourcePath, SourceSHA256, OriginalSource string
	IndependentlyDecoded                     bool
}
type v6Request struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Request struct {
		Address string    `json:"address"`
		Day     time.Time `json:"day"`
	} `json:"request"`
}
type v6Controls struct {
	Readonly    string `json:"readonly"`
	AssignedXID *int64 `json:"assigned_xid"`
	Snapshot    string `json:"snapshot"`
	Database    string `json:"database"`
}
type v6Proof struct {
	StoredBuild *BalanceBuild `json:"stored_build"`
	StoredRow   BalanceDay    `json:"stored_row"`
	Boundary    heightTime    `json:"day_boundary"`

	Source     string            `json:"source"`
	Controls   v6Controls        `json:"controls"`
	Address    string            `json:"address"`
	Day        time.Time         `json:"day"`
	Holdings   flowHoldings      `json:"holdings"`
	Known      bool              `json:"known"`
	Error      string            `json:"error"`
	Queries    []string          `json:"queries"`
	CodeHashes map[string]string `json:"code_sha256"`
}

func v6Validate(p v6Proof, r v6Request) error {
	if p.Source != v6Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Snapshot == "" || p.Controls.Database != "cosmostax" {
		return errors.New("source/read controls rejected")
	}
	if p.Address != r.Request.Address || !p.Day.Equal(r.Request.Day) || !p.Known || p.Error != "<nil>" {
		return errors.New("exact opening identity rejected")
	}
	if p.Holdings.Source.Kind != "ledger_exact" || p.Holdings.Source.Height <= 0 {
		return errors.New("exact ledger provenance rejected")
	}
	if p.StoredBuild == nil || p.StoredBuild.FromDay == nil || p.StoredBuild.ToDay == nil || p.Day.Before(*p.StoredBuild.FromDay) || p.Day.After(*p.StoredBuild.ToDay) {
		return errors.New("stored span rejected")
	}
	if p.StoredRow.Chain != apiMainnet || p.StoredRow.Address != p.Address || p.StoredRow.Day.After(p.Day) || p.StoredRow.Day.Before(*p.StoredBuild.FromDay) || p.StoredRow.Status != "" || p.StoredRow.Holdings == "" {
		return errors.New("stored raw row identity rejected")
	}
	if p.Boundary.Height <= 0 || p.Holdings.Source.Height != p.Boundary.Height {
		return errors.New("stored boundary/source height rejected")
	}
	for _, q := range p.Queries {
		if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
			return errors.New("native query forbidden in stored proof")
		}
	}
	raw, e := decodeHoldings(p.Boundary.Height, p.StoredRow.Holdings)
	if e != nil {
		return e
	}
	expected := decimal.Zero
	if x := raw.Liquid[DefaultNativeAsset.Denom]; x != nil {
		expected = decimal.NewFromBigInt(x, 0)
	}
	if !expected.Equal(p.Holdings.Liquid) {
		return errors.New("stored liquid decode differs")
	}
	for _, pair := range []struct {
		raw map[string]*big.Int
		got map[string]decimal.Decimal
	}{{raw.Delegations, p.Holdings.Staked}, {raw.Unbonding, p.Holdings.Unbonding}} {
		for k, v := range pair.raw {
			if !decimal.NewFromBigInt(v, 0).Equal(pair.got[k]) {
				return errors.New("stored stake decode differs")
			}
		}
		for k, v := range pair.got {
			x := decimal.Zero
			if n := pair.raw[k]; n != nil {
				x = decimal.NewFromBigInt(n, 0)
			}
			if !v.Equal(x) {
				return errors.New("stored extra stake rejected")
			}
		}
	}
	for _, m := range []map[string]*big.Int{raw.DelegationError, raw.UnbondingError} {
		for _, v := range m {
			if v.Sign() > 0 {
				return errors.New("stored approximate stake rejected")
			}
		}
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
func v6Publish(r v6Request, p v6Proof, n int) error {
	lock, e := os.OpenFile(filepath.Join(v6Root, "opening06-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if e := v6Validate(p, r); e != nil {
		return e
	}
	proofPath := filepath.Join(v6Dir, fmt.Sprintf("opening-%06d.proof.private.json", n))
	if e := v6Write(proofPath, p, false); e != nil {
		return e
	}
	pb, e := os.ReadFile(proofPath)
	if e != nil {
		return e
	}
	bindPath := filepath.Join(v6Root, "source-evidence.private.json")
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
	if bind["source"] != v6Source || bind["raw_sha256"] != v6Hash(raw) {
		return errors.New("prior proof hash rejected")
	}
	var cumulative map[string]any
	if e = json.Unmarshal(raw, &cumulative); e != nil {
		return e
	}
	if cumulative["source"] != v6Source || cumulative["readonly"] != "on" || cumulative["assigned_xid"] != nil {
		return errors.New("prior source controls rejected")
	}
	channels, ok := cumulative["channels"].([]any)
	if !ok || len(channels) < 44 {
		return errors.New("prior channel evidence missing")
	}
	entries, _ := cumulative["openings"].([]any)
	for _, v := range entries {
		x := v.(map[string]any)
		if x["Address"] == r.Request.Address && x["Day"] == r.Request.Day.UTC().Format(time.RFC3339Nano) {
			return errors.New("opening already supplied")
		}
	}
	entry := v6Opening{Address: p.Address, Day: p.Day, Holdings: p.Holdings, Known: true, Error: "<nil>", SourcePath: proofPath, SourceSHA256: v6Hash(pb), OriginalSource: v6Source, IndependentlyDecoded: true}
	cumulative["openings"] = append(entries, entry)
	refs, _ := cumulative["external_proofs"].([]any)
	cumulative["external_proofs"] = append(refs, map[string]any{"path": rp, "sha256": v6Hash(raw)}, map[string]any{"path": proofPath, "sha256": v6Hash(pb)})
	cumulative["readonly"] = p.Controls.Readonly
	cumulative["assigned_xid"] = p.Controls.AssignedXID
	rawPath := filepath.Join(v6Dir, fmt.Sprintf("cumulative-%06d.private.json", n))
	if e = v6Write(rawPath, cumulative, false); e != nil {
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
	bind["raw_sha256"] = v6Hash(newraw)
	if e = v6Write(bindPath, bind, true); e != nil {
		return e
	}
	fmt.Printf("phase opening_supplied count %d proof_sha256 %s\n", n, v6Hash(pb))
	return nil
}
func v6Capture(db *gorm.DB, r v6Request, n int) (ret error) {
	log := &v6Log{Interface: logger.Default.LogMode(logger.Silent)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Session(&gorm.Session{Logger: log}).Begin(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	defer func() {
		if ret != nil && !errors.Is(ret, v6MissingStored) {
			if writeErr := v6Write(filepath.Join(v6Dir, fmt.Sprintf("failed-%06d.private.json", n)), map[string]any{"error": ret.Error(), "request": r, "queries": log.Queries}, false); writeErr != nil {
				ret = fmt.Errorf("private failure evidence could not be finalized: %w", writeErr)
			}
		}
	}()
	for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL max_parallel_workers_per_gather=0"} {
		if e := tx.Exec(q).Error; e != nil {
			return e
		}
	}
	var controls v6Controls
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
	s := &Server{db: tx, oracle: oracle, native: DefaultNativeAsset, readOnlyLedger: true}
	l := &dbFlowLoader{s: s, chain: apiMainnet, private: true, mu: &sync.Mutex{}}
	// The same repeatable-read snapshot must prove a usable stored path
	// before invoking HistoricalBalances. Never fall back to ledger/BANK reads.
	build, err := s.balanceBuild(apiMainnet)
	if err != nil {
		return err
	}
	row := BalanceDay{}
	usable := false
	if build != nil && build.FromDay != nil && build.ToDay != nil && !r.Request.Day.Before(*build.FromDay) && !r.Request.Day.After(*build.ToDay) {
		var found bool
		row, found, err = s.storedBalanceDay(build, r.Request.Address, r.Request.Day)
		if err != nil {
			return err
		}
		usable = found && row.Status == ""
	}
	if !usable {
		if err = control(); err != nil {
			return err
		}
		proof := map[string]any{"source": v6Source, "controls": controls, "request": r, "balance_build": build, "stored_balance_day": row, "queries": log.Queries, "phase": "exact-stored-opening-unavailable-no-ledger-fallback", "native_event_queries": 0, "bank_capture_queries": 0, "financially_accepted": false}
		for _, q := range log.Queries {
			if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
				return errors.New("unexpected native query in stored preflight")
			}
		}
		path := filepath.Join(v6Dir, fmt.Sprintf("missing-%06d.proof.private.json", n))
		if err = v6Write(path, proof, false); err != nil {
			return err
		}
		fmt.Printf("phase exact_stored_opening_missing count %d\n", n)
		return v6MissingStored
	}
	ids, err := s.chainRowIDs(apiMainnet)
	if err != nil {
		return err
	}
	boundary, err := s.dayEndBlock(ids, r.Request.Day)
	if err != nil {
		return err
	}
	if boundary.Height <= 0 {
		return errors.New("stored boundary height missing")
	}
	holdings, known, e := l.opening(r.Request.Address, r.Request.Day)
	if e != nil {
		return e
	}
	if e = control(); e != nil {
		return e
	}
	hashes := map[string]string{}
	for _, name := range []string{"commissionflows.go", "balancehistory.go", "balancedays.go", "server.go", "zz_ops818_cohort06_stored_opening_v3_supplier_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = v6Hash(b)
	}
	p := v6Proof{StoredBuild: build, StoredRow: row, Boundary: boundary, Source: v6Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Holdings: holdings, Known: known, Error: "<nil>", Queries: log.Queries, CodeHashes: hashes}
	return v6Publish(r, p, n)
}
func TestOPS818Cohort06StoredOpeningV3Supplier(t *testing.T) {
	if v6Dir == "" {
		t.Skip("private run only")
	}
	syscall.Umask(0077)
	os.Setenv("NODE_REST_API", "http://127.0.0.1:1")
	os.Setenv("NODE_RPC_API", "http://127.0.0.1:1")
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != v6Source {
		t.Fatal("source mismatch")
	}
	cmd := exec.Command("git", "diff", "--quiet", v6Source, "--", "taxapi", ":(exclude)taxapi/zz_*")
	if cmd.Run() != nil {
		t.Fatal("production source changed")
	}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-opening06-stored-v3 default_transaction_read_only=on"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	var initialBinding struct {
		Source  string `json:"source"`
		RawPath string `json:"raw_path"`
		RawSHA  string `json:"raw_sha256"`
	}
	if e = v6Read(filepath.Join(v6Root, "source-evidence.private.json"), &initialBinding); e != nil {
		t.Fatal("initial reply binding absent")
	}
	initialBytes, e := os.ReadFile(initialBinding.RawPath)
	if e != nil || initialBinding.Source != v6Source || v6Hash(initialBytes) != initialBinding.RawSHA {
		t.Fatal("initial reply hash rejected")
	}
	var initial struct {
		Openings []v6Opening `json:"openings"`
	}
	if json.Unmarshal(initialBytes, &initial) != nil {
		t.Fatal("initial openings malformed")
	}
	for _, x := range initial.Openings {
		seen[v6Hash([]byte(v6Source+"/"+x.Address+"/"+x.Day.UTC().Format(time.RFC3339Nano)))] = true
	}
	priorFiles, e := filepath.Glob(filepath.Join(v6Dir, "*.private.json"))
	if e != nil {
		t.Fatal("memo scan failed")
	}
	for _, path := range priorFiles {
		parts := strings.Split(filepath.Base(path), "-")
		if len(parts) > 1 {
			v, _ := strconv.Atoi(strings.Split(parts[1], ".")[0])
			if v > n {
				n = v
			}
		}
		if strings.HasPrefix(filepath.Base(path), "missing-") {
			var previous struct {
				Source   string     `json:"source"`
				Controls v6Controls `json:"controls"`
				Request  v6Request  `json:"request"`
			}
			if v6Read(path, &previous) != nil || previous.Source != v6Source || previous.Controls.Readonly != "on" || previous.Controls.AssignedXID != nil {
				t.Fatal("prior missing memo rejected")
			}
			r := previous.Request
			seen[v6Hash([]byte(r.Source+"/"+r.Request.Address+"/"+r.Request.Day.UTC().Format(time.RFC3339Nano)))] = true
		}
	}
	for {
		var stat syscall.Statfs_t
		if syscall.Statfs(v6Dir, &stat) != nil || uint64(stat.Bavail)*uint64(stat.Bsize) < 4*1024*1024*1024 {
			t.Fatal("reserve stop")
		}
		b, e := os.ReadFile(filepath.Join(v6Root, "extra-bank-request.private.jsonl"))
		if e != nil {
			t.Fatal("request unavailable")
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var r v6Request
			if json.Unmarshal(line, &r) != nil {
				t.Fatal("malformed request")
			}
			if r.Kind != "retained-opening" {
				continue
			}
			if r.Source != v6Source || r.Request.Address == "" || r.Request.Day.IsZero() {
				t.Fatal("request identity rejected")
			}
			key := v6Hash([]byte(r.Source + "/" + r.Request.Address + "/" + r.Request.Day.UTC().Format(time.RFC3339Nano)))
			if seen[key] {
				continue
			}
			n++
			if e = v6Capture(db, r, n); e != nil && !errors.Is(e, v6MissingStored) {
				t.Fatal("opening capture failed explicitly; private failure retained")
			}
			seen[key] = true
		}
		time.Sleep(time.Second)
	}
}
