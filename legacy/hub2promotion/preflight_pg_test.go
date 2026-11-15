package hub2promotion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPGApprovedPreflightReadOnlyExactNoBaselineAdoption(t *testing.T) {
	f, _ := rawFixture(t, false)
	before := f.full(t)
	expected := cloneManifest(f.m)
	report, e := preflightTarget(context.Background(), f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.True(t, report.TargetEmpty)
	require.Equal(t, f.m.Destination, report.Destination)
	require.Equal(t, f.m.Protected, report.Protected)
	require.Equal(t, 0, report.ProductionRowsWritten)
	require.Equal(t, before, f.full(t))
	require.Equal(t, expected, f.m)
	// A known new scope changes the catalog, not data. The report observes the
	// actual new identity, but never changes the caller's pinned manifest.
	id := randomID()
	s := Scope{Schema: "h2p_s_" + id, Loader: "h2p_l_" + id, ViewOwner: "h2p_v_" + id, LockOwner: "h2p_k_" + id, ChainID: "cosmoshub-2"}
	require.NoError(t, InstallScope(context.Background(), f.owner, s))
	report, e = preflightTarget(context.Background(), f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.NotEqual(t, f.m.Destination.CatalogSHA, report.Destination.CatalogSHA)
	require.Equal(t, expected, f.m)
	require.Equal(t, before, f.full(t))
	wrong := cloneManifest(f.m)
	fp := wrong.Protected["blocks"]
	fp.Rows++
	wrong.Protected["blocks"] = fp
	_, e = preflightTarget(context.Background(), f.owner, wrong, f.runtime)
	require.ErrorContains(t, e, "no automatic baseline adoption")
	wrong = cloneManifest(f.m)
	wrong.Destination.OID++
	_, e = preflightTarget(context.Background(), f.owner, wrong, f.runtime)
	require.ErrorContains(t, e, "physical server mismatch")
	// The exported live preflight never takes a private/synthetic manifest.
	_, e = PreflightApprovedTarget(context.Background(), nil, f.m, f.runtime)
	require.Error(t, e)
}

func TestPGApprovedPreflightRefusesExistingRawStage(t *testing.T) {
	f, _ := rawFixture(t, false)
	_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	before := f.full(t)
	_, e = preflightTarget(context.Background(), f.owner, f.m, f.runtime)
	require.ErrorContains(t, e, "never adopted")
	require.Equal(t, before, f.full(t))
}
