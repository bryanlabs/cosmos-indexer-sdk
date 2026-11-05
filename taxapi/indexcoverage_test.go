package taxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The fixture chain starts 2024-06-01 with one block a minute, so a UTC day
// is 1440 blocks and 2025 starts at height 308161.
var fixtureGenesis = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

const (
	fixture2025 = int64(308161)
	fixtureTip  = int64(393121)
)

func fixtureTime(h int64) time.Time { return fixtureGenesis.Add(time.Duration(h-1) * time.Minute) }

type fakeRange struct{ lo, hi int64 }

// fakeCoverageSource answers from height ranges instead of rows.
type fakeCoverageSource struct {
	tip     int64
	rows    []fakeRange
	tx      []fakeRange
	ev      []fakeRange
	txCalls map[int64]int
	evCalls int
}

func (f *fakeCoverageSource) Tip(context.Context) (int64, time.Time, bool, error) {
	return f.tip, fixtureTime(f.tip), true, nil
}

func (f *fakeCoverageSource) MinHeight(context.Context) (int64, bool, error) {
	return f.rows[0].lo, true, nil
}

func (f *fakeCoverageSource) BlockAtOrAfter(_ context.Context, h int64) (int64, time.Time, bool, error) {
	for _, r := range f.rows {
		if h <= r.hi {
			h = max(h, r.lo)
			return h, fixtureTime(h), true, nil
		}
	}
	return 0, time.Time{}, false, nil
}

func clipRuns(ranges []fakeRange, lo, hi int64) []heightRun {
	var out []heightRun
	for _, r := range ranges {
		a, b := max(r.lo, lo), min(r.hi, hi)
		if a <= b {
			out = append(out, heightRun{Lo: a, Hi: b, T0: fixtureTime(a), T1: fixtureTime(b)})
		}
	}
	return out
}

func (f *fakeCoverageSource) TxRuns(_ context.Context, lo, hi int64) ([]heightRun, error) {
	if f.txCalls == nil {
		f.txCalls = map[int64]int{}
	}
	f.txCalls[lo]++
	runs := clipRuns(f.tx, lo, hi)
	for i := range runs {
		runs[i].Events = overlap(clipRuns(f.ev, runs[i].Lo, runs[i].Hi), runs[i].Lo, runs[i].Hi)
	}
	return runs, nil
}

func (f *fakeCoverageSource) EventRuns(_ context.Context, lo, hi int64) ([]heightRun, error) {
	f.evCalls++
	return clipRuns(f.ev, lo, hi), nil
}

func fixtureScanner(src *fakeCoverageSource, requireEvents bool) *indexCoverageScanner {
	sc := newIndexCoverageScanner(src, "fixture-1", requireEvents)
	sc.starts = []yearStart{{2024, 1}}
	sc.genesis, sc.genesisDay = 1, fixtureGenesis
	sc.pause = 0
	sc.now = func() time.Time { return fixtureTime(fixtureTip).Add(time.Minute) }
	return sc
}

// A chain indexed end to end except heights 100001-150000 in 2024.
func gappedSource() *fakeCoverageSource {
	all := []fakeRange{{1, fixtureTip}}
	return &fakeCoverageSource{
		tip:  fixtureTip,
		rows: all,
		tx:   []fakeRange{{1, 100000}, {150001, fixtureTip}},
		ev:   []fakeRange{{fixture2025, fixtureTip}},
	}
}

func refreshed(t *testing.T, sc *indexCoverageScanner) *IndexCoverage {
	t.Helper()
	require.NoError(t, sc.Refresh(context.Background()))
	snap := sc.Snapshot()
	require.NotNil(t, snap)
	return snap
}

func TestIndexCoveragePerYear(t *testing.T) {
	snap := refreshed(t, fixtureScanner(gappedSource(), false))
	require.Equal(t, coverageStatusReady, snap.Status)
	require.Equal(t, coverageCriterionTx, snap.Criterion)
	require.False(t, snap.Complete)
	require.Equal(t, "2024-06-01", snap.GenesisDate)
	require.Len(t, snap.Years, 2)

	y24 := snap.Years[0]
	require.Equal(t, 2024, y24.Year)
	require.Equal(t, int64(1), y24.FirstHeight)
	require.Equal(t, fixture2025-1, y24.LastHeight)
	require.Equal(t, int64(308160), y24.ExpectedBlocks)
	require.Equal(t, int64(258160), y24.IndexedBlocks)
	require.Equal(t, 83.7, y24.PercentIndexed)
	require.False(t, y24.Complete)
	require.False(t, y24.InProgress)
	require.Equal(t, 1, y24.GapCount)
	require.Equal(t, CoverageGap{FirstHeight: 100001, LastHeight: 150000, Blocks: 50000, From: "2024-08-09", Through: "2024-09-13"}, y24.Gaps[0])
	require.Equal(t, []CoverageRange{{"2024-06-01", "2024-08-08"}, {"2024-09-14", "2024-12-31"}}, y24.CoveredRanges)
	require.Equal(t, "2024-06-01", *y24.EarliestCoveredDate)
	require.Equal(t, "2024-12-31", *y24.LatestCoveredDate)
	require.Zero(t, y24.BlockEventsIndexedBlocks)

	y25 := snap.Years[1]
	require.Equal(t, fixture2025, y25.FirstHeight)
	require.Equal(t, fixtureTip, y25.LastHeight)
	require.True(t, y25.Complete)
	require.True(t, y25.InProgress)
	require.True(t, y25.BlockEventsComplete)
	require.Equal(t, 100.0, y25.PercentIndexed)
	require.Equal(t, []CoverageRange{{"2025-01-01", "2025-03-01"}}, y25.CoveredRanges)

	require.Equal(t, &IndexedSpan{FirstHeight: 150001, LastHeight: fixtureTip, Blocks: fixtureTip - 150000, From: "2024-09-14", Through: "2025-03-01"}, snap.IndexedRange)
	require.Contains(t, snap.Note, "2024 (83.7%)")
	require.NotContains(t, snap.Note, "—")
}

func TestIndexCoveragePeriod(t *testing.T) {
	snap := refreshed(t, fixtureScanner(gappedSource(), false))
	day := func(s string) time.Time { d, _ := time.Parse(apiDateLayout, s); return d }

	p := snap.Period(day("2025-01-01"), day("2026-01-01"))
	require.True(t, p.Complete)
	require.Equal(t, coverageStatusComplete, p.Status)
	require.Equal(t, "2025-03-01", p.End, "a period past the tip is judged up to the tip")
	require.Empty(t, p.Warning)

	p = snap.Period(day("2024-10-01"), day("2024-11-01"))
	require.True(t, p.Complete, "a month inside a covered range is complete even in a partial year")

	p = snap.Period(day("2024-01-01"), day("2025-01-01"))
	require.False(t, p.Complete)
	require.True(t, p.BeforeGenesis)
	require.Len(t, p.IncompleteYears, 1)
	require.Equal(t, 2024, p.IncompleteYears[0].Year)
	require.Equal(t, 83.7, p.IncompleteYears[0].PercentIndexed)
	require.Contains(t, p.Warning, "2024 is 83.7% indexed")
	require.Contains(t, p.Warning, "genesis (2024-06-01)")

	p = snap.Period(day("2024-08-01"), day("2024-09-01"))
	require.False(t, p.Complete)
	require.Equal(t, coverageStatusIncomplete, p.Status)

	p = snap.Period(time.Time{}, day("2024-08-01"))
	require.True(t, p.Complete, "all history up to a covered day starts at genesis, not at a pre-genesis date")
	require.Equal(t, "2024-06-01", p.Start)
	require.Equal(t, "2024-07-31", p.End)

	p = snap.Period(day("2023-01-01"), day("2024-01-01"))
	require.False(t, p.Complete)
	require.Contains(t, p.Warning, "before fixture-1 genesis")

	p = snap.Period(day("2025-06-01"), day("2025-07-01"))
	require.False(t, p.Complete)
	require.Contains(t, p.Warning, "after the latest indexed block")

	require.True(t, snap.MonthCovered("2024-06"))
	require.False(t, snap.MonthCovered("2024-08"))
	require.False(t, snap.MonthCovered("2024-09"))
	require.True(t, snap.MonthCovered("2024-10"))
	require.True(t, snap.MonthCovered("2025-03"), "the tip month is judged up to the tip")
}

func TestIndexCoverageRequireBlockEvents(t *testing.T) {
	src := gappedSource()
	src.ev = []fakeRange{{fixture2025, fixture2025 + 999}, {fixture2025 + 2000, fixtureTip}}
	snap := refreshed(t, fixtureScanner(src, true))
	require.Equal(t, coverageCriterionTxAndBlock, snap.Criterion)
	y25 := snap.Years[1]
	require.False(t, y25.Complete)
	require.True(t, y25.TxComplete)
	require.Equal(t, y25.ExpectedBlocks-1000, y25.IndexedBlocks)
	require.Equal(t, y25.ExpectedBlocks, y25.TxIndexedBlocks)
	require.Positive(t, src.evCalls, "a bucket with some but not all events gets the event query")
}

func TestIndexCoverageRescansOnlyUnsettledBuckets(t *testing.T) {
	src := gappedSource()
	src.ev = src.tx
	sc := fixtureScanner(src, false)
	refreshed(t, sc)
	first := map[int64]int{}
	for k, v := range src.txCalls {
		first[k] = v
	}
	require.Len(t, first, 4)
	require.Zero(t, src.evCalls, "buckets whose events match their transactions need no event query")

	// Fill the gap: only the gapped bucket is rescanned. The first bucket
	// starts at genesis (height 1), the key the fake records.
	src.tx = []fakeRange{{1, fixtureTip}}
	src.ev = src.tx
	snap := refreshed(t, sc)
	require.Equal(t, 1, src.txCalls[1])
	require.Equal(t, 2, src.txCalls[100000])
	require.Equal(t, 1, src.txCalls[200000])
	require.Equal(t, 1, src.txCalls[300000])
	require.True(t, snap.Complete)
	require.True(t, snap.Years[0].Complete)
	require.Equal(t, int64(1), snap.IndexedRange.FirstHeight)
	require.Contains(t, snap.Note, "Every fixture-1 block from genesis (2024-06-01)")

	// Settled buckets are rechecked once the recheck interval has passed.
	sc.now = func() time.Time { return fixtureTime(fixtureTip).Add(7 * time.Hour) }
	refreshed(t, sc)
	require.Equal(t, 2, src.txCalls[1])

	// A new block at the tip rescans the tip bucket, which has grown.
	src.tip++
	src.tx = []fakeRange{{1, src.tip}}
	src.ev = src.tx
	refreshed(t, sc)
	require.Equal(t, 3, src.txCalls[300000])
	require.Equal(t, 2, src.txCalls[200000])
}

func TestIndexCoverageTipSlack(t *testing.T) {
	src := &fakeCoverageSource{tip: fixtureTip, rows: []fakeRange{{1, fixtureTip}}, tx: []fakeRange{{1, fixtureTip - 50}}}
	snap := refreshed(t, fixtureScanner(src, false))
	require.True(t, snap.Complete, "blocks written but not yet marked indexed at the tip are the live edge")
	require.Equal(t, fixtureTip-50, snap.TipHeight)

	src.tx = []fakeRange{{1, fixtureTip - 5000}}
	snap = refreshed(t, fixtureScanner(src, false))
	require.False(t, snap.Complete, "a long unindexed tail is a gap")
	require.Nil(t, snap.IndexedRange)
}

func TestIndexCoverageFindsLaterYearStart(t *testing.T) {
	sc := fixtureScanner(gappedSource(), false)
	sc.starts = nil
	snap := refreshed(t, sc)
	require.Len(t, snap.Years, 2)
	require.Equal(t, fixture2025, snap.Years[1].FirstHeight)

	// A boundary inside a gap is not guessed: the year folds into the last
	// known one until the boundary blocks are indexed.
	src := gappedSource()
	src.rows = []fakeRange{{1, fixture2025 - 10}, {fixture2025 + 10, fixtureTip}}
	sc = fixtureScanner(src, false)
	sc.starts = nil
	snap = refreshed(t, sc)
	require.Len(t, snap.Years, 1)
}

func TestIndexCoverageHub(t *testing.T) {
	sc := newIndexCoverageScanner(&fakeCoverageSource{}, "cosmoshub", false)
	require.Equal(t, int64(5200791), sc.genesis)
	require.Equal(t, hubGenesisDate, sc.genesisDay)
	require.Equal(t, 2026, sc.starts[len(sc.starts)-1].Year)
	require.Equal(t, int64(29114836), sc.starts[len(sc.starts)-1].Height)
}

func TestIndexCoverageHandler(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleIndexCoverage(rec, httptest.NewRequest(http.MethodGet, "/index-coverage", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))

	sc := fixtureScanner(gappedSource(), false)
	refreshed(t, sc)
	srv.coverageScanner = sc

	rec = httptest.NewRecorder()
	srv.handleIndexCoverage(rec, httptest.NewRequest(http.MethodGet, "/index-coverage", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ready", body["status"])
	require.Len(t, body["years"], 2)
	require.NotContains(t, body, "period")

	rec = httptest.NewRecorder()
	srv.handleIndexCoverage(rec, httptest.NewRequest(http.MethodGet, "/index-coverage?start=2024-01-01&end=2025-01-01", nil))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	period := body["period"].(map[string]any)
	require.Equal(t, "incomplete", period["status"])
	require.Equal(t, false, period["complete"])
}

func TestPeriodCoverageHeaders(t *testing.T) {
	srv := &Server{}
	echo := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	rec := httptest.NewRecorder()
	srv.withPeriodCoverage(echo)(rec, httptest.NewRequest(http.MethodGet, "/8949?start=2024-01-01&end=2025-01-01", nil))
	require.Equal(t, "unknown", rec.Header().Get(headerIndexCoverage), "before the first scan the period is unknown, not complete")

	sc := fixtureScanner(gappedSource(), false)
	refreshed(t, sc)
	srv.coverageScanner = sc

	rec = httptest.NewRecorder()
	srv.withPeriodCoverage(echo)(rec, httptest.NewRequest(http.MethodGet, "/8949?start=2024-07-01&end=2025-01-01", nil))
	require.Equal(t, "incomplete", rec.Header().Get(headerIndexCoverage))
	require.Equal(t, "2024", rec.Header().Get(headerIndexCoverageYears))
	require.Contains(t, rec.Header().Get(headerIndexCoverageWarning), "2024 is 83.7% indexed")
	require.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), headerIndexCoverage)

	rec = httptest.NewRecorder()
	srv.withPeriodCoverage(echo)(rec, httptest.NewRequest(http.MethodGet, "/8949?start=2025-01-01&end=2026-01-01", nil))
	require.Equal(t, "complete", rec.Header().Get(headerIndexCoverage))
	require.Empty(t, rec.Header().Get(headerIndexCoverageWarning))

	// /positions is as of a date: all history up to and including that day.
	rec = httptest.NewRecorder()
	srv.withPeriodCoverage(echo)(rec, httptest.NewRequest(http.MethodGet, "/positions?date=2024-07-31", nil))
	require.Equal(t, "complete", rec.Header().Get(headerIndexCoverage))
	rec = httptest.NewRecorder()
	srv.withPeriodCoverage(echo)(rec, httptest.NewRequest(http.MethodGet, "/positions?date=2024-12-31", nil))
	require.Equal(t, "incomplete", rec.Header().Get(headerIndexCoverage))
}

func TestCoverageTextHasNoEmDash(t *testing.T) {
	snap := refreshed(t, fixtureScanner(gappedSource(), false))
	day := func(s string) time.Time { d, _ := time.Parse(apiDateLayout, s); return d }
	for _, s := range []string{snap.Note, snap.Period(day("2023-01-01"), day("2025-01-01")).Warning, unknownPeriodCoverage().Warning} {
		require.False(t, strings.ContainsAny(s, "—–"), s)
	}
}

// Runs the scanner's real SQL against connection-local temporary tables.
func TestIndexCoverageSourcePostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres index coverage verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE chains (id bigint, chain_id text)",
		"CREATE TEMP TABLE blocks (id bigserial, chain_id bigint, height bigint, time_stamp timestamptz, tx_indexed boolean, block_events_indexed boolean)",
		"INSERT INTO pg_temp.chains VALUES (1,'cosmoshub-4'),(2,'other-1')",
		// 2025-12-31 23:55 through 2026-01-01 00:20 at one block a minute,
		// with heights 29114840-29114844 missing and events only on the first 3.
		`INSERT INTO pg_temp.blocks (chain_id, height, time_stamp, tx_indexed, block_events_indexed)
		 SELECT 1, h, TIMESTAMPTZ '2025-12-31 23:55:00+00' + (h - 29114831) * INTERVAL '1 minute', true, h < 29114834
		 FROM generate_series(29114831, 29114855) h WHERE h NOT BETWEEN 29114840 AND 29114844`,
		"INSERT INTO pg_temp.blocks (chain_id, height, time_stamp, tx_indexed, block_events_indexed) VALUES (2, 29114845, now(), true, true)",
		"UPDATE pg_temp.blocks SET tx_indexed = false WHERE height = 29114850",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	src := &gormCoverageSource{db: database, chain: apiMainnet}
	tip, _, ok, err := src.Tip(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(29114855), tip)

	runs, err := src.TxRuns(ctx, 29114800, 29114899)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	require.Equal(t, int64(29114831), runs[0].Lo)
	require.Equal(t, int64(29114839), runs[0].Hi)
	require.Equal(t, int64(3), runs[0].Events)
	require.Equal(t, time.Date(2025, 12, 31, 23, 55, 0, 0, time.UTC), runs[0].T0.UTC())
	require.Equal(t, int64(29114845), runs[1].Lo)
	require.Equal(t, int64(29114849), runs[1].Hi)
	require.Equal(t, int64(29114851), runs[2].Lo)

	ev, err := src.EventRuns(ctx, 29114800, 29114899)
	require.NoError(t, err)
	require.Len(t, ev, 1)
	require.Equal(t, int64(29114833), ev[0].Hi)

	h, ts, ok, err := src.BlockAtOrAfter(ctx, 29114840)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(29114845), h, "rows of another chain are ignored")
	require.Equal(t, time.Date(2026, 1, 1, 0, 9, 0, 0, time.UTC), ts.UTC())

	found, ok, err := findYearStart(ctx, src, 2026, 29114831, tip)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(29114836), found, "matches the hard-coded 2026 start")
}
