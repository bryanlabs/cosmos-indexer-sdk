package taxapi

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Real-row rewrite proofs use a new retained fixture. Every DELETE or TRUNCATE
// is rolled back. The preserved public copy is only ever read.
func TestStatementSummaryRealMutationPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	ctx := context.Background()
	db, err := gorm.Open(postgres.Open(os.Getenv("COSMOS_TAX_TEST_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	requireRealProofDatabase(t, db)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(2)
	defer func() { _ = pool.Close() }()
	schema := fmt.Sprintf("statement_mutation_proof_%d", time.Now().UnixNano())
	t.Logf("retained real-row mutation schema=%s", schema)
	for _, q := range []string{"CREATE SCHEMA " + schema, "SET search_path=" + schema, "SET TIME ZONE 'UTC'", "CREATE TABLE taxable_events AS SELECT * FROM public.taxable_events TABLESAMPLE SYSTEM (0.1) REPEATABLE (20261113) LIMIT 100000"} {
		require.NoError(t, db.Exec(q).Error)
	}
	var indexes []string
	require.NoError(t, db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='taxable_events' ORDER BY indexname").Scan(&indexes).Error)
	for _, ddl := range indexes {
		require.NoError(t, db.Exec(strings.ReplaceAll(ddl, "public.taxable_events", schema+".taxable_events")).Error)
	}
	n := summaryCount(t, db, "SELECT count(*) FROM taxable_events")
	require.Positive(t, n)
	require.NoError(t, InstallStatementSummary(ctx, db, 0, 1, nil))
	now := summaryDay(2026, 11, 13)
	opts := summaryOpts(now)
	opts.DrainTimeout = 3 * time.Minute
	_, err = FillStatementSummary(ctx, db, false, opts)
	require.NoError(t, err)
	s := &Server{db: db}
	fresh := flowPGValoper(99)
	check := func(phase string) {
		vals, err := summaryValidators(db)
		require.NoError(t, err)
		vals = append(vals, fresh)
		for _, val := range vals {
			want := legacyStatementAggregates(t, s, val, time.Time{}, now)
			got, err := s.readStatement(ctx, val, time.Time{}, now, true)
			require.NoError(t, err)
			requireRealStatementEqual(t, want, got.Agg)
		}
		t.Logf("phase=%s exact=true unique_union=true fixture_rows=%d", phase, n)
	}
	check("real baseline")
	var id int64
	require.NoError(t, db.Raw("SELECT id FROM taxable_events WHERE category='reward' AND timestamp<'2026-11-01' LIMIT 1").Row().Scan(&id))
	summaryExec(t, db, "UPDATE taxable_events SET amount=(amount::numeric+1)::text,to_addr='synthetic-rewritten-recipient' WHERE id=?", id)
	check("amount and recipient rewrite dirty")
	_, err = RefreshStatementSummary(ctx, db, opts)
	require.NoError(t, err)
	check("amount and recipient refreshed")
	summaryExec(t, db, "UPDATE taxable_events SET timestamp='2025-06-15',validator_address=? WHERE id=?", fresh, id)
	check("month and validator move dirty")
	_, err = RefreshStatementSummary(ctx, db, opts)
	require.NoError(t, err)
	check("month and validator refreshed")
	// Capture every validator before deletion, including any that a delete
	// temporarily empties. Compare affected accounting inside the transaction.
	affected, err := summaryValidators(db)
	require.NoError(t, err)
	compareInside := func(local *Server) {
		for _, val := range affected {
			want := legacyStatementAggregates(t, local, val, time.Time{}, now)
			got, err := local.readStatement(ctx, val, time.Time{}, now, true)
			require.NoError(t, err)
			requireRealStatementEqual(t, want, got.Agg)
		}
	}
	// The repair's delete and batch reinsert, a large delete, and truncation
	// are visible inside one snapshot, then rolled back with their marks.
	for _, shape := range []string{"repair", "large delete", "truncate"} {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer func() { _ = tx.Rollback().Error }()
		local := &Server{db: tx}
		switch shape {
		case "repair":
			summaryExec(t, tx, "CREATE TEMP TABLE replacement_rows AS SELECT * FROM taxable_events LIMIT 1000")
			require.Positive(t, summaryCount(t, tx, "SELECT count(*) FROM replacement_rows WHERE category IN ('reward','commission') AND validator_address<>'' AND timestamp<'2026-11-01'"))
			var expected []string
			require.NoError(t, tx.Raw("SELECT DISTINCT validator_address || ' ' || to_char(timestamp AT TIME ZONE 'UTC','YYYY-MM') FROM taxable_events WHERE message_id IN (SELECT message_id FROM replacement_rows) AND category IN ('reward','commission') AND validator_address<>'' AND timestamp<'2026-11-01'").Scan(&expected).Error)
			summaryExec(t, tx, "DELETE FROM taxable_events WHERE message_id IN (SELECT message_id FROM replacement_rows)")
			marks := summaryDirtyCells(t, local)
			for _, cell := range expected {
				require.True(t, slices.Contains(marks, cell), "an affected repair month was not marked")
			}
			compareInside(local)
			// A deliberately changed replacement prevents identity reinsertion
			// from concealing a missing invalidation.
			summaryExec(t, tx, "UPDATE replacement_rows SET amount=(amount::numeric+1)::text WHERE category IN ('reward','commission')")
			summaryExec(t, tx, "INSERT INTO taxable_events SELECT * FROM replacement_rows")
		case "large delete":
			summaryExec(t, tx, "DELETE FROM taxable_events WHERE category IN ('reward','commission')")
		case "truncate":
			summaryExec(t, tx, "TRUNCATE taxable_events")
		}
		compareInside(local)
		require.NoError(t, tx.Rollback().Error)
		check(shape + " rolled back")
	}
	// A committed write racing the refresh remains marked after recomputation.
	summaryExec(t, db, "UPDATE taxable_events SET amount=(amount::numeric+1)::text WHERE id=?", id)
	peer := summaryPGPeer(t, s)
	summaryRefreshHook = func() {
		require.NoError(t, peer.Exec("INSERT INTO taxable_events(id,message_id,sub_index,category,amount,denom,to_addr,timestamp,validator_address) SELECT -90000001,-90000001,0,category,amount,denom,to_addr,timestamp,validator_address FROM taxable_events WHERE id=?", id).Error)
	}
	_, err = RefreshStatementSummary(ctx, db, opts)
	summaryRefreshHook = nil
	require.NoError(t, err)
	check("concurrent writer retained")
	require.NotEmpty(t, summaryDirtyCells(t, s))
	_, err = RefreshStatementSummary(ctx, db, opts)
	require.NoError(t, err)
	check("concurrent writer refreshed")
	require.Empty(t, summaryDirtyCells(t, s))
	all, err := CheckStatementSummary(ctx, db, 0, 1, StatementSummaryOptions{})
	require.NoError(t, err)
	require.Empty(t, all.Mismatches)
	t.Logf("real mutation all-cell check=%d mismatches=0", all.Checked)
}
