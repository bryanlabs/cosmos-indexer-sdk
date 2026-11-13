package taxapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
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

func summaryDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var summaryPGValidators = []string{statementPGBig, statementPGSmall, statementPGGold, statementPGNone}

// summaryPGWindows are closed windows, open ones, windows that start or end
// inside a month, and windows with no row.
var summaryPGWindows = []struct {
	name       string
	start, end time.Time
}{
	{"all time", time.Time{}, summaryDay(2026, 11, 14)},
	{"calendar 2025", summaryDay(2025, 1, 1), summaryDay(2026, 1, 1)},
	{"one month", summaryDay(2025, 6, 1), summaryDay(2025, 7, 1)},
	{"inside two months", summaryDay(2025, 3, 17), summaryDay(2025, 4, 9)},
	{"mid month to mid month", summaryDay(2025, 1, 20), summaryDay(2025, 10, 5)},
	{"one day", summaryDay(2025, 6, 15), summaryDay(2025, 6, 16)},
	{"month without recipients", summaryDay(2026, 6, 1), summaryDay(2026, 7, 1)},
	{"open end", summaryDay(2025, 5, 1), summaryDay(2027, 1, 1)},
	{"before any row", summaryDay(2020, 1, 1), summaryDay(2021, 1, 1)},
	{"empty range", summaryDay(2025, 6, 1), summaryDay(2025, 6, 1)},
}

func summaryOpts(now time.Time) StatementSummaryOptions {
	return StatementSummaryOptions{Now: func() time.Time { return now }, DrainTimeout: 20 * time.Second}
}

// summaryPGServer is statementPGServer with the unique key the indexer
// upserts on and the summary installed. The fixture rows carry no message.
func summaryPGServer(t *testing.T) *Server {
	t.Helper()
	s := statementPGServer(t)
	require.NoError(t, s.db.Exec("CREATE UNIQUE INDEX tax_msg_sub ON taxable_events (message_id, sub_index)").Error)
	require.NoError(t, InstallStatementSummary(context.Background(), s.db, 0, 1, t.Logf))
	poll := summaryDrainPoll
	summaryDrainPoll = 50 * time.Millisecond
	t.Cleanup(func() { summaryDrainPoll = poll })
	return s
}

// summaryPGPeer opens another session on the server's schema.
func summaryPGPeer(t *testing.T, s *Server) *gorm.DB {
	t.Helper()
	var schema string
	require.NoError(t, s.db.Raw("SELECT current_schema()").Row().Scan(&schema))
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "search_path=" + schema
	} else {
		dsn += " search_path=" + schema
	}
	peer, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, err := peer.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, peer.Exec("SET TIME ZONE 'UTC'").Error)
	return peer
}

func summaryExec(t *testing.T, db *gorm.DB, sqlText string, args ...any) int64 {
	t.Helper()
	res := db.Exec(sqlText, args...)
	require.NoError(t, res.Error, sqlText)
	return res.RowsAffected
}

func summaryCount(t *testing.T, db *gorm.DB, sqlText string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(sqlText, args...).Row().Scan(&n), sqlText)
	return n
}

func summaryDirtyCells(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Raw("SELECT DISTINCT validator_address || ' ' || COALESCE(to_char(month, 'YYYY-MM'), 'all') FROM statement_dirty ORDER BY 1").Rows()
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var cell string
		require.NoError(t, rows.Scan(&cell))
		out = append(out, cell)
	}
	return out
}

// requireSummaryEqualsBase reads every window of every validator through the
// summary path and requires exactly what the three reference queries return
// from taxable_events alone. It returns how many reads used the summary.
func requireSummaryEqualsBase(t *testing.T, s *Server, when string) int {
	t.Helper()
	used := 0
	for _, val := range summaryPGValidators {
		for _, w := range summaryPGWindows {
			want := legacyStatementAggregates(t, s, val, w.start, w.end)
			got, err := s.readStatement(context.Background(), val, w.start, w.end, true)
			require.NoError(t, err, "%s: %s %s", when, val, w.name)
			require.Equal(t, statementDayKeys(want.Days), statementDayKeys(got.Agg.Days), "%s: %s %s: day sums (%s)", when, val, w.name, got.Plan.describe())
			require.Equal(t, want.DelegatorsByMonth, got.Agg.DelegatorsByMonth, "%s: %s %s: delegators by month (%s)", when, val, w.name, got.Plan.describe())
			require.Equal(t, want.Delegators, got.Agg.Delegators, "%s: %s %s: unique delegators (%s)", when, val, w.name, got.Plan.describe())
			require.True(t, sort.SliceIsSorted(got.Agg.Days, func(i, j int) bool { return got.Agg.Days[i].Day.Before(got.Agg.Days[j].Day) }), "%s: %s %s: days in order", when, val, w.name)
			if got.Plan.Summary {
				used++
			}
		}
	}
	return used
}

func requireSummaryCheck(t *testing.T, s *Server, when string) int {
	t.Helper()
	res, err := CheckStatementSummary(context.Background(), s.db, 0, 1, StatementSummaryOptions{Logf: t.Logf})
	require.NoError(t, err, when)
	require.Empty(t, res.Mismatches, when)
	return res.Checked
}

func summaryPlan(t *testing.T, s *Server, val string) statementReadPlan {
	t.Helper()
	read, err := s.readStatement(context.Background(), val, time.Time{}, summaryDay(2026, 11, 14), false)
	require.NoError(t, err)
	return read.Plan
}

// The summary plus the live months must be exactly the base-table statement:
// with part of the history built (open months read live), after a month
// advance, and with the summary turned off.
func TestStatementSummaryEqualsBasePostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()

	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "installed, not built"))
	require.Equal(t, "live rows only (not built)", summaryPlan(t, s, statementPGBig).describe())

	res, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2025, 9, 15)))
	require.NoError(t, err)
	require.Equal(t, summaryDay(2025, 9, 1), res.BuiltBefore)
	require.Equal(t, 3, res.Validators)
	require.Positive(t, res.DayRows)
	require.Positive(t, res.Pairs)
	require.Zero(t, summaryCount(t, s.db, "SELECT count(*) FROM statement_days WHERE day >= '2025-09-01'"), "an unfinished month is never built")
	require.Greater(t, requireSummaryEqualsBase(t, s, "built to 2025-09"), 20)
	plan := summaryPlan(t, s, statementPGBig)
	require.True(t, plan.Summary)
	require.Equal(t, "summary 0001-01 to 2025-08, 0 changed month(s) and 1 range(s) read live", plan.describe())
	require.Greater(t, requireSummaryCheck(t, s, "built to 2025-09"), 20)

	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2025, 9, 15)))
	require.ErrorContains(t, err, "already built")

	// Rows written to months that are not built yet are not logged.
	summaryExec(t, s.db, "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward','5','uatom','cosmos1late','2025-10-03T00:00:00Z',?)", statementPGBig)
	require.Empty(t, summaryDirtyCells(t, s))

	// Time passes: the refresh builds the months that ended.
	ref, err := RefreshStatementSummary(ctx, s.db, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.True(t, ref.Advanced)
	require.Equal(t, summaryDay(2026, 11, 1), ref.BuiltBefore)
	require.Greater(t, requireSummaryEqualsBase(t, s, "built to 2026-11"), 20)
	require.Greater(t, requireSummaryCheck(t, s, "built to 2026-11"), 40)
	plan = summaryPlan(t, s, statementPGBig)
	require.Equal(t, "summary 0001-01 to 2026-10, 0 changed month(s) and 1 range(s) read live", plan.describe())

	// A window inside one month has no whole month to take from the summary.
	read, err := s.readStatement(ctx, statementPGBig, summaryDay(2025, 6, 15), summaryDay(2025, 6, 16), true)
	require.NoError(t, err)
	require.False(t, read.Plan.Summary)

	// The whole handler returns the same bytes with the summary and without.
	for _, query := range []string{"", "&start=2025-01-01&end=2026-01-01", "&start=2025-03-17&end=2025-10-05", "&format=csv", "&start=2025-01-01&end=2026-01-01&format=csv"} {
		for _, val := range []string{statementPGBig, statementPGSmall, statementPGGold} {
			s.SetStatementSummary(true)
			with := statementPGRequest(t, s, statementPGPath+val+query)
			s.SetStatementSummary(false)
			without := statementPGRequest(t, s, statementPGPath+val+query)
			s.SetStatementSummary(true)
			require.Equal(t, http.StatusOK, with.Code, with.Body.String())
			require.Equal(t, without.Body.String(), with.Body.String(), "%s %s", val, query)
		}
	}
	s.SetStatementSummary(false)
	require.Equal(t, "live rows only (summary turned off)", summaryPlan(t, s, statementPGBig).describe())
	s.SetStatementSummary(true)

	// The row limit counts only what is still read from taxable_events.
	s.SetStatementLimits(1000, 0)
	rec := statementPGRequest(t, s, statementPGPath+statementPGBig)
	require.Equal(t, http.StatusOK, rec.Code, "all time fits once finished months come from the summary: %s", rec.Body.String())
	s.SetStatementSummary(false)
	rec = statementPGRequest(t, s, statementPGPath+statementPGBig)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	s.SetStatementSummary(true)
	s.SetStatementLimits(0, 0)

	// Dropping the summary puts every read back on taxable_events.
	require.NoError(t, DropStatementSummary(ctx, s.db, 0))
	require.Equal(t, "live rows only (not installed)", summaryPlan(t, s, statementPGBig).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "dropped"))
	require.Zero(t, summaryCount(t, s.db, "SELECT count(*) FROM pg_trigger WHERE tgrelid = 'taxable_events'::regclass AND NOT tgisinternal"))
}

// Every way history can be rewritten in a built month: the month is read
// live from the moment the change commits, and the refresh makes the summary
// equal again. A change that rolls back leaves nothing behind.
func TestStatementSummaryHistoryRewritesPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big, small := statementPGBig, statementPGSmall

	settle := func(step string, wantDirty ...string) {
		t.Helper()
		require.Equal(t, wantDirty, summaryDirtyCells(t, s), "%s: changed months in the log", step)
		requireSummaryEqualsBase(t, s, step+", before refresh")
		ref, err := RefreshStatementSummary(ctx, s.db, summaryOpts(now))
		require.NoError(t, err, step)
		require.Equal(t, len(wantDirty), ref.Cells, step)
		require.Empty(t, summaryDirtyCells(t, s), step)
		require.Greater(t, requireSummaryEqualsBase(t, s, step+", after refresh"), 20)
		requireSummaryCheck(t, s, step)
	}

	// Delete.
	n := summaryExec(t, s.db, "DELETE FROM taxable_events WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-03-01' AND timestamp < '2025-03-11'", big)
	require.Positive(t, n)
	plan := summaryPlan(t, s, big)
	require.Equal(t, []string{"2025-03-01"}, plan.Dirty)
	require.Len(t, plan.Live, 2, "the changed month and the open tail are read live")
	settle("delete", big+" 2025-03")

	// Update of an amount, a recipient and a category.
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET amount = (amount::numeric + 1)::text WHERE validator_address = ? AND category = 'commission' AND timestamp >= '2025-05-01' AND timestamp < '2025-06-01'", big))
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET to_addr = 'cosmos1renamed' WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-07-01' AND timestamp < '2025-07-04'", small))
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET category = 'delegate' WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-08-01' AND timestamp < '2025-08-03'", big))
	settle("update", big+" 2025-05", big+" 2025-08", small+" 2025-07")

	// An update that changes nothing a statement reads logs nothing.
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET detail = 'x', amount = amount WHERE validator_address = ? AND timestamp >= '2025-02-01' AND timestamp < '2025-03-01'", big))
	require.Empty(t, summaryDirtyCells(t, s))

	// A repair that moves rows to another month, and rows to another validator.
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET timestamp = timestamp + interval '40 days' WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-01-10' AND timestamp < '2025-01-12'", big))
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET validator_address = ? WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-04-10' AND timestamp < '2025-04-12'", small, big))
	settle("move", big+" 2025-01", big+" 2025-02", big+" 2025-04", small+" 2025-04")

	// A row moved from a built month into the open month marks only the built one.
	require.Equal(t, int64(1), summaryExec(t, s.db, "UPDATE taxable_events SET timestamp = '2026-11-05T00:00:00Z' WHERE id = (SELECT min(id) FROM taxable_events WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-12-01' AND timestamp < '2026-01-01')", small))
	settle("move to the open month", small+" 2025-12")

	// Delete and re-insert of the same rows in one transaction, as a repair does.
	require.NoError(t, s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE TEMP TABLE reinsert ON COMMIT DROP AS SELECT * FROM taxable_events WHERE validator_address = ? AND timestamp >= '2025-09-01' AND timestamp < '2025-09-08'", big).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM taxable_events WHERE id IN (SELECT id FROM reinsert)").Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO taxable_events SELECT * FROM reinsert").Error
	}))
	settle("re-insert", big+" 2025-09")

	// The indexer's write: one upsert per event inside a savepoint, then the
	// delete of the message's surplus events.
	upsert := "INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, to_addr, timestamp, validator_address) VALUES (?,?,'reward',?,'uatom','cosmos1upsert',?,?) ON CONFLICT (message_id, sub_index) DO UPDATE SET amount = EXCLUDED.amount, to_addr = EXCLUDED.to_addr, timestamp = EXCLUDED.timestamp, validator_address = EXCLUDED.validator_address, category = EXCLUDED.category"
	message := func(amount string) {
		t.Helper()
		require.NoError(t, s.db.Transaction(func(tx *gorm.DB) error {
			return tx.Transaction(func(sp *gorm.DB) error {
				for sub := 0; sub < 3; sub++ {
					if err := sp.Exec(upsert, 900001, sub, amount, "2025-06-20T10:00:00Z", big).Error; err != nil {
						return err
					}
				}
				return sp.Exec("DELETE FROM taxable_events WHERE message_id = ? AND sub_index >= ?", 900001, 3).Error
			})
		}))
	}
	message("17")
	settle("upsert insert", big+" 2025-06")
	message("17")
	require.Empty(t, summaryDirtyCells(t, s), "re-indexing a message to the same values logs nothing")
	message("18")
	settle("upsert update", big+" 2025-06")
	require.Equal(t, int64(2), summaryExec(t, s.db, "DELETE FROM taxable_events WHERE message_id = ? AND sub_index >= ?", 900001, 1))
	settle("surplus delete", big+" 2025-06")

	// Rows of other categories and rows without a validator are not logged.
	summaryExec(t, s.db, "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('delegate','5','uatom','cosmos1x','2025-06-20T00:00:00Z',?), ('reward','5','uatom','cosmos1x','2025-06-20T00:00:00Z','')", big)
	require.Empty(t, summaryDirtyCells(t, s))

	// A new validator that first appears in a built month.
	fresh := flowPGValoper(15)
	summaryExec(t, s.db, "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward','5','uatom','cosmos1x','2025-06-20T00:00:00Z',?), ('commission','6','uatom',NULL,'2025-06-21T00:00:00Z',?)", fresh, fresh)
	read, err := s.readStatement(ctx, fresh, time.Time{}, now, true)
	require.NoError(t, err)
	require.Len(t, read.Agg.Days, 2)
	require.Equal(t, int64(1), read.Agg.Delegators)
	settle("new validator", fresh+" 2025-06")
	read, err = s.readStatement(ctx, fresh, time.Time{}, now, true)
	require.NoError(t, err)
	require.True(t, read.Plan.Summary)
	require.Len(t, read.Agg.Days, 2)
	require.Equal(t, int64(1), read.Agg.Delegators)
	// Its last rows deleted again: the summary must not keep them.
	summaryExec(t, s.db, "DELETE FROM taxable_events WHERE validator_address = ?", fresh)
	settle("validator emptied", fresh+" 2025-06")
	require.Zero(t, summaryCount(t, s.db, "SELECT (SELECT count(*) FROM statement_days WHERE validator_address = ?) + (SELECT count(*) FROM statement_delegators WHERE validator_address = ?)", fresh, fresh))

	// A change that rolls back, whole or to a savepoint, leaves no log row,
	// no summary change, and nothing missed.
	logged := summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty")
	failures := summaryCount(t, s.db, "SELECT COALESCE(pg_sequence_last_value('statement_log_failures'::regclass), 0)")
	rollback := fmt.Errorf("rolled back")
	require.ErrorIs(t, s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM taxable_events WHERE validator_address = ? AND timestamp >= '2025-10-01' AND timestamp < '2025-11-01'", big).Error; err != nil {
			return err
		}
		var seen int64
		if err := tx.Raw("SELECT count(*) FROM statement_dirty").Row().Scan(&seen); err != nil {
			return err
		}
		if seen == 0 {
			return fmt.Errorf("the open transaction must see its own log row")
		}
		return rollback
	}), rollback)
	require.NoError(t, s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.Transaction(func(sp *gorm.DB) error {
			if err := sp.Exec("UPDATE taxable_events SET amount = '1' WHERE validator_address = ? AND timestamp >= '2025-10-01' AND timestamp < '2025-11-01'", big).Error; err != nil {
				return err
			}
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		return nil
	}))
	require.Equal(t, logged, summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty"))
	require.Equal(t, failures, summaryCount(t, s.db, "SELECT COALESCE(pg_sequence_last_value('statement_log_failures'::regclass), 0)"))
	require.True(t, summaryPlan(t, s, big).Summary)
	settle("rollback")

	// Truncate: nothing in the summary can be used until it is rebuilt.
	summaryExec(t, s.db, "CREATE TEMP TABLE keep AS SELECT * FROM taxable_events")
	summaryExec(t, s.db, "TRUNCATE taxable_events")
	require.Equal(t, "live rows only (taxable_events was truncated since the last build)", summaryPlan(t, s, big).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "truncated"))
	summaryExec(t, s.db, "INSERT INTO taxable_events SELECT * FROM keep WHERE id % 3 <> 0")
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "reloaded after truncate"))
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(now))
	require.ErrorContains(t, err, "truncated", "a refresh does not rebuild on its own")
	_, err = CheckStatementSummary(ctx, s.db, 0, 1, StatementSummaryOptions{})
	require.ErrorContains(t, err, "truncated")
	_, err = FillStatementSummary(ctx, s.db, true, summaryOpts(now))
	require.NoError(t, err)
	require.Empty(t, summaryDirtyCells(t, s))
	require.Greater(t, requireSummaryEqualsBase(t, s, "rebuilt after truncate"), 20)
	requireSummaryCheck(t, s, "rebuilt after truncate")
}

// Writes that race the refresh Job and the month advance.
func TestStatementSummaryConcurrentWritesPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	peer := summaryPGPeer(t, s)
	built := summaryDay(2025, 9, 15)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(built))
	require.NoError(t, err)
	big := statementPGBig
	insert := "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward',?,'uatom',?,?,?)"

	// A write that commits while the refresh is between recomputing the
	// month and removing its log rows: the month stays marked.
	summaryExec(t, s.db, insert, "100", "cosmos1first", "2025-05-05T00:00:00Z", big)
	summaryRefreshHook = func() {
		require.NoError(t, peer.Exec(insert, "200", "cosmos1during", "2025-05-06T00:00:00Z", big).Error)
	}
	ref, err := RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	summaryRefreshHook = nil
	require.NoError(t, err)
	require.Equal(t, 1, ref.Cells)
	require.Equal(t, []string{big + " 2025-05"}, summaryDirtyCells(t, s), "the write made during the refresh is still in the log")
	require.Equal(t, []string{"2025-05-01"}, summaryPlan(t, s, big).Dirty)
	requireSummaryEqualsBase(t, s, "write during refresh")
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	require.NoError(t, err)
	require.Empty(t, summaryDirtyCells(t, s))
	requireSummaryEqualsBase(t, s, "after the next refresh")
	requireSummaryCheck(t, s, "after the next refresh")

	// A write still uncommitted when the refresh runs: its log row is not
	// visible to the refresh, so it survives, and is honoured on commit.
	tx := peer.Begin()
	require.NoError(t, tx.Exec(insert, "300", "cosmos1open", "2025-04-06T00:00:00Z", big).Error)
	ref, err = RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	require.NoError(t, err)
	require.Zero(t, ref.Cells)
	require.True(t, summaryPlan(t, s, big).Summary)
	require.Empty(t, summaryPlan(t, s, big).Dirty, "an uncommitted change is not visible to readers")
	requireSummaryEqualsBase(t, s, "uncommitted write")
	require.NoError(t, tx.Commit().Error)
	require.Equal(t, []string{"2025-04-01"}, summaryPlan(t, s, big).Dirty)
	requireSummaryEqualsBase(t, s, "committed after the refresh")
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	require.NoError(t, err)
	requireSummaryCheck(t, s, "committed after the refresh")

	// A month that already carries a mark gets no second one. A writer that
	// relied on the existing mark and is still open when the refresh runs
	// must not lose its change: the refresh claims the mark, waits for that
	// writer, and only recomputes once it has ended.
	summaryExec(t, s.db, insert, "310", "cosmos1marked", "2025-03-06T00:00:00Z", big)
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty"))
	relying := peer.Begin()
	require.NoError(t, relying.Exec(insert, "320", "cosmos1relying", "2025-03-07T00:00:00Z", big).Error)
	require.Equal(t, int64(1), summaryCount(t, relying, "SELECT count(*) FROM statement_dirty"), "the open writer added no mark of its own")
	short := summaryOpts(built)
	short.DrainTimeout = time.Second
	ref, err = RefreshStatementSummary(ctx, s.db, short)
	require.NoError(t, err)
	require.Contains(t, ref.Waiting, "still open")
	require.Zero(t, ref.Cells)
	require.Equal(t, []string{"2025-03-01"}, summaryPlan(t, s, big).Dirty, "the month stays marked while the refresh waits")
	// Another writer meanwhile finds only a claimed mark and adds its own.
	summaryExec(t, s.db, insert, "330", "cosmos1afterclaim", "2025-03-08T00:00:00Z", big)
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty WHERE NOT claimed"))
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty WHERE claimed"))
	require.NoError(t, relying.Commit().Error)
	requireSummaryEqualsBase(t, s, "relying writer committed, not refreshed")
	ref, err = RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	require.NoError(t, err)
	require.Empty(t, ref.Waiting)
	require.Equal(t, 1, ref.Cells)
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_delegators WHERE to_addr = 'cosmos1relying'"), "the relying writer's row is in the recomputed month")
	require.Equal(t, []string{big + " 2025-03"}, summaryDirtyCells(t, s), "the mark added after the claim is kept for the next run")
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(built))
	require.NoError(t, err)
	require.Empty(t, summaryDirtyCells(t, s))
	requireSummaryEqualsBase(t, s, "relying writer refreshed")
	requireSummaryCheck(t, s, "relying writer refreshed")

	// Month advance against a transaction that began before the change log
	// covered the new months. It holds a snapshot in which those months are
	// not logged yet and writes into one of them. The advance must wait for
	// it, then build from a snapshot that contains its row.
	later := summaryDay(2026, 11, 13)
	old := peer.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.Equal(t, int64(0), summaryCount(t, old, "SELECT count(*) FROM statement_dirty"))
	done := make(chan error, 1)
	var advanced StatementSummaryRefreshResult
	go func() {
		var err error
		advanced, err = RefreshStatementSummary(ctx, s.db, summaryOpts(later))
		done <- err
	}()
	require.Eventually(t, func() bool {
		var before time.Time
		return peer.Raw("SELECT log_before FROM statement_state").Row().Scan(&before) == nil && before.Equal(summaryDay(2026, 11, 1))
	}, 10*time.Second, 50*time.Millisecond, "the change log covers the new months first")
	require.NoError(t, old.Exec(insert, "400", "cosmos1oldsnapshot", "2025-10-10T00:00:00Z", big).Error)
	require.Equal(t, int64(0), summaryCount(t, old, "SELECT count(*) FROM statement_dirty"), "the old snapshot did not log its write")
	select {
	case err := <-done:
		t.Fatalf("the advance did not wait for the open transaction: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	require.Equal(t, summaryDay(2025, 9, 1), monthFloor(*mustTrust(t, peer).BuiltBefore), "nothing is built while it waits")
	require.NoError(t, old.Commit().Error)
	require.NoError(t, <-done)
	require.True(t, advanced.Advanced)
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_delegators WHERE to_addr = 'cosmos1oldsnapshot' AND month = '2025-10-01'"), "the unlogged row is in the build")
	requireSummaryEqualsBase(t, s, "after the advance")
	requireSummaryCheck(t, s, "after the advance")

	// The wait is bounded: while the transaction stays open nothing is
	// built, and the next run goes on waiting for the same transactions.
	summaryExec(t, s.db, "UPDATE statement_state SET built_before = '2025-09-01', log_before = '2025-09-01'")
	summaryExec(t, s.db, "DELETE FROM statement_days WHERE day >= '2025-09-01'")
	summaryExec(t, s.db, "DELETE FROM statement_delegators WHERE month >= '2025-09-01'")
	stuck := peer.Begin()
	require.NoError(t, stuck.Exec(insert, "500", "cosmos1stuck", "2026-11-12T00:00:00Z", big).Error)
	opts := summaryOpts(later)
	opts.DrainTimeout = time.Second
	ref, err = RefreshStatementSummary(ctx, s.db, opts)
	require.NoError(t, err)
	require.Contains(t, ref.Waiting, "still open")
	require.False(t, ref.Advanced)
	require.Equal(t, summaryDay(2025, 9, 1), monthFloor(*mustTrust(t, s.db).BuiltBefore))
	require.NoError(t, stuck.Rollback().Error)
	requireSummaryEqualsBase(t, s, "advance timed out")
	ref, err = RefreshStatementSummary(ctx, s.db, summaryOpts(later))
	require.NoError(t, err)
	require.True(t, ref.Advanced)
	requireSummaryEqualsBase(t, s, "advance retried")
	requireSummaryCheck(t, s, "advance retried")

	// Only one fill or refresh runs at a time.
	require.NoError(t, peer.Exec("SELECT pg_advisory_lock(?)", statementSummaryLockKey).Error)
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(later))
	require.ErrorIs(t, err, ErrStatementSummaryBusy)
	require.NoError(t, peer.Exec("SELECT pg_advisory_unlock(?)", statementSummaryLockKey).Error)
}

func mustTrust(t *testing.T, db *gorm.DB) summaryTrust {
	t.Helper()
	trust, err := readSummaryTrust(db)
	require.NoError(t, err)
	return trust
}

// The change log must never fail the write to taxable_events. When it
// cannot be written the write goes through and the summary is not used
// again until it is rebuilt.
func TestStatementSummaryLogFailurePostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	peer := summaryPGPeer(t, s)
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big := statementPGBig
	insert := "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward',?,'uatom',?,?,?)"
	rows := func() int64 { return summaryCount(t, s.db, "SELECT count(*) FROM taxable_events") }
	before := rows()

	// The log table is locked by someone else: the writer waits the
	// function's two seconds, then its row is written without a log row.
	lock := peer.Begin()
	require.NoError(t, lock.Exec("LOCK TABLE statement_dirty IN ACCESS EXCLUSIVE MODE").Error)
	began := time.Now()
	summaryExec(t, s.db, insert, "1", "cosmos1blocked", "2025-05-05T00:00:00Z", big)
	waited := time.Since(began)
	require.Greater(t, waited, 1900*time.Millisecond)
	require.Less(t, waited, 4*time.Second)
	// A row for the open month needs no mark and does not wait at all.
	began = time.Now()
	summaryExec(t, s.db, insert, "1", "cosmos1tip", "2026-11-12T00:00:00Z", big)
	require.Less(t, time.Since(began), time.Second)
	// A writer whose own statement timeout is shorter than that wait is
	// cancelled by it, as it would be by any lock it waits on.
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '300ms'").Error; err != nil {
			return err
		}
		return tx.Exec(insert, "1", "cosmos1cancelled", "2025-05-05T00:00:00Z", big).Error
	})
	require.ErrorContains(t, err, "57014")
	require.NoError(t, lock.Rollback().Error)
	require.Equal(t, before+2, rows())
	trust := mustTrust(t, s.db)
	require.Equal(t, int64(1), trust.Failures-trust.Acked, "the write that could not be marked is counted")
	require.Equal(t, "live rows only (the change log failed 1 time(s) since the last build)", summaryPlan(t, s, big).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "log blocked"))
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(now))
	require.ErrorContains(t, err, "change log failed")
	status, err := ReadStatementSummaryStatus(ctx, s.db)
	require.NoError(t, err)
	require.False(t, status.Trusted)
	require.Equal(t, int64(1), status.Failures)
	require.Contains(t, status.Line(), "trusted=false")
	require.Contains(t, status.Line(), "log_failures=1")

	_, err = FillStatementSummary(ctx, s.db, true, summaryOpts(now))
	require.NoError(t, err)
	require.Greater(t, requireSummaryEqualsBase(t, s, "rebuilt"), 20)
	requireSummaryCheck(t, s, "rebuilt")

	// The counter survives the writer's rollback, so a failure is never
	// forgotten (a rolled back write then costs a rebuild it did not need).
	lock = peer.Begin()
	require.NoError(t, lock.Exec("LOCK TABLE statement_dirty IN ACCESS EXCLUSIVE MODE").Error)
	failed := fmt.Errorf("rolled back")
	require.ErrorIs(t, s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(insert, "1", "cosmos1gone", "2025-05-05T00:00:00Z", big).Error; err != nil {
			return err
		}
		return failed
	}), failed)
	require.NoError(t, lock.Rollback().Error)
	require.False(t, summaryPlan(t, s, big).Summary)
	_, err = FillStatementSummary(ctx, s.db, true, summaryOpts(now))
	require.NoError(t, err)

	// The log table is gone altogether (dropped by hand): writes of every
	// shape still succeed.
	summaryExec(t, s.db, "DROP TABLE statement_dirty")
	summaryExec(t, s.db, insert, "1", "cosmos1nolog", "2025-05-05T00:00:00Z", big)
	require.Positive(t, summaryExec(t, s.db, "UPDATE taxable_events SET amount = '2' WHERE to_addr = 'cosmos1nolog'"))
	require.Positive(t, summaryExec(t, s.db, "DELETE FROM taxable_events WHERE to_addr = 'cosmos1nolog'"))
	require.Equal(t, "live rows only (not installed)", summaryPlan(t, s, big).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "log table dropped"))
	// The state row is gone too: same.
	summaryExec(t, s.db, "DROP TABLE statement_state")
	summaryExec(t, s.db, insert, "1", "cosmos1nostate", "2025-05-05T00:00:00Z", big)
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "state table dropped"))

	// Installing again does not bring the old summary back into use.
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	require.False(t, summaryPlan(t, s, big).Summary)
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	require.Greater(t, requireSummaryEqualsBase(t, s, "installed and built again"), 20)
	requireSummaryCheck(t, s, "installed and built again")
}

// What the API refuses to trust: triggers switched off or dropped, a table
// swapped in under the same name, child tables. And what keeps working:
// rewrites of the table and replica-mode sessions.
func TestStatementSummaryTrustPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big := statementPGBig
	insert := "INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward',?,'uatom',?,?,?)"
	refresh := func(step string) {
		t.Helper()
		_, err := RefreshStatementSummary(ctx, s.db, summaryOpts(now))
		require.NoError(t, err, step)
		require.Greater(t, requireSummaryEqualsBase(t, s, step), 20)
		requireSummaryCheck(t, s, step)
	}

	// Rewrites keep the triggers and the table's identity.
	summaryExec(t, s.db, "ALTER TABLE taxable_events ADD COLUMN note text DEFAULT 'n'")
	summaryExec(t, s.db, "ALTER TABLE taxable_events ALTER COLUMN block_height TYPE numeric")
	summaryExec(t, s.db, "VACUUM FULL taxable_events")
	summaryExec(t, s.db, "CLUSTER taxable_events USING taxable_events_pkey")
	require.True(t, summaryPlan(t, s, big).Summary, summaryPlan(t, s, big).describe())
	require.Empty(t, summaryDirtyCells(t, s), "a rewrite is not a row change")
	summaryExec(t, s.db, insert, "1", "cosmos1afterrewrite", "2025-05-05T00:00:00Z", big)
	require.Equal(t, []string{big + " 2025-05"}, summaryDirtyCells(t, s))
	refresh("after rewrites")
	summaryExec(t, s.db, "ALTER TABLE taxable_events DROP COLUMN note")

	// A session in replica mode (logical replication apply, some restore
	// tools) still fires the triggers.
	require.NoError(t, s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL session_replication_role = replica").Error; err != nil {
			return err
		}
		return tx.Exec(insert, "1", "cosmos1replica", "2025-06-05T00:00:00Z", big).Error
	}))
	require.Equal(t, []string{big + " 2025-06"}, summaryDirtyCells(t, s))
	refresh("replica mode write")

	// A trigger switched off: not trusted. Switching it back on by hand
	// (origin mode, not ALWAYS) is not trusted either, and an install after
	// that discards the summary, because changes may have been missed.
	summaryExec(t, s.db, "ALTER TABLE taxable_events DISABLE TRIGGER statement_log_delete")
	require.Equal(t, "live rows only (3 of 4 change triggers are present and enabled)", summaryPlan(t, s, big).describe())
	require.Positive(t, summaryExec(t, s.db, "DELETE FROM taxable_events WHERE validator_address = ? AND timestamp >= '2025-07-01' AND timestamp < '2025-07-05'", big))
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "trigger off"))
	summaryExec(t, s.db, "ALTER TABLE taxable_events ENABLE TRIGGER statement_log_delete")
	require.False(t, summaryPlan(t, s, big).Summary)
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(now))
	require.ErrorContains(t, err, "change triggers")
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	require.Equal(t, "live rows only (not built)", summaryPlan(t, s, big).describe())
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	refresh("rebuilt after a trigger was off")

	// A healthy install run again (to update the function) keeps the summary.
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	require.True(t, summaryPlan(t, s, big).Summary)

	// A trigger dropped.
	summaryExec(t, s.db, "DROP TRIGGER statement_log_truncate ON taxable_events")
	require.False(t, summaryPlan(t, s, big).Summary)
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	require.Equal(t, "live rows only (not built)", summaryPlan(t, s, big).describe())
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)

	// A child table: rows written to it show up in reads of taxable_events
	// without firing its statement triggers.
	summaryExec(t, s.db, "CREATE TABLE taxable_events_child () INHERITS (taxable_events)")
	summaryExec(t, s.db, "INSERT INTO taxable_events_child (category, amount, denom, to_addr, timestamp, validator_address) VALUES ('reward','9','uatom','cosmos1child','2025-05-05T00:00:00Z',?)", big)
	require.Empty(t, summaryDirtyCells(t, s), "the write to the child was not logged")
	require.Equal(t, "live rows only (taxable_events is partitioned or has child tables)", summaryPlan(t, s, big).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "child table"))
	_, err = FillStatementSummary(ctx, s.db, true, summaryOpts(now))
	require.ErrorContains(t, err, "child tables")
	summaryExec(t, s.db, "DROP TABLE taxable_events_child")
	require.True(t, summaryPlan(t, s, big).Summary)

	// A migration that builds a new table and renames it into place: the
	// new table has no triggers and is not the table the summary was built
	// from. Both are checked; here the triggers are even put back by an
	// install, and the summary is still discarded.
	summaryExec(t, s.db, "CREATE TABLE taxable_events_new (LIKE taxable_events INCLUDING ALL)")
	summaryExec(t, s.db, "INSERT INTO taxable_events_new SELECT * FROM taxable_events WHERE id % 2 = 0")
	summaryExec(t, s.db, "ALTER TABLE taxable_events RENAME TO taxable_events_old")
	summaryExec(t, s.db, "ALTER TABLE taxable_events_new RENAME TO taxable_events")
	require.Equal(t, "live rows only (taxable_events was replaced since the summary was built)", summaryPlan(t, s, big).describe())
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "table swapped"))
	_, err = RefreshStatementSummary(ctx, s.db, summaryOpts(now))
	require.ErrorContains(t, err, "replaced")
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	require.Equal(t, "live rows only (not built)", summaryPlan(t, s, big).describe())
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	refresh("rebuilt on the new table")

	// Partitioned from the start: the install works on the parent, but rows
	// can be written straight to a partition, so the summary is never used.
	summaryExec(t, s.db, "ALTER TABLE taxable_events RENAME TO taxable_events_plain")
	summaryExec(t, s.db, "CREATE TABLE taxable_events (LIKE taxable_events_plain) PARTITION BY RANGE (timestamp)")
	summaryExec(t, s.db, "CREATE TABLE taxable_events_p1 PARTITION OF taxable_events FOR VALUES FROM (MINVALUE) TO ('2025-07-01')")
	summaryExec(t, s.db, "CREATE TABLE taxable_events_p2 PARTITION OF taxable_events FOR VALUES FROM ('2025-07-01') TO (MAXVALUE)")
	summaryExec(t, s.db, "INSERT INTO taxable_events SELECT * FROM taxable_events_plain")
	require.NoError(t, InstallStatementSummary(ctx, s.db, 0, 1, t.Logf))
	_, err = FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.ErrorContains(t, err, "partitioned")
	require.Equal(t, "live rows only (not built)", summaryPlan(t, s, big).describe())
	summaryExec(t, s.db, "INSERT INTO taxable_events (id, category, amount, denom, to_addr, timestamp, validator_address) VALUES (99999901,'reward','9','uatom','cosmos1viaparent','2025-05-05T00:00:00Z',?)", big)
	summaryExec(t, s.db, "INSERT INTO taxable_events_p1 (id, category, amount, denom, to_addr, timestamp, validator_address) VALUES (99999902,'reward','9','uatom','cosmos1direct','2025-05-05T00:00:00Z',?)", big)
	require.Equal(t, []string{big + " 2025-05"}, summaryDirtyCells(t, s))
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty"), "the write through the parent is logged, the write straight to the partition is not")
	require.Equal(t, 0, requireSummaryEqualsBase(t, s, "partitioned"))
}
