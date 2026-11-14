package hub2promotion

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPG8PrivateSQLProjectionExportIsExactReadOnlyAndNoReuse(t *testing.T) {
	f := newFixture(t)
	_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	before := f.full(t)
	dir := filepath.Join(f.dir, "NEW-sql-export")
	p, e := ExportProjection(context.Background(), f.owner, f.m.Destination, dir)
	require.NoError(t, e)
	require.Len(t, p, 10)
	for i, v := range p {
		require.Equal(t, f.m.Tables[i].Table, v.Table)
		require.Equal(t, f.m.Tables[i].Columns, v.Columns)
		require.Equal(t, f.m.Tables[i].Rows, v.Rows)
		require.Equal(t, f.m.Tables[i].File.SHA256, v.File.SHA256)
	}
	_, e = ExportProjection(context.Background(), f.owner, f.m.Destination, dir)
	require.Error(t, e, "never overwrite or reuse export output")
	bad := f.m.Destination
	bad.OID++
	_, e = ExportProjection(context.Background(), f.owner, bad, filepath.Join(f.dir, "wrong-source"))
	require.Error(t, e)
	require.Equal(t, before, f.full(t))
}
func TestPrivatePrefixScaleCannotClaimCanonicalOrCompleteEvidence(t *testing.T) {
	f := newFixture(t)
	m := cloneManifest(f.m)
	m.Profile = PrivateScaleProfile
	m.Last = 1400000
	m.Canonical = 0
	m.MigrationLast = 0
	m.TailFirst = 0
	m.TailLast = 0
	m.CanonicalRows = 0
	m.CanonicalProjectionSHA = valueEmptySHA()
	m.Components[0].Last = 1400000
	require.NoError(t, m.ValidatePrivateScale(), "metadata only, actual1.4M SQL scale has not run")
	require.Error(t, m.ValidateComplete())
	require.Error(t, m.ValidateFixture())
	_, e := MergeFixture(context.Background(), f.loader, f.owner, m, f.runtime)
	require.ErrorIs(t, e, ErrProductionDisabled)
	m.MigrationLast = 1400000
	require.Error(t, m.ValidatePrivateScale())
	m.MigrationLast = 0
	m.CanonicalRows = 1
	require.Error(t, m.ValidatePrivateScale())
}
