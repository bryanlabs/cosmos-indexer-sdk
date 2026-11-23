package taxapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// summaryChangeMonths rewrites the amounts of val's reward and commission
// rows in [from, to), which marks those months changed, and returns how many
// rows it touched.
func summaryChangeMonths(t *testing.T, db *gorm.DB, val string, from, to time.Time) int64 {
	t.Helper()
	n := summaryExec(t, db, "UPDATE taxable_events SET amount = (amount::numeric + 1)::text WHERE validator_address = ? AND category IN ('reward','commission') AND timestamp >= ? AND timestamp < ?", val, from, to)
	require.Positive(t, n)
	return n
}

// summaryRefreshLog collects the validators a refresh recomputed, in order.
type summaryRefreshLog struct {
	mu    sync.Mutex
	order []string
}

var summaryRefreshLine = regexp.MustCompile(`^statement-summary refresh (\S+): (\d+) changed month\(s\) recomputed`)

func (l *summaryRefreshLog) opts(now time.Time) StatementSummaryOptions {
	o := summaryOpts(now)
	o.Logf = func(format string, args ...any) {
		line := strings.TrimSpace(fmt.Sprintf(format, args...))
		if m := summaryRefreshLine.FindStringSubmatch(line); m != nil {
			l.mu.Lock()
			l.order = append(l.order, m[1])
			l.mu.Unlock()
		}
	}
	return o
}

func (l *summaryRefreshLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.order
	l.order = nil
	return out
}

// bySize orders validators by the rows each changed, largest first.
func bySize(changed map[string]int64) []string {
	out := make([]string, 0, len(changed))
	for v := range changed {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if changed[out[i]] != changed[out[j]] {
			return changed[out[i]] > changed[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// The refresh recomputes the validator whose changed months hold the most
// rows first, whatever the address order, and still recomputes every claimed
// month exactly as before.
func TestStatementSummaryRefreshLargestFirstPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big, small := statementPGBig, statementPGSmall
	alphabetical := []string{big, small}
	sort.Strings(alphabetical)
	var log summaryRefreshLog
	seenNonAlphabetical := false

	for _, scenario := range []struct {
		name               string
		bigFrom, bigTo     time.Time
		smallFrom, smallTo time.Time
	}{
		{"big changed most", summaryDay(2025, 1, 1), summaryDay(2025, 7, 1), summaryDay(2025, 3, 1), summaryDay(2025, 4, 1)},
		{"small changed most", summaryDay(2025, 5, 1), summaryDay(2025, 6, 1), summaryDay(2025, 1, 1), summaryDay(2026, 1, 1)},
	} {
		changed := map[string]int64{
			big:   summaryChangeMonths(t, s.db, big, scenario.bigFrom, scenario.bigTo),
			small: summaryChangeMonths(t, s.db, small, scenario.smallFrom, scenario.smallTo),
		}
		want := bySize(changed)
		if want[0] != alphabetical[0] {
			seenNonAlphabetical = true
		}
		cells := len(summaryDirtyCells(t, s))
		ref, err := RefreshStatementSummary(ctx, s.db, log.opts(now))
		require.NoError(t, err, scenario.name)
		require.Equal(t, want, log.take(), "%s: largest changed first", scenario.name)
		require.Equal(t, cells, ref.Cells, scenario.name)
		require.Equal(t, 2, ref.Validators, scenario.name)
		require.Empty(t, summaryDirtyCells(t, s), scenario.name)
		require.Greater(t, requireSummaryEqualsBase(t, s, scenario.name), 20)
		requireSummaryCheck(t, s, scenario.name)
	}
	require.True(t, seenNonAlphabetical, "one scenario must order against the address order")
}

// A run stopped mid-way (the Job's deadline) leaves its claimed months and
// its claim. The next run takes exactly those first, largest first,
// including the validator whose recompute was rolled back, and leaves a
// month changed after the claim for the run after it.
func TestStatementSummaryRefreshAfterStoppedRunPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	peer := summaryPGPeer(t, s)
	// The stopped run's session is closed by the cancel; a session whose DSN
	// carries the schema reconnects into it.
	runner := summaryPGPeer(t, s)
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big, small, gold := statementPGBig, statementPGSmall, statementPGGold
	changed := map[string]int64{
		big:   summaryChangeMonths(t, s.db, big, summaryDay(2025, 2, 1), summaryDay(2025, 3, 1)),
		small: summaryChangeMonths(t, s.db, small, summaryDay(2025, 1, 1), summaryDay(2026, 1, 1)),
	}
	want := bySize(changed)
	claimedCells := len(summaryDirtyCells(t, s))

	// The first run is stopped inside its first validator's recompute, after
	// a writer changed a month of another validator.
	stopped, stop := context.WithCancel(ctx)
	calls := 0
	summaryRefreshHook = func() {
		calls++
		if calls == 1 {
			require.NoError(t, peer.Exec("UPDATE taxable_events SET amount = (amount::numeric + 1)::text WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-04-01' AND timestamp < '2025-04-02'", gold).Error)
			stop()
		}
	}
	var log summaryRefreshLog
	_, err = RefreshStatementSummary(stopped, runner, log.opts(now))
	summaryRefreshHook = nil
	require.ErrorContains(t, err, "canceled")
	require.Empty(t, log.take(), "nothing was recomputed")
	require.Equal(t, int64(claimedCells), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty WHERE claimed"), "the stopped run's claims stay")
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty WHERE NOT claimed"), "the later change waits unclaimed")
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_state WHERE claim_horizon IS NOT NULL"))
	requireSummaryEqualsBase(t, s, "after the stopped run")

	ref, err := RefreshStatementSummary(ctx, runner, log.opts(now))
	require.NoError(t, err)
	require.Equal(t, want, log.take(), "the stopped run's claims, largest first")
	require.Equal(t, claimedCells, ref.Cells)
	require.Equal(t, []string{gold + " 2025-04"}, summaryDirtyCells(t, s), "the month changed after the claim is the next run's")
	require.Zero(t, summaryCount(t, s.db, "SELECT count(*) FROM statement_dirty WHERE claimed"))
	requireSummaryEqualsBase(t, s, "after the next run")

	ref, err = RefreshStatementSummary(ctx, runner, log.opts(now))
	require.NoError(t, err)
	require.Equal(t, []string{gold}, log.take())
	require.Equal(t, 1, ref.Cells)
	require.Empty(t, summaryDirtyCells(t, s))
	require.Greater(t, requireSummaryEqualsBase(t, s, "after the run after"), 20)
	requireSummaryCheck(t, s, "after the run after")
}

// A period over the row limit only because of months the refresh is
// recomputing after a correction answers 503 with Retry-After; a period
// over the limit with nothing to recompute, or over it even without the
// changed months, keeps the 422.
func TestValidatorStatementRefreshingPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	big := statementPGBig
	path := statementPGPath + big

	// Built through 2026-10: everything but the open month is in the summary.
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	s.SetStatementLimits(1000, 0)
	rec := statementPGRequest(t, s, path)
	require.Equal(t, http.StatusOK, rec.Code, "clean summary: %s", rec.Body.String())

	summaryChangeMonths(t, s.db, big, summaryDay(2025, 1, 1), summaryDay(2025, 7, 1))
	require.NoError(t, s.db.Exec("ANALYZE taxable_events").Error)
	for _, format := range []string{"json", apiCSV} {
		rec = statementPGRequest(t, s, path+"&format="+format)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		body := rec.Body.String()
		require.Contains(t, body, "being updated after a correction to earlier records")
		require.Contains(t, body, "6 month(s) in the chosen period are being recomputed")
		require.Contains(t, body, "Try again in about ")
		require.NotContains(t, body, "shorter period")
		retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		require.NoError(t, err)
		require.GreaterOrEqual(t, retry, 60)
		require.LessOrEqual(t, retry, 3600)
		require.Zero(t, retry%60)
		require.Contains(t, body, "about "+strconv.Itoa(retry/60)+" minutes")
	}
	// A period without the changed months is not affected.
	rec = statementPGRequest(t, s, path+"&start=2025-07-01&end=2026-11-01")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	ref, err := RefreshStatementSummary(ctx, s.db, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.Equal(t, 6, ref.Cells)
	rec = statementPGRequest(t, s, path)
	require.Equal(t, http.StatusOK, rec.Code, "recomputed: %s", rec.Body.String())
	s.SetStatementLimits(0, 0)
}

func TestValidatorStatementOverLimitCleanPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	big := statementPGBig
	path := statementPGPath + big

	// Built only through 2025-08: the months after it are read live and are
	// over the limit on their own.
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2025, 9, 15)))
	require.NoError(t, err)
	s.SetStatementLimits(1000, 0)
	rec := statementPGRequest(t, s, path)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "clean summary over the limit: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "shorter period")
	require.Empty(t, rec.Header().Get("Retry-After"))

	// Changed months on top: still over without them, so still 422.
	summaryChangeMonths(t, s.db, big, summaryDay(2025, 1, 1), summaryDay(2025, 3, 1))
	read, err := s.readStatement(ctx, big, time.Time{}, summaryDay(2026, 11, 14), false)
	var tooLarge statementTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Len(t, read.Plan.Dirty, 2)
	rec = statementPGRequest(t, s, path)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "shorter period")

	// The summary turned off: everything is live, 422.
	s.SetStatementSummary(false)
	rec = statementPGRequest(t, s, path)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	s.SetStatementSummary(true)
	s.SetStatementLimits(0, 0)
}

// With a time budget a run stops between validators once the budget is used:
// the claimed months left keep their claim, the next run recomputes them
// first, largest first, and only then claims what changed since, in the same
// run while time is left. A run killed at its deadline is picked up the same
// way. Every run recomputes at least one validator.
func TestStatementSummaryRefreshBudgetPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	peer := summaryPGPeer(t, s)
	runner := summaryPGPeer(t, s)
	now := summaryDay(2026, 11, 13)
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(now))
	require.NoError(t, err)
	big, small, gold := statementPGBig, statementPGSmall, statementPGGold
	changed := map[string]int64{
		big:   summaryChangeMonths(t, s.db, big, summaryDay(2025, 1, 1), summaryDay(2025, 4, 1)),
		small: summaryChangeMonths(t, s.db, small, summaryDay(2025, 1, 1), summaryDay(2025, 9, 1)),
		gold:  summaryChangeMonths(t, s.db, gold, summaryDay(2025, 3, 1), summaryDay(2025, 5, 1)),
	}
	want := bySize(changed)
	var log summaryRefreshLog
	spent := log.opts(now)
	spent.RunBudget = time.Nanosecond
	roomy := log.opts(now)
	roomy.RunBudget = time.Hour

	// The budget is used at once: one validator, the largest, then stop.
	ref, err := RefreshStatementSummary(ctx, runner, spent)
	require.NoError(t, err)
	require.Equal(t, want[:1], log.take())
	require.Equal(t, 1, ref.Validators)
	require.Equal(t, 2, ref.Left)
	require.Equal(t, 1, ref.Claims)
	require.Equal(t, int64(1), summaryCount(t, s.db, "SELECT count(*) FROM statement_state WHERE claim_horizon IS NOT NULL"), "the claim is kept")
	requireSummaryEqualsBase(t, s, "after the first budgeted run")

	// A change after the claim waits: the next run takes the claim's
	// leftovers first.
	summaryChangeMonths(t, peer, want[0], summaryDay(2025, 6, 1), summaryDay(2025, 7, 1))
	ref, err = RefreshStatementSummary(ctx, runner, spent)
	require.NoError(t, err)
	require.Equal(t, want[1:2], log.take(), "the leftover claim first, the new change after")
	require.Equal(t, 1, ref.Left)
	require.Equal(t, []string{want[0] + " 2025-06"}, unclaimedCells(t, s))

	// Time left: the rest of the claim, then the change since, in one run.
	ref, err = RefreshStatementSummary(ctx, runner, roomy)
	require.NoError(t, err)
	require.Equal(t, []string{want[2], want[0]}, log.take())
	require.Equal(t, 2, ref.Claims)
	require.Zero(t, ref.Left)
	require.Empty(t, summaryDirtyCells(t, s))
	require.Zero(t, summaryCount(t, s.db, "SELECT count(*) FROM statement_state WHERE claim_horizon IS NOT NULL"))
	require.Greater(t, requireSummaryEqualsBase(t, s, "budgeted runs done"), 20)
	requireSummaryCheck(t, s, "budgeted runs done")

	// A run killed at its deadline inside a validator: the next budgeted run
	// recomputes the killed run's claim first, largest first, then claims
	// the change made meanwhile, so the kill costs no extra cycle.
	changed = map[string]int64{
		big:   summaryChangeMonths(t, s.db, big, summaryDay(2025, 8, 1), summaryDay(2025, 10, 1)),
		small: summaryChangeMonths(t, s.db, small, summaryDay(2025, 2, 1), summaryDay(2025, 3, 1)),
	}
	want = bySize(changed)
	killed, kill := context.WithCancel(ctx)
	calls := 0
	summaryRefreshHook = func() {
		calls++
		if calls == 2 {
			require.NoError(t, peer.Exec("UPDATE taxable_events SET amount = (amount::numeric + 1)::text WHERE validator_address = ? AND category = 'reward' AND timestamp >= '2025-04-01' AND timestamp < '2025-04-02'", gold).Error)
			kill()
		}
	}
	_, err = RefreshStatementSummary(killed, runner, roomy)
	summaryRefreshHook = nil
	require.ErrorContains(t, err, "canceled")
	require.Equal(t, want[:1], log.take(), "the first validator was recomputed before the kill")
	require.Equal(t, []string{gold + " 2025-04"}, unclaimedCells(t, s))
	ref, err = RefreshStatementSummary(ctx, runner, roomy)
	require.NoError(t, err)
	require.Equal(t, []string{want[1], gold}, log.take(), "the killed run's leftover first, then the change made meanwhile")
	require.Equal(t, 2, ref.Claims)
	require.Empty(t, summaryDirtyCells(t, s))
	require.Greater(t, requireSummaryEqualsBase(t, s, "after the killed run"), 20)
	requireSummaryCheck(t, s, "after the killed run")
}

func unclaimedCells(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Raw("SELECT DISTINCT validator_address || ' ' || COALESCE(to_char(month, 'YYYY-MM'), 'all') FROM statement_dirty WHERE NOT claimed ORDER BY 1").Rows()
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}
