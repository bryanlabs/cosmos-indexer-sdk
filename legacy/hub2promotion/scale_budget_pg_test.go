package hub2promotion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPGPrivateScaleBudgetIsBoundedAndReadOnly(t *testing.T) {
	f := newFixture(t)
	for _, scale := range []bool{false, true} {
		tx, e := beginAuditBudget(context.Background(), f.owner, scale)
		require.NoError(t, e)
		var timeout, lock, ro, isolation string
		require.NoError(t, tx.QueryRow(`SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),current_setting('transaction_read_only'),current_setting('transaction_isolation')`).Scan(&timeout, &lock, &ro, &isolation))
		if scale {
			require.Equal(t, "10min", timeout)
		} else {
			require.Equal(t, "30s", timeout)
		}
		require.Equal(t, "250ms", lock)
		require.Equal(t, "on", ro)
		require.Equal(t, "repeatable read", isolation)
		require.NoError(t, tx.Rollback())
	}
}

func TestPrivateScaleToolsRefuseOtherDatabaseAndTool(t *testing.T) {
	require.Error(t, localScaleTool(context.Background(), "pg_dump", "postgres", nil, nil))
	require.Error(t, localScaleTool(context.Background(), "psql", "legacy_replay_rehearsal_test", nil, nil))
}

func TestPrivateScaleRetainedStorageIdentity(t *testing.T) {
	id, e := scaleStorageIdentity("retained-nodepath", "", "controller-1", scaleRetainedNodeUID)
	require.NoError(t, e)
	require.Equal(t, "PRIVATE-RETAINED-NODEPATH:"+scaleRetainedNodeUID+":"+digest([]byte(scaleRetainedNodePath)), id)
	_, e = scaleStorageIdentity("retained-nodepath", "", "worker-1", scaleRetainedNodeUID)
	require.Error(t, e)
	_, e = scaleStorageIdentity("retained-nodepath", "", "controller-1", "wrong-node-uid")
	require.Error(t, e)
	_, e = scaleStorageIdentity("", "", "", "")
	require.Error(t, e)
	id, e = scaleStorageIdentity("pvc", "private-pvc-fixture", "", "")
	require.NoError(t, e)
	require.Equal(t, "private-pvc-fixture", id)
}

func TestPGPrivateScaleLockMeasurement(t *testing.T) {
	f := newFixture(t)
	r, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Positive(t, r.LegacyLockHeld)
	require.Less(t, r.LegacyLockHeld, r.Duration)
	require.Less(t, r.LockWait, r.Duration)
}
