package hub2promotion

import (
	"context"
	"database/sql"
	"errors"
)

// PreflightReport is an actual read-only destination observation, not admission
// or a rewritten manifest. The operator checks it against the known authorized
// scope installation and retains the original expected data fingerprints.
type PreflightReport struct {
	Destination           Identity `json:"destination"`
	Protected             Snapshot `json:"protected"`
	TargetEmpty           bool     `json:"target_empty"`
	ProductionRowsWritten int      `json:"production_rows_written"`
}

func PreflightApprovedTarget(ctx context.Context, auditor *sql.DB, m Manifest, runtime Identity) (PreflightReport, error) {
	if m.Fixture || (m.Profile != ApprovedProfile && m.Profile != ApprovedRawProfile) || m.Destination.Database != "cosmostax" {
		return PreflightReport{}, errors.New("approved live target preflight required")
	}
	return preflightTarget(ctx, auditor, m, runtime)
}
func preflightTarget(ctx context.Context, auditor *sql.DB, m Manifest, runtime Identity) (PreflightReport, error) {
	if e := m.Scope.valid(); e != nil {
		return PreflightReport{}, e
	}
	if runtime.DeploymentUID != m.Destination.DeploymentUID || runtime.PodUID != m.Destination.PodUID || runtime.PVCUID != m.Destination.PVCUID || runtime.Endpoint != m.Destination.Endpoint {
		return PreflightReport{}, errors.New("preflight runtime identity mismatch")
	}
	tx, e := beginAuditBudget(ctx, auditor, m.scanBudget())
	if e != nil {
		return PreflightReport{}, e
	}
	defer tx.Rollback()
	id, e := ProbeIdentity(ctx, tx, m.Scope, runtime)
	if e != nil {
		return PreflightReport{}, e
	}
	if id.Database != m.Destination.Database || id.OID != m.Destination.OID || id.SystemHash != m.Destination.SystemHash {
		return PreflightReport{}, errors.New("preflight physical server mismatch")
	}
	if e = CheckScope(ctx, tx, m.Scope); e != nil {
		return PreflightReport{}, e
	}
	if e = erasAndGenesis(ctx, tx, m); e != nil {
		return PreflightReport{}, e
	}
	if e = m.emptyTarget(ctx, tx); e != nil {
		return PreflightReport{}, e
	}
	protected, e := m.protectedSnapshot(ctx, tx)
	if e != nil {
		return PreflightReport{}, e
	}
	if !equalSnapshot(protected, m.Protected) {
		return PreflightReport{}, errors.New("preflight protected data changed; no automatic baseline adoption")
	}
	return PreflightReport{Destination: id, Protected: protected, TargetEmpty: true}, nil
}
