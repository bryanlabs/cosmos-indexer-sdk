package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
)

func TestRehearsalPGPinnedSessionLossFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := pgtest.OpenRetained(t)
	preserved := pgtest.OpenRetained(t)
	publicBefore := pgtest.Snapshot(t, db.Owner, "public")
	preservedBefore := pgtest.Snapshot(t, preserved.Owner, "public")
	block := ledger.Block{Chain: "cosmoshub-2", Height: 9, Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Hash: strings.Repeat("AB", 32), Proposer: "synthetic"}
	preservedConn, err := preserved.Importer.DB()
	require.NoError(t, err)
	_, err = ledger.Write(ctx, preservedConn, []ledger.Block{block})
	require.NoError(t, err)
	preservedLegacyBefore := pgtest.Snapshot(t, preserved.Owner, "legacy")
	var name string
	require.NoError(t, db.Owner.Raw("SELECT current_database()").Scan(&name).Error)
	require.True(t, rehearsalName.MatchString(name))
	dsn := db.Importer.Dialector.(*postgres.Dialector).Config.DSN
	first, err := openRehearsalDestination(ctx, dsn, name)
	require.NoError(t, err)
	defer first.close()
	var pid int
	require.NoError(t, first.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	var owns bool
	require.NoError(t, db.Owner.Raw(`SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
		WHERE l.pid = ? AND l.locktype = 'advisory' AND l.granted AND l.objid = 340310
		AND a.datname = current_database() AND a.usename = ?)`, pid, db.Role).Scan(&owns).Error)
	require.True(t, owns, "the pinned backend itself must hold admission's lock")
	blocked, err := openRehearsalDestination(ctx, dsn, name)
	require.Error(t, err)
	require.Nil(t, blocked)

	// Only the known importer backend in this newly named scratch database
	// can be terminated, with the owner connection and a bounded wait.
	var terminated bool
	require.NoError(t, db.Owner.Raw(`SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
		WHERE pid = ? AND datname = current_database() AND datname = ? AND usename = ?`, pid, name, db.Role).Scan(&terminated).Error)
	require.True(t, terminated)
	alternate, err := openRehearsalDestination(ctx, dsn, name)
	require.NoError(t, err, "the released lock must be acquired by another importer")
	defer alternate.close()
	var alternatePID int
	require.NoError(t, alternate.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&alternatePID))
	require.NotEqual(t, pid, alternatePID)

	// This same valid block was written to the separate preservation fixture;
	// rejection here must be caused by loss of the pinned session.
	_, err = ledger.Write(ctx, first.conn, []ledger.Block{block})
	require.Error(t, err)
	require.Error(t, first.write(ctx, block))
	require.True(t, first.failed)
	require.Error(t, first.write(ctx, block))
	require.Error(t, first.conn.PingContext(ctx), "the dedicated connection cannot reconnect")
	require.Equal(t, 0, first.pool.Stats().InUse)
	require.Equal(t, 0, first.pool.Stats().OpenConnections, "no replacement session was opened")
	var count int64
	for _, table := range []string{"blocks", "tx_inclusions", "taxable_events", "block_events", "import_ranges", "genesis_files", "genesis_accounts", "genesis_delegations", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses", "reconciliation", "balance_days"} {
		require.NoError(t, db.Owner.Raw("SELECT count(*) FROM legacy."+table).Scan(&count).Error)
		require.Zero(t, count, "failed writer must leave no rows")
	}
	require.NoError(t, alternate.write(ctx, block))
	var writerPID int
	// The surviving pinned session remains exactly the one that acquired its lock.
	require.NoError(t, alternate.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&writerPID))
	require.Equal(t, alternatePID, writerPID)
	pgtest.RequireUntouched(t, publicBefore, pgtest.Snapshot(t, db.Owner, "public"))
	pgtest.RequireUntouched(t, preservedBefore, pgtest.Snapshot(t, preserved.Owner, "public"))
	pgtest.RequireUntouched(t, preservedLegacyBefore, pgtest.Snapshot(t, preserved.Owner, "legacy"))
}
