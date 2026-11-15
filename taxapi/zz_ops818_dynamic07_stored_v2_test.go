//go:build ops818dynamic07openingv2

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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const v8Source = "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const v8Root = "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/validator-07"

var v8MissingStored = errors.New("exact stored opening absent; no ledger fallback")

var v8Dir = os.Getenv("OPS818_DYNAMIC07_STORED_ROOT")

type v8Log struct {
	logger.Interface
	Queries []string
}

func (l *v8Log) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.Queries = append(l.Queries, s)
}
func v8Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func v8Read(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func v8Write(p string, v any, replace bool) error {
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

type v8Opening struct {
	Address                                  string
	Day                                      time.Time
	Holdings                                 flowHoldings
	Known                                    bool
	Error                                    string
	SourcePath, SourceSHA256, OriginalSource string
	IndependentlyDecoded                     bool
}
type v8Request struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Request struct {
		Address string    `json:"address"`
		Day     time.Time `json:"day"`
	} `json:"request"`
}
type v8Controls struct {
	Readonly    string `json:"readonly"`
	AssignedXID *int64 `json:"assigned_xid"`
	Snapshot    string `json:"snapshot"`
	Database    string `json:"database"`
}
type v8Proof struct {
	StoredBuild *BalanceBuild `json:"stored_build"`
	StoredRow   BalanceDay    `json:"stored_row"`
	Boundary    heightTime    `json:"day_boundary"`

	Source     string            `json:"source"`
	Controls   v8Controls        `json:"controls"`
	Address    string            `json:"address"`
	Day        time.Time         `json:"day"`
	Holdings   flowHoldings      `json:"holdings"`
	Known      bool              `json:"known"`
	Error      string            `json:"error"`
	Queries    []string          `json:"queries"`
	CodeHashes map[string]string `json:"code_sha256"`
}

func v8Validate(p v8Proof, r v8Request) error {
	if p.Source != v8Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Snapshot == "" || p.Controls.Database != "cosmostax" {
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
func v8Publish(r v8Request, p v8Proof, n int) error {
	lock, e := os.OpenFile(filepath.Join(v8Root, "shared07-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if e := v8Validate(p, r); e != nil {
		return e
	}
	proofPath := filepath.Join(v8Dir, fmt.Sprintf("opening-%06d.proof.private.json", n))
	if e := v8Write(proofPath, p, false); e != nil {
		return e
	}
	pb, e := os.ReadFile(proofPath)
	if e != nil {
		return e
	}
	bindPath := filepath.Join(v8Root, "source-evidence.private.json")
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
	if bind["source"] != v8Source || bind["raw_sha256"] != v8Hash(raw) {
		return errors.New("prior proof hash rejected")
	}
	var cumulative map[string]any
	if e = json.Unmarshal(raw, &cumulative); e != nil {
		return e
	}
	if cumulative["source"] != v8Source || cumulative["readonly"] != "on" || cumulative["assigned_xid"] != nil {
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
	entry := v8Opening{Address: p.Address, Day: p.Day, Holdings: p.Holdings, Known: true, Error: "<nil>", SourcePath: proofPath, SourceSHA256: v8Hash(pb), OriginalSource: v8Source, IndependentlyDecoded: true}
	cumulative["openings"] = append(entries, entry)
	refs, _ := cumulative["external_proofs"].([]any)
	cumulative["external_proofs"] = append(refs, map[string]any{"path": rp, "sha256": v8Hash(raw)}, map[string]any{"path": proofPath, "sha256": v8Hash(pb)})
	cumulative["readonly"] = p.Controls.Readonly
	cumulative["assigned_xid"] = p.Controls.AssignedXID
	rawPath := filepath.Join(v8Dir, fmt.Sprintf("cumulative-%06d.private.json", n))
	if e = v8Write(rawPath, cumulative, false); e != nil {
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
	bind["raw_sha256"] = v8Hash(newraw)
	if e = v8Write(bindPath, bind, true); e != nil {
		return e
	}
	fmt.Printf("phase opening_supplied count %d proof_sha256 %s\n", n, v8Hash(pb))
	return nil
}
func v8Capture(db *gorm.DB, r v8Request, n int) (ret error) {
	log := &v8Log{Interface: logger.Default.LogMode(logger.Silent)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Session(&gorm.Session{Logger: log}).Begin(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	defer func() {
		if ret != nil && !errors.Is(ret, v8MissingStored) {
			if writeErr := v8Write(filepath.Join(v8Dir, fmt.Sprintf("failed-%06d.private.json", n)), map[string]any{"error": ret.Error(), "request": r, "queries": log.Queries}, false); writeErr != nil {
				ret = fmt.Errorf("private failure evidence could not be finalized: %w", writeErr)
			}
		}
	}()
	for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL max_parallel_workers_per_gather=0"} {
		if e := tx.Exec(q).Error; e != nil {
			return e
		}
	}
	var controls v8Controls
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
		proof := map[string]any{"source": v8Source, "controls": controls, "request": r, "balance_build": build, "stored_balance_day": row, "queries": log.Queries, "phase": "exact-stored-opening-unavailable-no-ledger-fallback", "native_event_queries": 0, "bank_capture_queries": 0, "financially_accepted": false}
		for _, q := range log.Queries {
			if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
				return errors.New("unexpected native query in stored preflight")
			}
		}
		path := filepath.Join(v8Dir, fmt.Sprintf("missing-%06d.proof.private.json", n))
		if err = v8Write(path, proof, false); err != nil {
			return err
		}
		fmt.Printf("phase exact_stored_opening_missing count %d\n", n)
		return v8MissingStored
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
	for _, name := range []string{"commissionflows.go", "balancehistory.go", "balancedays.go", "server.go", "zz_ops818_dynamic07_stored_v2_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = v8Hash(b)
	}
	p := v8Proof{StoredBuild: build, StoredRow: row, Boundary: boundary, Source: v8Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Holdings: holdings, Known: known, Error: "<nil>", Queries: log.Queries, CodeHashes: hashes}
	return v8Publish(r, p, n)
}
