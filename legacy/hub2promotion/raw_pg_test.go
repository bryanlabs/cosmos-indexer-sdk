package hub2promotion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"os"
)

func rawFixture(t *testing.T, populate bool) (fixture, fixture) {
	t.Helper()
	f := newFixtureForChain(t, "cosmoshub-1")
	financial := f
	financial.m = cloneManifest(f.m)
	// Some tests populate the selected chain too, protecting ALL legacy rows.
	if populate {
		for _, tb := range inserts {
			raw := canonicalRow(t, f.owner, tb, fixtureRow(tb.name, "cosmoshub-1", f.m.Genesis.SHA256))
			mustExec(t, f.owner, "INSERT INTO legacy."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
		}
		for _, name := range []string{"balance_days", "import_ranges", "reconciliation"} {
			fixtureInsert(t, f, name)
		}
	}
	id := randomID()
	s := Scope{Schema: "h2p_s_" + id, Loader: "h2p_l_" + id, ViewOwner: "h2p_v_" + id, LockOwner: "h2p_k_" + id, ChainID: "cosmoshub-1", RawStage: true}
	require.NoError(t, InstallScope(context.Background(), f.owner, s))
	mustExec(t, f.owner, "ALTER ROLE "+s.Loader+" PASSWORD 'synthetic-fixture-only'")
	c, e := pgx.ParseConfig(os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"))
	require.NoError(t, e)
	c.Database = f.m.Destination.Database
	c.User = s.Loader
	c.Password = "synthetic-fixture-only"
	loader := stdlib.OpenDB(*c)
	t.Cleanup(func() { loader.Close() })
	f.loader = loader
	f.m.Profile = RawFixtureProfile
	f.m.Scope = s
	f.m.Approved = nil
	f.m.RawHistory = nil
	f.m.CanonicalRows = 0
	f.m.CanonicalProjectionSHA = valueEmptySHA()
	var projections []Projection
	for _, tb := range s.insertTables() {
		for _, p := range f.m.Tables {
			if tb.name == p.Table {
				projections = append(projections, p)
			}
		}
	}
	f.m.Tables = projections
	tx, e := f.owner.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, e)
	f.m.Destination, e = ProbeIdentity(context.Background(), tx, s, f.runtime)
	require.NoError(t, e)
	require.NoError(t, tx.Rollback())
	f.m.Protected, e = f.m.protectedSnapshot(context.Background(), f.owner)
	require.NoError(t, e)
	require.NoError(t, f.m.Recheck())
	return f, financial
}
func TestPGRawStageAtomicPreservesEveryLegacyRow(t *testing.T) {
	f, _ := rawFixture(t, true)
	ctx := context.Background()
	before := f.full(t)
	_, e := merge(ctx, f.loader, f.owner, f.m, f.runtime, hooks{afterChunk: func(n int) error {
		if n == 2 {
			return errors.New("synthetic late failure")
		}
		return nil
	}})
	require.Error(t, e)
	require.NoError(t, f.m.emptyTarget(ctx, f.owner))
	require.Equal(t, before, f.full(t))
	result, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Positive(t, result.Rows)
	require.Equal(t, before, f.full(t))
	outcome, e := AuditOutcome(ctx, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, OutcomeExact, outcome)
	_, e = MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.Error(t, e)
	require.Equal(t, before, f.full(t))
	// The raw role cannot write any legacy relation, including its selected chain.
	for _, tb := range inserts {
		raw := canonicalRow(t, f.owner, tb, fixtureRow(tb.name, "cosmoshub-1", f.m.Genesis.SHA256))
		_, e = f.loader.Exec("INSERT INTO legacy."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
		require.Error(t, e)
	}
	// Raw data does not masquerade as full acceptance even with a copied profile.
	wrong := f.m
	wrong.Profile = ApprovedProfile
	wrong.Fixture = false
	require.EqualError(t, wrong.ValidateApproved(), "raw evidence cannot satisfy complete financial profile")
	// A separate ordinary financial scope is not blocked by the raw tables or
	// helper. This checks privilege isolation without modifying financial rows.
	id := randomID()
	ordinary := Scope{Schema: "h2p_s_" + id, Loader: "h2p_l_" + id, ViewOwner: "h2p_v_" + id, LockOwner: "h2p_k_" + id, ChainID: "cosmoshub-1"}
	require.NoError(t, InstallScope(ctx, f.owner, ordinary))
	require.NoError(t, CheckScope(ctx, f.owner, ordinary))
}
func TestPGRawStageScopeNegativesAndPhysicalAudit(t *testing.T) {
	f, _ := rawFixture(t, true)
	ctx := context.Background()
	for _, tb := range f.m.Scope.insertTables() {
		raw := canonicalRow(t, f.owner, tb, fixtureRow(tb.name, "cosmoshub-2", f.m.Genesis.SHA256))
		_, e := f.loader.Exec("INSERT INTO "+f.m.Scope.Schema+"."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
		require.Error(t, e)
		_, e = f.loader.Exec("INSERT INTO "+f.m.Scope.baseRelation(tb)+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
		require.Error(t, e)
	}
	_, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	for _, tb := range f.m.Scope.insertTables() {
		_, e = f.owner.Exec("UPDATE " + f.m.Scope.baseRelation(tb) + " SET height=height")
		require.Error(t, e)
		_, e = f.owner.Exec("TRUNCATE " + f.m.Scope.baseRelation(tb) + " CASCADE")
		require.Error(t, e)
	}
	mustExec(t, f.owner, "UPDATE legacy.balance_days SET height=height WHERE chain_id='cosmoshub-1'")
	outcome, e := AuditOutcome(ctx, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, OutcomeConflict, outcome)
}
func TestPGRawStageFollowedByFullFinancialImportWithoutOverwrite(t *testing.T) {
	raw, financial := rawFixture(t, false)
	ctx := context.Background()
	_, e := MergeFixture(ctx, raw.loader, raw.owner, raw.m, raw.runtime)
	require.NoError(t, e)
	before := Snapshot{}
	for _, tb := range raw.m.Scope.insertTables() {
		before[tb.name], e = fingerprintRelation(ctx, raw.owner, tb, raw.m.Scope.baseRelation(tb), "true", true)
		require.NoError(t, e)
	}
	// Scope installation changes real catalog ACLs. Re-probe only after the
	// known authorized installation, never ignore a catalog mismatch at load.
	refreshIdentity(t, &financial)
	require.NoError(t, financial.m.ValidateFixture())
	_, e = MergeFixture(ctx, financial.loader, financial.owner, financial.m, financial.runtime)
	require.NoError(t, e)
	require.NoError(t, verifyProjection(ctx, financial.owner, financial.m))
	for _, tb := range raw.m.Scope.insertTables() {
		after, e := fingerprintRelation(ctx, raw.owner, tb, raw.m.Scope.baseRelation(tb), "true", true)
		require.NoError(t, e)
		require.Equal(t, before[tb.name], after)
	}
}

func TestPGRawFinancialGuardFromOtherwiseValidManifest(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.m.ValidateFixture())
	wrong := cloneManifest(f.m)
	wrong.Scope.RawStage = true
	require.EqualError(t, wrong.ValidateFixture(), "raw stage is not complete financial evidence")
	wrong = cloneManifest(f.m)
	wrong.RawHistory = &RawHistoryLoad{}
	require.EqualError(t, wrong.ValidateFixture(), "raw stage is not complete financial evidence")
}

func TestPGRawTimestampMatchesActualPostgresCast(t *testing.T) {
	f := newFixture(t)
	for _, frac := range []string{"", ".0000001", ".0000005", ".0000015", ".123456499", ".123456500", ".123457500", ".123456789", ".999999999"} {
		native := "2019-05-01T00:00:00" + frac + "Z"
		var actual time.Time
		require.NoError(t, f.owner.QueryRow("SELECT $1::pg_catalog.timestamptz", native).Scan(&actual))
		want, e := rawSQLTime(native)
		require.NoError(t, e)
		require.True(t, actual.Equal(want), "PostgreSQL timestamp normalization mismatch")
	}
}

func TestRawProfileCannotBecomeFinancialOrArbitraryChain(t *testing.T) {
	s := Scope{Schema: "h2p_s_raw", Loader: "h2p_l_raw", ViewOwner: "h2p_v_raw", LockOwner: "h2p_k_raw", ChainID: "cosmoshub-1", RawStage: true}
	text, e := s.SQL()
	require.NoError(t, e)
	require.Equal(t, 2, strings.Count(text, "CREATE VIEW"))
	require.Contains(t, text, "CREATE TABLE h2p_s_raw.raw_blocks")
	require.NotContains(t, text, "GRANT SELECT, INSERT ON legacy.")
	require.Contains(t, text, fmt.Sprintf("GRANT SELECT, INSERT ON %s.raw_blocks, %s.raw_block_events", s.Schema, s.Schema))
	for _, chain := range []string{"cosmoshub-2", "cosmoshub-3", "cosmoshub-4", "x' OR true--"} {
		wrong := s
		wrong.ChainID = chain
		_, e = wrong.SQL()
		require.Error(t, e)
	}
	for _, profile := range []string{FixtureProfile, PrivateScaleProfile, CompleteProfile, ApprovedProfile, ApprovedRawProfile} {
		m := Manifest{Scope: s, Profile: profile}
		require.Error(t, m.Recheck())
	}
}
