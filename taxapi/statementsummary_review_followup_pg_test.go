// Keep adversarial fixture SQL beside each failure scenario.
//
//nolint:goconst
package taxapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatementSummaryReviewRejectsBrokenLoggerPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	summaryExec(t, s.db, "CREATE OR REPLACE FUNCTION statement_log_change() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NULL; END$$")
	_, err = FillStatementSummary(ctx, s.db, true, summaryOpts(now))
	require.ErrorContains(t, err, "reinstall")
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, nil))
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	summaryExec(t, s.db, "UPDATE taxable_events SET amount=(amount::numeric+1)::text WHERE category='reward'")
	require.NotEmpty(t, summaryDirtyCells(t, s))
	requireSummaryEqualsBase(t, s, "reinstalled logger marks subsequent historical updates")
}

func TestStatementSummaryReviewFallbackPublicationRacePostgres(t *testing.T) {
	for _, stage := range []string{"fill", "advance"} {
		t.Run(stage, func(t *testing.T) {
			s := summaryPGServer(t)
			writer := summaryPGPeer(t, s)
			locker := summaryPGPeer(t, s)
			ctx := context.Background()
			now := summaryDay(2026, 11, 13)
			if stage == "advance" {
				_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2025, 9, 15)))
				require.NoError(t, err)
			}
			summaryPublishHook = func() {
				lock := locker.Begin()
				require.NoError(t, lock.Error)
				defer func() { _ = lock.Rollback().Error }()
				summaryExec(t, lock, "LOCK TABLE statement_dirty IN ACCESS EXCLUSIVE MODE")
				summaryExec(t, lock, "ALTER SEQUENCE statement_log_failures RESTART WITH 1")
				summaryExec(t, writer, "INSERT INTO taxable_events(category,amount,denom,to_addr,timestamp,validator_address) VALUES('reward','1','uatom','synthetic-publication-race','2025-05-15',?)", statementPGBig)
				require.NoError(t, lock.Rollback().Error)
			}
			t.Cleanup(func() { summaryPublishHook = nil })
			var err error
			if stage == "fill" {
				_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
			} else {
				_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(now))
			}
			summaryPublishHook = nil
			require.ErrorContains(t, err, "invalidated")
			require.False(t, summaryPlan(t, s, statementPGBig).Summary)
			require.Zero(t, requireSummaryEqualsBase(t, s, "fallback survived "+stage+" publication"))
		})
	}
}

func TestStatementSummaryReviewBlockedWithdrawalPostgres(t *testing.T) {
	s := summaryPGServer(t)
	locker := summaryPGPeer(t, s)
	ctx := context.Background()
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	summaryExec(t, s.db, "UPDATE statement_days SET amount=amount+1")
	lock := locker.Begin()
	require.NoError(t, lock.Error)
	defer func() { _ = lock.Rollback().Error }()
	summaryExec(t, lock, "UPDATE statement_state SET updated_at=updated_at")
	res, err := CheckStatementSummary(ctx, s.db, 0, 1, StatementSummaryOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, res.Mismatches)
	// Observe while the row is still locked. A timed-out backend update
	// may finish after the lock is released, which is safe but would no
	// longer isolate the sequence-only protection being proved here.
	trust := mustTrust(t, s.db)
	require.NotNil(t, trust.BuiltBefore, "the state withdrawal was blocked, so this proves the independent counter path")
	require.Greater(t, trust.Failures, trust.Acked)
	require.False(t, summaryPlan(t, s, statementPGBig).Summary)
	require.NoError(t, lock.Rollback().Error)
}

func TestStatementSummaryReviewRejectsInheritedBasePostgres(t *testing.T) {
	s := summaryPGServer(t)
	summaryExec(t, s.db, "CREATE TABLE statement_parent (LIKE taxable_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)")
	summaryExec(t, s.db, "ALTER TABLE taxable_events INHERIT statement_parent")
	_, err := FillStatementSummary(context.Background(), s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "partitioned")
	require.False(t, mustTrust(t, s.db).PlainTable)
	summaryExec(t, s.db, "ALTER TABLE taxable_events NO INHERIT statement_parent")
	_, err = FillStatementSummary(context.Background(), s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.Positive(t, requireSummaryEqualsBase(t, s, "standalone base restored"))
}

func TestStatementSummaryReviewTemporaryInheritanceDuringFillPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	vals, err := summaryValidators(s.db)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(vals), 2)
	target := vals[len(vals)-1]
	summaryExec(t, peer, "CREATE TABLE statement_child (LIKE taxable_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)")
	summaryExec(t, peer, "INSERT INTO statement_child(category,amount,denom,to_addr,timestamp,validator_address) VALUES('reward','1000','uatom','synthetic-child-only-recipient','2025-05-15',?)", target)
	attached := false
	opts := summaryOpts(summaryDay(2026, 11, 13))
	opts.Logf = func(format string, args ...any) {
		if !attached && strings.HasPrefix(fmt.Sprintf(format, args...), "statement-summary fill ") {
			// The first validator is finished, but the populated child's
			// validator has not been built yet. No financial row is removed.
			summaryExec(t, peer, "ALTER TABLE statement_child INHERIT taxable_events")
			attached = true
		}
	}
	_, err = FillStatementSummary(context.Background(), s.db, false, opts)
	require.ErrorContains(t, err, "invalidated")
	require.True(t, attached)
	summaryExec(t, peer, "ALTER TABLE statement_child NO INHERIT taxable_events")
	require.False(t, summaryPlan(t, s, target).Summary, "detachment must not restore a withdrawn build")
	_, err = FillStatementSummary(context.Background(), s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.True(t, summaryPlan(t, s, target).Summary)
	require.Positive(t, requireSummaryEqualsBase(t, s, "temporary inheritance cannot inject phantom income"))
	require.Equal(t, int64(1), summaryCount(t, peer, "SELECT count(*) FROM statement_child"), "child evidence retained")
}

func TestStatementSummaryReviewCancellationAtCompareCommitPostgres(t *testing.T) {
	s := summaryPGServer(t)
	observer := summaryPGPeer(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	summaryExec(t, s.db, "UPDATE statement_days SET amount=amount+1")
	fired := false
	summaryCompareHook = func() {
		if !fired {
			fired = true
			cancel()
		}
	}
	t.Cleanup(func() { summaryCompareHook = nil })
	res, err := CheckStatementSummary(ctx, s.db, 0, 1, StatementSummaryOptions{})
	summaryCompareHook = nil
	require.Error(t, err, fmt.Sprint("cancelled after comparison, fired=", fired))
	require.True(t, fired)
	require.NotEmpty(t, res.Mismatches)
	require.False(t, summaryPlan(t, &Server{db: observer}, statementPGBig).Summary, "counter withdrawal survives a retired comparison session")
}
