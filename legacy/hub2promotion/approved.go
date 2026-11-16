package hub2promotion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const ApprovedProfile = "approved-hub1-hub2-atomic-v1"
const ApprovalQuote = "yes, load hub one hub two and production when their balance checks pass"

// Accepted original Hub2 genesis reused by the retained 1.4M SQL proof.
// This is the same actual file consumed by the existing genesis-prefix owner.
const acceptedHub2GenesisSHA = "1e349fb39b85f7707ee78d39879f9d5d61f4d30f67980bb0bf07bd35b2f8bf30"

// ApprovedLoad records the already-given human authority and the actual two
// files being compared. It is not a request for another approval or signature.
type ApprovedLoad struct {
	ApprovalQuote   string `json:"approval_quote"`
	ApprovalSource  string `json:"approval_source"`
	CanonicalExport Pin    `json:"canonical_export"`
	NextGenesis     Pin    `json:"next_genesis"`
	MigratedExport  *Pin   `json:"migrated_export,omitempty"`
	MigrationBinary *Pin   `json:"migration_binary,omitempty"`
}

func (m Manifest) scanBudget() bool {
	return m.Profile == PrivateScaleProfile || m.Profile == ApprovedProfile || m.Profile == ApprovedRawProfile || m.Profile == PrivateRawProfile
}

func boundaries(chain string) (canonical, last int64, next string, err error) {
	switch chain {
	case "cosmoshub-1":
		return 500000, 500015, "cosmoshub-2", nil
	case "cosmoshub-2":
		return 2902000, 2902002, "cosmoshub-3", nil
	default:
		return 0, 0, "", errors.New("only approved Hub1/Hub2 may load")
	}
}

func (m Manifest) ValidateApproved() error {
	if m.Profile == ApprovedRawProfile || m.Profile == PrivateRawProfile {
		return m.validateApprovedRaw()
	}
	if m.Scope.RawStage || m.RawHistory != nil {
		return errors.New("raw evidence cannot satisfy complete financial profile")
	}
	if m.Fixture || m.Profile != ApprovedProfile || m.Approved == nil {
		return errors.New("approved complete load required")
	}
	a := m.Approved
	if a.ApprovalQuote != ApprovalQuote || a.ApprovalSource == "" {
		return errors.New("record actual Nov15 15:49 human approval")
	}
	canonical, last, next, e := boundaries(m.Scope.chain())
	if e != nil {
		return e
	}
	if m.First != 1 || m.Last != last || m.Canonical != canonical || m.MigrationLast != canonical || m.TailFirst != canonical+1 || m.TailLast != last {
		return errors.New("canonical export and actual historical tail must remain distinct")
	}
	if m.Destination.Database != "cosmostax" {
		return errors.New("approved live database must be cosmostax")
	}
	if e = m.validate(false); e != nil {
		return e
	}
	// Reuse the source owner's real final financial acceptance. A component or
	// merely a replay-height status cannot stand in for complete financial proof.
	b, e := readPinnedBytes(m.Financial, 16<<20)
	if e != nil {
		return e
	}
	v, e := decodeUnique(b)
	if e != nil {
		return e
	}
	r, ok := v.(map[string]any)
	if !ok {
		return errors.New("financial acceptance object required")
	}
	integer := func(name string, want int64) bool {
		n, ok := r[name].(json.Number)
		if !ok {
			return false
		}
		i, e := n.Int64()
		return e == nil && i == want
	}
	if r["full_chain_books_tax_acceptance"] != true || r["source_preserved"] != true || !integer("first", 1) || !integer("last", last) || !integer("held", 0) || r["canonical_export_sha256"] != a.CanonicalExport.SHA256 || r["projections_sha"] != valueSHA(m.Tables) || r["genesis_sha"] != m.Genesis.SHA256 || r["source_sha"] != m.Source.SHA256 || r["components_sha"] != valueSHA(m.Components) || r["build_sha"] != m.BuildSHA || r["binary_sha"] != m.BinarySHA || r["next_genesis_sha"] != a.NextGenesis.SHA256 || !integer("canonical_rows", m.CanonicalRows) || r["canonical_projection_sha"] != m.CanonicalProjectionSHA {
		return errors.New("actual complete source financial acceptance missing or not bound to canonical export")
	}
	_, exportRoot, e := readAllAccountCoins(a.CanonicalExport)
	if e != nil {
		return e
	}
	h, ok := exportRoot["source_height"].(json.Number)
	if !ok {
		return errors.New("native canonical export source height missing")
	}
	height, e := h.Int64()
	if e != nil || height != canonical || exportRoot["for_zero_height"] != true {
		return errors.New("canonical payout export required, not normal or historical tail export")
	}
	_, genesisRoot, e := readAllAccountCoins(a.NextGenesis)
	if e != nil {
		return e
	}
	if genesisRoot["chain_id"] != next {
		return errors.New("wrong next-chain starting genesis")
	}
	if m.Scope.chain() == "cosmoshub-1" && a.NextGenesis.SHA256 != acceptedHub2GenesisSHA {
		return errors.New("canonical comparison must use accepted original Hub2 genesis")
	}
	if m.Scope.chain() == "cosmoshub-2" && a.NextGenesis.SHA256 != m.Hub3GenesisSHA {
		return errors.New("canonical comparison must use retained Hub3 genesis")
	}
	comparisonExport := a.CanonicalExport
	if m.Scope.chain() == "cosmoshub-2" {
		if a.MigratedExport == nil || a.MigrationBinary == nil || r["migrated_export_sha"] != a.MigratedExport.SHA256 || r["migration_binary_sha"] != a.MigrationBinary.SHA256 {
			return errors.New("actual official SDK036 migration bindings required")
		}
		if e = hashFile(*a.MigrationBinary); e != nil {
			return e
		}
		if _, _, e = readAllAccountCoins(*a.MigratedExport); e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, a.MigrationBinary.Path, "-input", a.CanonicalExport.Path, "-sha256", a.CanonicalExport.SHA256, "-check-output", a.MigratedExport.Path, "-check-sha256", a.MigratedExport.SHA256)
		cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=512MiB"}
		e = cmd.Run()
		cancel()
		if e != nil {
			return errors.New("official SDK034-to-036 migration recomputation refused")
		}
		comparisonExport = *a.MigratedExport
	} else if a.MigratedExport != nil || a.MigrationBinary != nil {
		return errors.New("Hub1 must not apply Hub2 SDK036 migration")
	}
	comparison, e := CompareAllAccounts(comparisonExport, a.NextGenesis)
	if e != nil {
		return e
	}
	if !comparison.Pass {
		return fmt.Errorf("all-account/all-coin mismatch: checked=%d missing=%d extra=%d denominations=%d", comparison.AccountsChecked, comparison.Missing, comparison.Extra, comparison.MismatchedDenominations)
	}
	if m.CanonicalRows < 1 {
		return errors.New("accepted final payout must occur exactly once")
	}
	return m.verifyOfflineProjection()
}

// MergeApproved is the single production entry point for both static scopes.
// Complete source and every-account equality are checked before a connection
// is acquired and again before the one transaction commits.
func MergeApproved(ctx context.Context, loader, auditor *sql.DB, m Manifest, runtime Identity) (Result, error) {
	if m.Profile == ApprovedRawProfile && !m.Fixture {
		// merge rechecks complete source before acquiring the first session and
		// again before COMMIT. Do not add redundant full58GB admission passes.
		return merge(ctx, loader, auditor, m, runtime, hooks{})
	}
	if e := m.ValidateApproved(); e != nil {
		return Result{}, e
	}
	return merge(ctx, loader, auditor, m, runtime, hooks{})
}

func verifyCommitted(ctx context.Context, auditor *sql.DB, m Manifest, runtime Identity) error {
	outcome, e := AuditOutcome(ctx, auditor, m, runtime)
	if e != nil || outcome != OutcomeExact {
		return ErrCommittedUnverified
	}
	return nil
}

// InstallApprovedScope pins the physical server before installing any new
// object. The caller coordinates its explicit execution with Auth. Existing
// objects are never replaced, and all new objects share one transaction.
func InstallApprovedScope(ctx context.Context, owner *sql.DB, m Manifest, runtime Identity) error {
	if e := m.ValidateApproved(); e != nil {
		return e
	}
	if runtime.DeploymentUID != m.Destination.DeploymentUID || runtime.PodUID != m.Destination.PodUID || runtime.PVCUID != m.Destination.PVCUID || runtime.Endpoint != m.Destination.Endpoint {
		return errors.New("scope installation runtime mismatch")
	}
	return installPinned(ctx, owner, m.Scope, m.Destination)
}

// CaptureProtected is read-only and must run again immediately before load.
// A concurrent chain load changes these pins and requires fresh admission.
func CaptureProtected(ctx context.Context, auditor *sql.DB, chain string) (Snapshot, error) {
	if _, _, _, e := boundaries(chain); e != nil {
		return nil, e
	}
	tx, e := beginAuditBudget(ctx, auditor, true)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	return snapshotChain(ctx, tx, chain, true, true)
}

// ExportApprovedProjection only reads an already-reconciled private SQL stage.
// The stage is never a serving schema and never the live cosmostax database.
func ExportApprovedProjection(ctx context.Context, source *sql.DB, expected Identity, dir, chain string) ([]Projection, error) {
	if _, _, _, e := boundaries(chain); e != nil {
		return nil, e
	}
	return exportProjectionChain(ctx, source, expected, dir, true, chain)
}
