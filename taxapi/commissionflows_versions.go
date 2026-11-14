package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CommissionFlowVersion is an immutable result and its provenance. Separate
// storage leaves all prior published runs and flow rows untouched while a new
// build is being proved. Serving endpoints do not read this table implicitly.
type CommissionFlowVersion struct {
	Version      string `gorm:"primaryKey"`
	Chain        string `gorm:"primaryKey"`
	Validator    string `gorm:"primaryKey"`
	Build        string `gorm:"not null"`
	ComputedAt   time.Time
	RunJSON      string `gorm:"type:jsonb;not null"`
	RowsJSON     string `gorm:"type:jsonb;not null"`
	EvidenceJSON string `gorm:"type:jsonb;not null"`
}

func (CommissionFlowVersion) TableName() string { return "tax_commission_flow_versions" }

// CommissionFlowGeneration prevents slices with mismatched builds or windows
// from being accepted under the same version name.
type CommissionFlowGeneration struct {
	Version string    `gorm:"primaryKey"`
	Chain   string    `gorm:"primaryKey"`
	Build   string    `gorm:"not null"`
	FromDay time.Time `gorm:"type:date;not null"`
	AsOf    time.Time `gorm:"type:date;not null"`
}

func (CommissionFlowGeneration) TableName() string { return "tax_commission_flow_generations" }

// CommissionFlowBuildOptions pins a whole trace generation to one build and
// one exclusive end date. Versioned saves only INSERT, never UPDATE or DELETE.
type CommissionFlowBuildOptions struct {
	Version    string
	Build      string
	AsOf       time.Time
	ArchiveRPC string
	// ReadSnapshotTimeout overrides only private versioned reads. Zero keeps 30s.
	ReadSnapshotTimeout time.Duration
}

func (o CommissionFlowBuildOptions) validate() error {
	if o.ReadSnapshotTimeout < 0 {
		return fmt.Errorf("commission read snapshot timeout must not be negative")
	}
	if strings.TrimSpace(o.Version) == "" || strings.TrimSpace(o.Build) == "" || o.AsOf.IsZero() {
		return fmt.Errorf("versioned commission output requires version, build and exclusive as-of date")
	}
	if !o.AsOf.Equal(flowDay(o.AsOf)) {
		return fmt.Errorf("commission as-of must be midnight UTC")
	}
	return nil
}

// CommissionFlowHoldingSource identifies the exact holding read. Archive hashes
// link to retained response bytes for this wallet at the recorded height.
type CommissionFlowHoldingSource struct {
	Kind          string
	Height        int64
	ReceiptHashes []string `json:",omitempty"`
}

// CommissionFlowWalletEvidence preserves the closing comparison for each
// private traced wallet so source gaps can be investigated without retracing.
// These records never become serving balances or repair the source ledger.
type CommissionFlowWalletEvidence struct {
	Address             string
	Hop                 int
	Since               time.Time
	OpeningKnown        bool
	OpeningSource       CommissionFlowHoldingSource
	ClosingSource       CommissionFlowHoldingSource
	ClosingDay          time.Time
	ActualLiquidBase    string
	ActualStakedBase    map[string]string
	ActualUnbondingBase map[string]string
	SimulatedBase       string
	DriftBase           string
	Overdraw            map[string]string
}

func commissionFlowWalletEvidence(w *flowWallet, actual flowHoldings, day time.Time) CommissionFlowWalletEvidence {
	e := CommissionFlowWalletEvidence{
		Address: w.Addr, Hop: w.Hop, Since: w.Since, OpeningKnown: w.Known,
		ClosingDay: day, ActualLiquidBase: actual.Liquid.String(),
		ActualStakedBase: map[string]string{}, ActualUnbondingBase: map[string]string{},
		SimulatedBase: w.simulated().String(), DriftBase: w.simulated().Sub(actual.total()).String(),
		Overdraw: map[string]string{},
	}
	for k, v := range actual.Staked {
		e.ActualStakedBase[k] = v.String()
	}
	for k, v := range actual.Unbonding {
		e.ActualUnbondingBase[k] = v.String()
	}
	for r, name := range flowRuleNames {
		e.Overdraw[name] = w.Overdraw[r].String()
	}
	return e
}

// CommissionFlowReadPrefix records discovery order, not completed financial proof.
type CommissionFlowReadPrefix struct {
	Address string
	Since   time.Time
	Hop     int
}

type CommissionFlowEvidence struct {
	ReadFailure *CommissionReadFailure         `json:",omitempty"`
	ReadPrefix  []CommissionFlowReadPrefix     `json:",omitempty"`
	Wallets     []CommissionFlowWalletEvidence `json:",omitempty"`
	Labels      []AddressLabel
	Channels    map[string]string
	Archive     []FlowArchiveReceipt
}

func (e *CommissionFlowEvidence) retainReadFailure(err error, order []*flowWallet) {
	if !errors.As(err, &e.ReadFailure) {
		return
	}
	for _, w := range order {
		e.ReadPrefix = append(e.ReadPrefix, CommissionFlowReadPrefix{Address: w.Addr, Since: w.Since, Hop: w.Hop})
	}
}

func (l *dbFlowLoader) evidence() CommissionFlowEvidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := CommissionFlowEvidence{Channels: map[string]string{}}
	for _, label := range l.labels {
		e.Labels = append(e.Labels, label)
	}
	for key, chain := range l.chans {
		e.Channels[key] = chain
	}
	sort.Slice(e.Labels, func(i, j int) bool { return e.Labels[i].Address < e.Labels[j].Address })
	return e
}

func (s *Server) saveCommissionFlowVersion(o CommissionFlowBuildOptions, run CommissionFlowRun, rows []CommissionFlow, evidence ...CommissionFlowEvidence) error {
	if err := o.validate(); err != nil {
		return err
	}
	if !run.AsOf.Equal(o.AsOf.AddDate(0, 0, -1)) {
		return fmt.Errorf("commission result date differs from version date")
	}
	if rows == nil {
		rows = []CommissionFlow{}
	}
	r, err := json.Marshal(run)
	if err != nil {
		return err
	}
	f, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	e := CommissionFlowEvidence{}
	if len(evidence) == 1 {
		e = evidence[0]
	}
	if len(evidence) > 1 {
		return fmt.Errorf("only one evidence set is allowed")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	snapshot := CommissionFlowVersion{Version: o.Version, Chain: run.Chain, Validator: run.Validator, Build: o.Build, ComputedAt: run.ComputedAt, RunJSON: string(r), RowsJSON: string(f), EvidenceJSON: string(b)}
	// Create is intentionally not an upsert: even a retry must not change
	// evidence already stored under this version and validator.
	return s.db.Transaction(func(tx *gorm.DB) error {
		generation := CommissionFlowGeneration{Version: o.Version, Chain: run.Chain, Build: o.Build, FromDay: run.FromDay, AsOf: run.AsOf}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&generation).Error; err != nil {
			return err
		}
		var stored CommissionFlowGeneration
		if err := tx.Where("version = ? AND chain = ?", o.Version, run.Chain).First(&stored).Error; err != nil {
			return err
		}
		if stored.Build != generation.Build || !stored.FromDay.Equal(generation.FromDay) || !stored.AsOf.Equal(generation.AsOf) {
			return fmt.Errorf("commission output version already belongs to a different build or window")
		}
		return tx.Create(&snapshot).Error
	})
}

// reuseCommissionFlowVersion makes interrupted batches resumable without
// replacing a snapshot. Failed snapshots require a new version, not an upsert.
func (s *Server) reuseCommissionFlowVersion(o CommissionFlowBuildOptions, chain, validator string, from time.Time) (bool, error) {
	var snapshot CommissionFlowVersion
	err := s.db.Where("version = ? AND chain = ? AND validator = ?", o.Version, chain, validator).Take(&snapshot).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := reusableCommissionFlowVersion(snapshot, o, chain, validator, from); err != nil {
		return false, err
	}
	return true, nil
}

func reusableCommissionFlowVersion(snapshot CommissionFlowVersion, o CommissionFlowBuildOptions, chain, validator string, from time.Time) error {
	var run CommissionFlowRun
	if err := json.Unmarshal([]byte(snapshot.RunJSON), &run); err != nil {
		return fmt.Errorf("invalid immutable commission result")
	}
	if snapshot.Build != o.Build || snapshot.Version != o.Version || snapshot.Chain != chain || snapshot.Validator != validator || run.Chain != chain || run.Validator != validator || !run.FromDay.Equal(from) || !run.AsOf.Equal(o.AsOf.AddDate(0, 0, -1)) {
		return fmt.Errorf("immutable commission result has a different build or window")
	}
	switch run.Status {
	case flowStatusOK, flowStatusReconciled, flowStatusDrift:
		return nil
	default:
		return fmt.Errorf("immutable failed commission result requires a new output version")
	}
}

func CommissionFlowVersionsMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&CommissionFlowGeneration{}, &CommissionFlowVersion{})
}
