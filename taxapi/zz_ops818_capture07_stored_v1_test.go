//go:build ops818capture07v1

package taxapi

import (
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
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const v9Source = "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const v9Root = "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/validator-07"

var v9MissingStored = errors.New("exact stored opening absent; no ledger fallback")

var v9Dir = os.Getenv("OPS818_DYNAMIC07_STORED_ROOT")

type v9Log struct {
	logger.Interface
	Queries []string
}

func (l *v9Log) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.Queries = append(l.Queries, s)
}
func v9Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func v9Read(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func v9Write(p string, v any, replace bool) error {
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

type v9Opening struct {
	Address                                  string
	Day                                      time.Time
	Holdings                                 flowHoldings
	Known                                    bool
	Error                                    string
	SourcePath, SourceSHA256, OriginalSource string
	IndependentlyDecoded                     bool
}
type v9Request struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Request struct {
		Address string    `json:"address"`
		Day     time.Time `json:"day"`
	} `json:"request"`
}
type v9Controls struct {
	Readonly    string `json:"readonly"`
	AssignedXID *int64 `json:"assigned_xid"`
	Snapshot    string `json:"snapshot"`
	Database    string `json:"database"`
}
type v9Proof struct {
	StoredBuild *BalanceBuild `json:"stored_build"`
	StoredRow   BalanceDay    `json:"stored_row"`
	Boundary    heightTime    `json:"day_boundary"`

	Source     string            `json:"source"`
	Controls   v9Controls        `json:"controls"`
	Address    string            `json:"address"`
	Day        time.Time         `json:"day"`
	Holdings   flowHoldings      `json:"holdings"`
	Known      bool              `json:"known"`
	Error      string            `json:"error"`
	Queries    []string          `json:"queries"`
	CodeHashes map[string]string `json:"code_sha256"`
}

func v9Validate(p v9Proof, r v9Request) error {
	if p.Source != v9Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Snapshot == "" || p.Controls.Database != "cosmostax" {
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
func v9Publish(r v9Request, p v9Proof, n int) error {
	if e := v9Validate(p, r); e != nil {
		return e
	}
	proof := filepath.Join(v9Dir, fmt.Sprintf("opening-%06d.proof.private.json", n))
	if e := v9Write(proof, p, false); e != nil {
		return e
	}
	fmt.Printf("phase opening_captured_only count %d\n", n)
	return nil
}
func v9Capture(db *gorm.DB, r v9Request, n int) (ret error) {
	log := &v9Log{Interface: logger.Default.LogMode(logger.Silent)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Session(&gorm.Session{Logger: log}).Begin(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	defer func() {
		if ret != nil && !errors.Is(ret, v9MissingStored) {
			if writeErr := v9Write(filepath.Join(v9Dir, fmt.Sprintf("failed-%06d.private.json", n)), map[string]any{"error": ret.Error(), "request": r, "queries": log.Queries}, false); writeErr != nil {
				ret = fmt.Errorf("private failure evidence could not be finalized: %w", writeErr)
			}
		}
	}()
	for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'", "SET LOCAL max_parallel_workers_per_gather=0"} {
		if e := tx.Exec(q).Error; e != nil {
			return e
		}
	}
	var controls v9Controls
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
		proof := map[string]any{"source": v9Source, "controls": controls, "request": r, "balance_build": build, "stored_balance_day": row, "queries": log.Queries, "phase": "exact-stored-opening-unavailable-no-ledger-fallback", "native_event_queries": 0, "bank_capture_queries": 0, "financially_accepted": false}
		for _, q := range log.Queries {
			if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
				return errors.New("unexpected native query in stored preflight")
			}
		}
		path := filepath.Join(v9Dir, fmt.Sprintf("missing-%06d.proof.private.json", n))
		if err = v9Write(path, proof, false); err != nil {
			return err
		}
		fmt.Printf("phase exact_stored_opening_missing count %d\n", n)
		return v9MissingStored
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
	for _, name := range []string{"commissionflows.go", "balancehistory.go", "balancedays.go", "server.go", "zz_ops818_capture07_stored_v1_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = v9Hash(b)
	}
	p := v9Proof{StoredBuild: build, StoredRow: row, Boundary: boundary, Source: v9Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Holdings: holdings, Known: known, Error: "<nil>", Queries: log.Queries, CodeHashes: hashes}
	if !known {
		path := filepath.Join(v9Dir, fmt.Sprintf("missing-%06d.proof.private.json", n))
		for _, q := range log.Queries {
			if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
				return errors.New("native query forbidden in strict stored refusal")
			}
		}
		if e := v9Write(path, map[string]any{"source": v9Source, "controls": controls, "request": r, "phase": "exact-stored-opening-unavailable-no-ledger-fallback", "reason": "actual-production-strict-opening-unknown", "stored_attempt": p, "financially_accepted": false}, false); e != nil {
			return e
		}
		return v9MissingStored
	}
	return v9Publish(r, p, n)
}
