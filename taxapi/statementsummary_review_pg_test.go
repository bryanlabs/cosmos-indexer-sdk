// Explicit fixture SQL stays beside its accounting and failure scenario.
//
//nolint:goconst
package taxapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatementSummaryReviewCatalogContinuityPostgres(t *testing.T) {
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	t.Run("disable then restore", func(t *testing.T) {
		s := summaryPGServer(t)
		_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
		require.NoError(t, err)
		summaryExec(t, s.db, "ALTER TABLE taxable_events DISABLE TRIGGER statement_log_update")
		summaryExec(t, s.db, "UPDATE taxable_events SET amount=(amount::numeric+1)::text WHERE category='reward'")
		summaryExec(t, s.db, "ALTER TABLE taxable_events ENABLE ALWAYS TRIGGER statement_log_update")
		require.False(t, summaryPlan(t, s, statementPGBig).Summary)
		require.Nil(t, mustTrust(t, s.db).BuiltBefore, "DDL withdrawal must persist after logging is restored")
		require.Zero(t, requireSummaryEqualsBase(t, s, "logging restored after missed write"))
	})
	t.Run("accounting rewrite", func(t *testing.T) {
		s := summaryPGServer(t)
		_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
		require.NoError(t, err)
		summaryExec(t, s.db, "ALTER TABLE taxable_events ALTER COLUMN amount TYPE numeric USING amount::numeric * 2")
		require.False(t, summaryPlan(t, s, statementPGBig).Summary)
		require.Nil(t, mustTrust(t, s.db).BuiltBefore, "accounting-changing DDL must durably withdraw trust")
		require.Zero(t, requireSummaryEqualsBase(t, s, "accounting-changing DDL"))
	})
	t.Run("replaced summary object", func(t *testing.T) {
		s := summaryPGServer(t)
		_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
		require.NoError(t, err)
		summaryExec(t, s.db, "ALTER TABLE statement_days RENAME TO retained_statement_days")
		summaryExec(t, s.db, "ALTER INDEX statement_days_key RENAME TO retained_statement_days_key")
		require.False(t, summaryPlan(t, s, statementPGBig).Summary)
		require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, nil))
		require.False(t, summaryPlan(t, s, statementPGBig).Summary, "empty replacement must not be certified")
		require.Positive(t, summaryCount(t, s.db, "SELECT count(*) FROM retained_statement_days"))
		require.Zero(t, requireSummaryEqualsBase(t, s, "reinstalled after summary replacement"))
	})
}

func TestStatementSummaryReviewMismatchWithdrawalPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	summaryExec(t, s.db, "UPDATE statement_days SET amount=amount+1")
	opts := StatementSummaryOptions{Logf: func(format string, args ...any) {
		if strings.Contains(format, "MISMATCH") {
			_, busy := RefreshStatementSummary(context.Background(), peer, summaryOpts(now))
			require.ErrorIs(t, busy, ErrStatementSummaryBusy)
			cancel()
		}
	}}
	res, err := CheckStatementSummary(ctx, s.db, 0, 1, opts)
	require.Error(t, err, "later comparisons must observe cancellation")
	require.NotEmpty(t, res.Mismatches)
	// Cancellation may retire the primary physical session. Observe the
	// committed withdrawal through the independent, schema-bound peer.
	observer := &Server{db: peer}
	require.False(t, summaryPlan(t, observer, statementPGBig).Summary, "withdrawal survived cancellation and no builder could republish it")
	require.Positive(t, summaryCount(t, peer, "SELECT count(*) FROM statement_days"))
}

func TestStatementSummaryReviewCounterFallbackPostgres(t *testing.T) {
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	for _, blockState := range []bool{false, true} {
		t.Run(fmt.Sprintf("block_state_%t", blockState), func(t *testing.T) {
			s := summaryPGServer(t)
			peer := summaryPGPeer(t, s)
			_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
			require.NoError(t, err)
			before := summaryCount(t, s.db, "SELECT count(*) FROM taxable_events")
			lock := peer.Begin()
			require.NoError(t, lock.Error)
			defer func() { _ = lock.Rollback().Error }()
			summaryExec(t, lock, "LOCK TABLE statement_dirty IN ACCESS EXCLUSIVE MODE")
			summaryExec(t, lock, "ALTER SEQUENCE statement_log_failures RESTART WITH 1")
			if blockState {
				summaryExec(t, lock, "LOCK TABLE statement_state IN ACCESS EXCLUSIVE MODE")
			}
			write := s.db.Exec("INSERT INTO taxable_events(category,amount,denom,to_addr,timestamp,validator_address) VALUES('reward','1','uatom','synthetic-fallback','2025-06-15',?)", statementPGBig)
			require.NoError(t, lock.Rollback().Error)
			if blockState {
				require.Error(t, write.Error, "if every durable invalidation path is blocked the accounting write must fail closed")
				require.Equal(t, before, summaryCount(t, s.db, "SELECT count(*) FROM taxable_events"))
			} else {
				require.NoError(t, write.Error)
				require.Equal(t, before+1, summaryCount(t, s.db, "SELECT count(*) FROM taxable_events"))
				require.False(t, summaryPlan(t, s, statementPGBig).Summary, "fallback withdrawal protects a committed unmarked write")
			}
		})
	}
}

func TestStatementSummaryReviewRestrictedRolePostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	var schema string
	require.NoError(t, s.db.Raw("SELECT current_schema()").Row().Scan(&schema))
	role := fmt.Sprintf("statement_proof_reader_%d", time.Now().UnixNano())
	summaryExec(t, s.db, "CREATE ROLE "+role+" NOLOGIN")
	summaryExec(t, s.db, "GRANT USAGE ON SCHEMA "+schema+" TO "+role)
	summaryExec(t, s.db, "GRANT SELECT ON ALL TABLES IN SCHEMA "+schema+" TO "+role)
	summaryExec(t, s.db, "SET ROLE "+role)
	defer func() { _ = s.db.Exec("RESET ROLE").Error }()
	trust, err := readSummaryTrust(s.db)
	require.NoError(t, err)
	require.Contains(t, trust.Reason(), "counter is not readable")
	_, err = summaryDrain(ctx, s.db, "1", summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "pg_read_all_stats")
}
