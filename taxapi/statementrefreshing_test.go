package taxapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatementRefreshWait(t *testing.T) {
	at := func(hhmmss string) time.Time {
		ts, err := time.Parse(time.DateTime, "2026-11-23 "+hhmmss)
		require.NoError(t, err)
		return ts
	}
	for _, c := range []struct {
		name string
		st   statementRefreshState
		now  time.Time
		want time.Duration
	}{
		{"no run, next slot in 7 min", statementRefreshState{Unclaimed: true, OldestDirty: 2 * time.Minute}, at("02:08:00"), 12 * time.Minute},
		{"no run, slot just passed", statementRefreshState{Unclaimed: true, OldestDirty: time.Minute}, at("02:15:30"), 20 * time.Minute},
		{"running run holds every month", statementRefreshState{ClaimHeld: true, ClaimAge: 4 * time.Minute}, at("02:19:00"), 16 * time.Minute},
		{"running run near its end holds every month", statementRefreshState{ClaimHeld: true, ClaimAge: 19 * time.Minute}, at("02:34:00"), 5 * time.Minute},
		// Began 02:15, ends about 02:35; the 02:30 slot passes meanwhile,
		// so the next run starts as this one ends.
		{"running run, month changed after its claim", statementRefreshState{Unclaimed: true, OldestDirty: time.Minute, ClaimHeld: true, ClaimAge: 5 * time.Minute}, at("02:20:00"), 20 * time.Minute},
		// Began 02:00:30 off a slot (right after the run before), expected
		// to end about 02:20:30, after the 02:15 slot: the next run follows
		// at once.
		{"run begun off a slot", statementRefreshState{Unclaimed: true, ClaimHeld: true, ClaimAge: 8 * time.Minute}, at("02:08:30"), 17 * time.Minute},
		// Past its usual run time: it may end any moment.
		{"run over its usual time", statementRefreshState{Unclaimed: true, ClaimHeld: true, ClaimAge: 25 * time.Minute}, at("02:40:00"), 5 * time.Minute},
		// A claim older than the Job deadline is a stopped run's: the next
		// run continues it.
		{"stopped run's claim", statementRefreshState{ClaimHeld: true, ClaimAge: 45 * time.Minute}, at("02:44:00"), 6 * time.Minute},
		{"refresh held up", statementRefreshState{Unclaimed: true, OldestDirty: 3 * time.Hour}, at("02:44:00"), 35 * time.Minute},
	} {
		got := c.st.wait(c.now)
		require.Equal(t, c.want, got, c.name)
		require.GreaterOrEqual(t, got, time.Minute, c.name)
		require.LessOrEqual(t, got, statementRefreshMaxWait, c.name)
		require.Zero(t, got%time.Minute, c.name)
	}
}

func TestStatementRefreshingMessage(t *testing.T) {
	e := statementRefreshing{Rows: 12_100_000, Limit: 12_000_000, Months: 30, RetryAfter: 20 * time.Minute}
	msg := e.Error()
	require.Contains(t, msg, "being updated after a correction to earlier records")
	require.Contains(t, msg, "30 month(s) in the chosen period")
	require.Contains(t, msg, "Try again in about 20 minutes.")
	require.NotContains(t, msg, "shorter period")
	require.NotContains(t, msg, "—")
}

func TestStatementPlanWithoutDirty(t *testing.T) {
	d := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	open := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	p := statementReadPlan{
		Summary: true, SumLo: d(2021, 3), SumHi: d(2026, 11),
		Dirty: []string{"2022-10-01", "2022-11-01", "2024-05-01"},
		Live: []statementRange{
			{time.Date(2021, 2, 18, 0, 0, 0, 0, time.UTC), d(2021, 3)},
			{d(2022, 10), d(2022, 12)},
			{d(2024, 5), d(2024, 6)},
			{d(2026, 11), open},
		},
	}
	q := p.withoutDirty()
	require.Empty(t, q.Dirty)
	require.Equal(t, []statementRange{{time.Date(2021, 2, 18, 0, 0, 0, 0, time.UTC), d(2021, 3)}, {d(2026, 11), open}}, q.Live)
	require.True(t, q.Summary)
	require.Len(t, p.Live, 4, "the plan itself is unchanged")

	// A live range only partly covered by a changed month keeps the rest.
	p = statementReadPlan{Summary: true, Dirty: []string{d(2025, 3).Format(summaryDayLayout)}, Live: []statementRange{{time.Date(2025, 2, 20, 0, 0, 0, 0, time.UTC), time.Date(2025, 4, 10, 0, 0, 0, 0, time.UTC)}}}
	require.Equal(t, []statementRange{{time.Date(2025, 2, 20, 0, 0, 0, 0, time.UTC), d(2025, 3)}, {d(2025, 4), time.Date(2025, 4, 10, 0, 0, 0, 0, time.UTC)}}, p.withoutDirty().Live)
}
