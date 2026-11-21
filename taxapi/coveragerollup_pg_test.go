package taxapi

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The rollup sums must equal the live reconciliation query for any period,
// before and after the index moves. Runs in a fresh schema of a throwaway
// database (COSMOS_TAX_TEST_DSN).
func TestCoverageRollupMatchesLiveQueryPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN to a throwaway Postgres for the coverage rollup proof")
	}
	schema := fmt.Sprintf("covroll_%d", time.Now().UnixNano())
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, boot.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		boot.Exec("DROP SCHEMA " + schema + " CASCADE")
		if c, err := boot.DB(); err == nil {
			_ = c.Close()
		}
	})
	sep := " "
	if strings.Contains(dsn, "://") {
		sep = "&"
		if !strings.Contains(dsn, "?") {
			sep = "?"
		}
		dsn += sep + "search_path=" + schema + "&timezone=America/New_York"
	} else {
		dsn += sep + "search_path=" + schema + " timezone=America/New_York"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = pool.Close() })

	for _, q := range []string{
		"CREATE TABLE chains (id bigint PRIMARY KEY, chain_id text)",
		"CREATE TABLE blocks (id bigint PRIMARY KEY, height bigint, chain_id bigint, time_stamp timestamptz)",
		"CREATE TABLE txes (id bigserial PRIMARY KEY, block_id bigint, hash text, code bigint)",
		"CREATE INDEX ON txes (block_id)",
		"CREATE TABLE message_types (id bigint PRIMARY KEY, message_type text NOT NULL UNIQUE)",
		"CREATE TABLE messages (id bigserial PRIMARY KEY, tx_id bigint, message_type_id bigint, message_index bigint, opaque boolean NOT NULL DEFAULT false)",
		"CREATE TABLE taxable_events (id bigserial PRIMARY KEY, message_id bigint, sub_index bigint)",
		"CREATE INDEX ON taxable_events (message_id, sub_index)",
		// classificationReadiness reads these; empty is fine.
		"CREATE TABLE message_parser_errors (message_id bigint)",
		"CREATE TABLE failed_messages (tx_id bigint)",
		"CREATE TABLE failed_txes (block_id bigint, hash text)",
		"INSERT INTO chains VALUES (1, 'cosmoshub-4'), (2, 'osmosis-1')",
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
	types := []string{"/cosmos.bank.v1beta1.MsgSend", "/cosmos.staking.v1beta1.MsgDelegate", "/cosmos.gov.v1beta1.MsgVote",
		"/cosmwasm.wasm.v1.MsgExecuteContract", "/ibc.core.client.v1.MsgUpdateClient"}
	for i, ty := range types {
		require.NoError(t, db.Exec("INSERT INTO message_types VALUES (?, ?)", i+1, ty).Error)
	}
	rng := rand.New(rand.NewSource(20261121))
	base := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC)
	blockID := int64(0)
	// addBlocks writes n blocks with txs and messages; times step across
	// day, month and year boundaries and include exact midnights.
	addBlocks := func(n int, from time.Time, step time.Duration) {
		for i := 0; i < n; i++ {
			blockID++
			ts := any(from.Add(time.Duration(i) * step))
			chain := any(int64(1))
			switch {
			case blockID%17 == 0:
				ts = nil
			case blockID%11 == 0:
				chain = int64(2)
			case blockID%13 == 0:
				chain = nil
			}
			require.NoError(t, db.Exec("INSERT INTO blocks VALUES (?, ?, ?, ?)", blockID, blockID, chain, ts).Error)
			for k := 0; k < rng.Intn(4); k++ {
				var txID int64
				require.NoError(t, db.Raw("INSERT INTO txes (block_id, hash, code) VALUES (?, 'h', 0) RETURNING id", blockID).Scan(&txID).Error)
				for m := 0; m < rng.Intn(5); m++ {
					typeID := any(int64(rng.Intn(len(types)) + 1))
					if rng.Intn(40) == 0 {
						typeID = int64(99)
					} else if rng.Intn(40) == 0 {
						typeID = nil
					}
					var msgID int64
					require.NoError(t, db.Raw("INSERT INTO messages (tx_id, message_type_id, message_index) VALUES (?, ?, ?) RETURNING id", txID, typeID, m).Scan(&msgID).Error)
					for e := 0; e < rng.Intn(3); e++ {
						require.NoError(t, db.Exec("INSERT INTO taxable_events (message_id, sub_index) VALUES (?, ?)", msgID, e).Error)
					}
				}
			}
		}
		// messages without a tx, and a tx without a block
		require.NoError(t, db.Exec("INSERT INTO messages (tx_id, message_type_id) VALUES (NULL, 1)").Error)
		require.NoError(t, db.Exec("INSERT INTO txes (block_id, hash, code) VALUES (987654321, 'orphan', 0)").Error)
	}
	addBlocks(400, base, 41*time.Minute) // about 11 days over the new year
	addBlocks(60, base.AddDate(0, 0, 5).Add(3*time.Hour), 2*time.Hour)

	live := &Server{db: db}
	rolled := &Server{db: db, covRollup: newCoverageRollup(db)}
	ctx := context.Background()
	periods := [][2]time.Time{
		{time.Time{}, time.Date(2026, 11, 21, 15, 4, 5, 0, time.UTC)}, // all history, unaligned end
		{time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},   // a year
		{time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},  // a month
		{time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},   // a day
		{time.Date(2026, 1, 2, 7, 30, 0, 0, time.UTC), time.Date(2026, 1, 4, 13, 0, 0, 0, time.UTC)}, // partial first and last days
		{time.Date(2026, 1, 2, 7, 30, 0, 0, time.UTC), time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)},  // inside one day
		{time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},   // empty
	}
	txCount := func(start, end time.Time) int64 {
		var n int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM txes t JOIN blocks b ON b.id = t.block_id JOIN chains c ON c.id = b.chain_id
 WHERE c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ?`, chainIDs(apiMainnet), start, end).Scan(&n).Error)
		return n
	}
	compare := func(phase string, wantRollup bool) {
		t.Helper()
		for _, p := range periods {
			want, err := live.coverage(apiMainnet, p[0], p[1])
			require.NoError(t, err)
			got, err := rolled.coverage(apiMainnet, p[0], p[1])
			require.NoError(t, err, phase)
			label := fmt.Sprintf("%s %s..%s", phase, p[0].Format(time.RFC3339), p[1].Format(time.RFC3339))
			require.Equal(t, sortedRows(want.Rows), sortedRows(got.Rows), label)
			require.Equal(t, want.TotalMessages, got.TotalMessages, label)
			require.Equal(t, want.ClassifiedMsgs, got.ClassifiedMsgs, label)
			require.Equal(t, len(want.Gaps), len(got.Gaps), label)
			if wantRollup {
				require.NotNil(t, got.TransactionsScanned, label)
				require.Equal(t, txCount(p[0], p[1]), *got.TransactionsScanned, label)
				require.NotNil(t, got.CountedFrom, label)
			} else {
				require.Nil(t, got.TransactionsScanned, label)
			}
			for i := 1; i < len(got.Rows); i++ {
				require.GreaterOrEqual(t, got.Rows[i-1].Total, got.Rows[i].Total, label)
			}
		}
	}
	someMessages := func() bool {
		r, err := live.coverage(apiMainnet, periods[0][0], periods[0][1])
		require.NoError(t, err)
		return r.TotalMessages > 100 && r.ClassifiedMsgs > 0 && r.ClassifiedMsgs < r.TotalMessages
	}
	require.True(t, someMessages())

	// No rollup tables: the rollup server counts live.
	compare("no tables", false)

	require.NoError(t, db.Exec(CoverageRollupDDL).Error)
	require.NoError(t, db.Exec("UPDATE tax_coverage_rollup_state SET message_chunk = 37, tx_chunk = 23").Error)
	// Installed but not built yet: still live.
	compare("not built", false)

	opts := CoverageRollupOptions{BatchTime: time.Millisecond, Logf: t.Logf}
	res, err := RefreshCoverageRollup(ctx, db, opts)
	require.NoError(t, err)
	require.True(t, res.Current)
	require.Positive(t, res.Built)
	compare("built", true)
	diffs, err := CheckCoverageRollup(ctx, db, 0, 1, t.Logf)
	require.NoError(t, err)
	require.Empty(t, diffs)

	// New blocks above the frontier are counted live until the next run.
	addBlocks(80, base.AddDate(0, 0, 9), 37*time.Minute)
	compare("live tail", true)
	res, err = RefreshCoverageRollup(ctx, db, opts)
	require.NoError(t, err)
	require.True(t, res.Current)
	compare("tail built", true)

	// A taxable event added to an old, unclassified message: the next run
	// marks its chunk dirty and rebuilds it.
	var oldMsg int64
	require.NoError(t, db.Raw(`SELECT m.id FROM messages m WHERE m.id < 200 AND m.message_type_id = 1 AND m.tx_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM taxable_events te WHERE te.message_id = m.id) ORDER BY m.id LIMIT 1`).Scan(&oldMsg).Error)
	require.Positive(t, oldMsg)
	require.NoError(t, db.Exec("INSERT INTO taxable_events (message_id, sub_index) VALUES (?, 0)", oldMsg).Error)
	res, err = RefreshCoverageRollup(ctx, db, opts)
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Dirtied, 1)
	compare("new event on old message", true)
	// With nothing changed, a run rebuilds nothing (ids just below the
	// frontier are re-read but their chunks have not moved).
	idle := opts
	idle.Rolling = -1
	res, err = RefreshCoverageRollup(ctx, db, idle)
	require.NoError(t, err)
	require.Equal(t, 0, res.Dirtied)
	require.Equal(t, 0, res.Rebuilt)
	var oldTE int64
	require.NoError(t, db.Raw("SELECT id FROM taxable_events WHERE message_id = ?", oldMsg).Scan(&oldTE).Error)

	// The only taxable event of an old message deleted: the fingerprint
	// pass finds the chunk.
	require.NoError(t, db.Exec("DELETE FROM taxable_events WHERE message_id = ?", oldMsg).Error)
	fp := opts
	fp.ForceFingerprint = true
	fp.FingerprintPerRun = 1000
	res, err = RefreshCoverageRollup(ctx, db, fp)
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Dirtied, 1)
	require.Positive(t, res.Fingerprinted)
	compare("deleted event", true)

	// A taxable event committed late, with an id below the frontier: the
	// re-read finds the chunk's classified count moved.
	require.NoError(t, db.Exec("INSERT INTO taxable_events (id, message_id, sub_index) VALUES (?, ?, 0)", oldTE, oldMsg).Error)
	res, err = RefreshCoverageRollup(ctx, db, idle)
	require.NoError(t, err)
	require.Equal(t, 1, res.Dirtied)
	compare("late taxable event", true)

	// A late commit below the frontier: a recent chunk's row count moved.
	var maxMsg int64
	require.NoError(t, db.Raw("SELECT max(id) FROM messages").Scan(&maxMsg).Error)
	var tx1 int64
	require.NoError(t, db.Raw("SELECT min(t.id) FROM txes t JOIN blocks b ON b.id = t.block_id WHERE b.chain_id = 1 AND b.time_stamp IS NOT NULL").Scan(&tx1).Error)
	// An id below the frontier is missing at one run (its insert has not
	// committed yet) and present at the next.
	require.NoError(t, db.Exec("DELETE FROM messages WHERE id = ?", maxMsg-40).Error)
	res, err = RefreshCoverageRollup(ctx, db, opts)
	require.NoError(t, err)
	compare("gap", true)
	require.NoError(t, db.Exec("INSERT INTO messages (id, tx_id, message_type_id) VALUES (?, ?, 2)", maxMsg-40, tx1).Error)
	res, err = RefreshCoverageRollup(ctx, db, opts)
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Dirtied, 1)
	compare("late commit", true)

	// The rolling refresh rebuilds the oldest chunks.
	roll := opts
	roll.Rolling = 3
	res, err = RefreshCoverageRollup(ctx, db, roll)
	require.NoError(t, err)
	require.Equal(t, 6, res.Rolled)
	diffs, err = CheckCoverageRollup(ctx, db, 0, 1, t.Logf)
	require.NoError(t, err)
	require.Empty(t, diffs)

	// Another run holding the lock.
	other, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	otherPool, err := other.DB()
	require.NoError(t, err)
	otherPool.SetMaxOpenConns(1)
	defer otherPool.Close()
	require.NoError(t, other.Exec("SELECT pg_advisory_lock(?)", coverageRollupLockKey).Error)
	_, err = RefreshCoverageRollup(ctx, db, opts)
	require.ErrorIs(t, err, ErrCoverageRollupBusy)
	require.NoError(t, other.Exec("SELECT pg_advisory_unlock(?)", coverageRollupLockKey).Error)

	// The statement carries the scan counts; the cache answers a repeat.
	p := rolled.periodCoverage(periods[3][0], periods[3][1])
	require.NotNil(t, p.Statement.TransactionsScanned)
	require.Equal(t, txCount(periods[3][0], periods[3][1]), *p.Statement.TransactionsScanned)
	require.Contains(t, p.Statement.Text, "Transactions scanned: ")
	require.Contains(t, p.Statement.Text, " cosmoshub-4 transactions (")
	status, err := CoverageRollupStatus(ctx, db)
	require.NoError(t, err)
	t.Log(status)

	// Revoked read: the API counts live again.
	require.NoError(t, db.Exec("ALTER TABLE tax_coverage_rollup_state RENAME TO tax_coverage_rollup_state_off").Error)
	compare("state missing", false)
	_ = sql.ErrNoRows
}

func sortedRows(rows []CoverageRow) []CoverageRow {
	out := append([]CoverageRow(nil), rows...)
	sort.Slice(out, func(i, j int) bool { return out[i].MessageType < out[j].MessageType })
	return out
}
