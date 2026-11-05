package taxapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Index coverage answers "which blocks of the chain are in the database",
// per UTC calendar year, so a report for a period the history backfill has
// not reached yet says so instead of looking like a quiet wallet.
//
// Counting tens of millions of block rows per request is not affordable, so
// a background scanner walks the height range in fixed buckets, keeps each
// bucket's runs of indexed heights in memory, and publishes one immutable
// snapshot that requests read. A bucket is rescanned every refresh while it
// is still filling and only on the slow recheck cadence once every height in
// it is indexed (a repair can still flip a block back to tx_indexed=false).

// yearStart is the first block of a UTC calendar year.
type yearStart struct {
	Year   int
	Height int64
}

// hubYearStarts are the first cosmoshub-4 heights of each UTC year, read from
// archive block headers. 2021 starts at the cosmoshub-4 genesis (initial
// height 5200791, 2021-02-18); earlier Hub history is on cosmoshub-3 and
// before, which this index does not cover. Later years are found from the
// indexed blocks themselves (see findYearStart).
var hubYearStarts = []yearStart{
	{2021, 5200791},
	{2022, 8902586},
	{2023, 13482775},
	{2024, 18525613},
	{2025, 23763459},
	{2026, 29114836},
}

// hubGenesisDate is the UTC day of the first cosmoshub-4 block.
var hubGenesisDate = time.Date(2021, 2, 18, 0, 0, 0, 0, time.UTC)

const (
	coverageBucketSize = int64(100_000)
	coverageMaxListed  = 50
	// Up to this many unindexed blocks at the tip are the live indexer's
	// in-flight edge (about 20 minutes of Hub blocks), not a gap.
	coverageTipSlack = int64(200)
	// Pause between bucket queries so a full pass never competes with the
	// indexer's writes for long.
	coverageBucketPause = 20 * time.Millisecond
)

// Coverage criteria. Reports read transactions only, so a block counts as
// indexed when its transactions are. Once the block events pass has covered
// the history, TAX_COVERAGE_REQUIRE_BLOCK_EVENTS=true makes it require both.
const (
	coverageCriterionTx         = "tx_indexed"
	coverageCriterionTxAndBlock = "tx_indexed and block_events_indexed"
)

// heightRun is a run of consecutive heights that meet a criterion, with the
// timestamps of its first and last block.
type heightRun struct {
	Lo, Hi int64
	T0, T1 time.Time
	// Events counts heights in the run that also have block events indexed
	// (filled only for transaction runs).
	Events int64
}

func (r heightRun) blocks() int64 { return r.Hi - r.Lo + 1 }

// coverageSource is the database side of the scanner, an interface so the
// assembly logic is testable without Postgres.
type coverageSource interface {
	// Tip is the highest block row for the chain, indexed or not.
	Tip(ctx context.Context) (height int64, ts time.Time, ok bool, err error)
	// TxRuns returns runs of tx_indexed heights in [lo,hi] with each run's
	// count of heights that also have block events.
	TxRuns(ctx context.Context, lo, hi int64) ([]heightRun, error)
	// EventRuns returns runs of heights in [lo,hi] with both flags set.
	EventRuns(ctx context.Context, lo, hi int64) ([]heightRun, error)
	// BlockAtOrAfter is the first block row at or after height.
	BlockAtOrAfter(ctx context.Context, height int64) (h int64, ts time.Time, ok bool, err error)
	// MinHeight is the lowest block row, the genesis fallback for a chain
	// without a known year table.
	MinHeight(ctx context.Context) (int64, bool, error)
}

type coverageBucket struct {
	Lo, Hi    int64
	Tx, Ev    []heightRun
	ScannedAt time.Time
}

func (b coverageBucket) settled() bool {
	full := func(runs []heightRun) bool {
		return len(runs) == 1 && runs[0].Lo == b.Lo && runs[0].Hi == b.Hi
	}
	return full(b.Tx) && full(b.Ev)
}

// CoverageGap is a run of heights not yet indexed. From and Through are the
// UTC days the gap touches, taken from the indexed blocks on either side, so
// they are conservative by up to a day.
type CoverageGap struct {
	FirstHeight int64  `json:"first_height"`
	LastHeight  int64  `json:"last_height"`
	Blocks      int64  `json:"blocks"`
	From        string `json:"from"`
	Through     string `json:"through"`
}

// CoverageRange is an inclusive range of UTC days in which every block is
// indexed.
type CoverageRange struct {
	From    string `json:"from"`
	Through string `json:"through"`
}

// YearCoverage is one UTC calendar year of the chain.
type YearCoverage struct {
	Year                     int             `json:"year"`
	FirstHeight              int64           `json:"first_height"`
	LastHeight               int64           `json:"last_height"`
	ExpectedBlocks           int64           `json:"expected_blocks"`
	IndexedBlocks            int64           `json:"indexed_blocks"`
	TxIndexedBlocks          int64           `json:"tx_indexed_blocks"`
	BlockEventsIndexedBlocks int64           `json:"block_events_indexed_blocks"`
	PercentIndexed           float64         `json:"percent_indexed"`
	Complete                 bool            `json:"complete"`
	TxComplete               bool            `json:"tx_complete"`
	BlockEventsComplete      bool            `json:"block_events_complete"`
	InProgress               bool            `json:"in_progress"`
	EarliestCoveredDate      *string         `json:"earliest_covered_date"`
	LatestCoveredDate        *string         `json:"latest_covered_date"`
	CoveredRanges            []CoverageRange `json:"covered_ranges"`
	CoveredRangeCount        int             `json:"covered_range_count"`
	Gaps                     []CoverageGap   `json:"gaps"`
	GapCount                 int             `json:"gap_count"`

	firstDay, lastDay time.Time
	ranges            []dayRange
}

// IndexedSpan is the contiguous indexed range that ends at the tip.
type IndexedSpan struct {
	FirstHeight int64  `json:"first_height"`
	LastHeight  int64  `json:"last_height"`
	Blocks      int64  `json:"blocks"`
	From        string `json:"from"`
	Through     string `json:"through"`
}

// IndexCoverage is the published snapshot, served at GET /index-coverage.
type IndexCoverage struct {
	Chain         string         `json:"chain"`
	Status        string         `json:"status"`
	Criterion     string         `json:"criterion"`
	GenesisHeight int64          `json:"genesis_height"`
	GenesisDate   string         `json:"genesis_date"`
	TipHeight     int64          `json:"tip_height"`
	TipTime       time.Time      `json:"tip_time"`
	ComputedAt    time.Time      `json:"computed_at"`
	Complete      bool           `json:"complete"`
	IndexedRange  *IndexedSpan   `json:"indexed_range"`
	Years         []YearCoverage `json:"years"`
	Note          string         `json:"note"`

	ranges []dayRange
}

// PeriodCoverage says whether a requested period is fully indexed. It rides
// on every report and export as headers and, for JSON bodies, as
// index_coverage.
type PeriodCoverage struct {
	Status          string               `json:"status"`
	Complete        bool                 `json:"complete"`
	Start           string               `json:"start"`
	End             string               `json:"end"`
	BeforeGenesis   bool                 `json:"before_genesis,omitempty"`
	IncompleteYears []PeriodYearCoverage `json:"incomplete_years"`
	Warning         string               `json:"warning,omitempty"`
}

// PeriodYearCoverage is one year the period touches that is not fully
// indexed inside the period.
type PeriodYearCoverage struct {
	Year                int     `json:"year"`
	PercentIndexed      float64 `json:"percent_indexed"`
	IndexedBlocks       int64   `json:"indexed_blocks"`
	ExpectedBlocks      int64   `json:"expected_blocks"`
	EarliestCoveredDate *string `json:"earliest_covered_date"`
	LatestCoveredDate   *string `json:"latest_covered_date"`
}

const (
	coverageStatusReady      = "ready"
	coverageStatusWarming    = "warming"
	coverageStatusComplete   = "complete"
	coverageStatusIncomplete = "incomplete"
	coverageStatusUnknown    = "unknown"
)

// Response headers that carry PeriodCoverage on CSV and JSON responses.
const (
	headerIndexCoverage        = "X-Index-Coverage"
	headerIndexCoverageWarning = "X-Index-Coverage-Warning"
	headerIndexCoverageYears   = "X-Index-Coverage-Incomplete-Years"
)

// indexCoverageScanner owns the bucket cache and the published snapshot.
type indexCoverageScanner struct {
	src           coverageSource
	chain         string
	starts        []yearStart
	genesis       int64
	genesisDay    time.Time
	requireEvents bool
	recheck       time.Duration
	now           func() time.Time
	pause         time.Duration

	mu       sync.Mutex
	buckets  map[int64]*coverageBucket
	found    map[int]int64
	snapshot *IndexCoverage
	snapMu   sync.RWMutex
}

func newIndexCoverageScanner(src coverageSource, chain string, requireEvents bool) *indexCoverageScanner {
	sc := &indexCoverageScanner{
		src: src, chain: chain, requireEvents: requireEvents,
		recheck: 6 * time.Hour, now: nowUTC, pause: coverageBucketPause,
		buckets: map[int64]*coverageBucket{}, found: map[int]int64{},
	}
	if chainParam(chain) == apiMainnet {
		sc.starts = append([]yearStart(nil), hubYearStarts...)
		sc.genesis = hubYearStarts[0].Height
		sc.genesisDay = hubGenesisDate
	}
	return sc
}

func (sc *indexCoverageScanner) criterion() string {
	if sc.requireEvents {
		return coverageCriterionTxAndBlock
	}
	return coverageCriterionTx
}

// Snapshot is the last published coverage, nil until the first pass ends.
func (sc *indexCoverageScanner) Snapshot() *IndexCoverage {
	sc.snapMu.RLock()
	defer sc.snapMu.RUnlock()
	return sc.snapshot
}

// Refresh rescans the buckets that need it and publishes a new snapshot.
func (sc *indexCoverageScanner) Refresh(ctx context.Context) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	tip, tipTime, ok, err := sc.src.Tip(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no blocks indexed yet")
	}
	if sc.genesis == 0 {
		lo, ok, err := sc.src.MinHeight(ctx)
		if err != nil || !ok {
			return fmt.Errorf("genesis height: %v", err)
		}
		sc.genesis = lo
		if _, ts, ok, err := sc.src.BlockAtOrAfter(ctx, lo); err == nil && ok {
			sc.genesisDay = utcDay(ts)
		}
	}
	now := sc.now()
	for lo := (sc.genesis / coverageBucketSize) * coverageBucketSize; lo <= tip; lo += coverageBucketSize {
		bLo, bHi := max(lo, sc.genesis), min(lo+coverageBucketSize-1, tip)
		b := sc.buckets[lo]
		if b != nil && b.Hi == bHi && b.settled() && now.Sub(b.ScannedAt) < sc.recheck {
			continue
		}
		next, err := sc.scanBucket(ctx, bLo, bHi)
		if err != nil {
			return err
		}
		next.ScannedAt = now
		sc.buckets[lo] = next
		if sc.pause > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sc.pause):
			}
		}
	}
	for lo, b := range sc.buckets {
		if b.Lo > tip {
			delete(sc.buckets, lo)
		}
	}
	starts, err := sc.yearStarts(ctx, tip, tipTime)
	if err != nil {
		return err
	}
	txRuns, evRuns := sc.mergedRuns()
	snap := assembleCoverage(sc.chain, sc.criterion(), sc.genesis, sc.genesisDay, tip, tipTime, now, starts, txRuns, evRuns)
	sc.snapMu.Lock()
	sc.snapshot = snap
	sc.snapMu.Unlock()
	return nil
}

func (sc *indexCoverageScanner) scanBucket(ctx context.Context, lo, hi int64) (*coverageBucket, error) {
	tx, err := sc.src.TxRuns(ctx, lo, hi)
	if err != nil {
		return nil, err
	}
	b := &coverageBucket{Lo: lo, Hi: hi, Tx: tx}
	var withEvents, txBlocks int64
	for _, r := range tx {
		withEvents += r.Events
		txBlocks += r.blocks()
	}
	switch {
	case withEvents == 0:
	case withEvents == txBlocks:
		b.Ev = append([]heightRun(nil), tx...)
	default:
		if b.Ev, err = sc.src.EventRuns(ctx, lo, hi); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// mergedRuns joins the per-bucket runs into chain-wide runs, transaction
// runs and event runs separately.
func (sc *indexCoverageScanner) mergedRuns() (tx, ev []heightRun) {
	keys := make([]int64, 0, len(sc.buckets))
	for k := range sc.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		tx = appendRuns(tx, sc.buckets[k].Tx)
		ev = appendRuns(ev, sc.buckets[k].Ev)
	}
	return tx, ev
}

func appendRuns(out, runs []heightRun) []heightRun {
	for _, r := range runs {
		if n := len(out); n > 0 && out[n-1].Hi+1 >= r.Lo {
			if r.Hi > out[n-1].Hi {
				out[n-1].Hi, out[n-1].T1 = r.Hi, r.T1
			}
			out[n-1].Events += r.Events
			continue
		}
		out = append(out, r)
	}
	return out
}

// yearStarts returns the known year table plus any later year whose first
// block is found in the index, up to the tip's year.
func (sc *indexCoverageScanner) yearStarts(ctx context.Context, tip int64, tipTime time.Time) ([]yearStart, error) {
	starts := append([]yearStart(nil), sc.starts...)
	if len(starts) == 0 {
		starts = []yearStart{{sc.genesisDay.Year(), sc.genesis}}
	}
	for y := starts[len(starts)-1].Year + 1; y <= tipTime.UTC().Year(); y++ {
		h, ok := sc.found[y]
		if !ok {
			var err error
			h, ok, err = findYearStart(ctx, sc.src, y, starts[len(starts)-1].Height, tip)
			if err != nil {
				return nil, err
			}
			if !ok {
				// The boundary block or its predecessor is not indexed yet;
				// the year stays folded into the previous one until it is.
				break
			}
			sc.found[y] = h
		}
		starts = append(starts, yearStart{y, h})
	}
	return starts, nil
}

// findYearStart binary searches indexed block timestamps for the first
// height of year y in (lo, hi]. It only accepts a boundary whose block and
// predecessor are both present, so a gap at the boundary is never mistaken
// for the start of the year.
func findYearStart(ctx context.Context, src coverageSource, y int, lo, hi int64) (int64, bool, error) {
	boundary := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	for lo < hi {
		mid := lo + (hi-lo)/2
		h, ts, ok, err := src.BlockAtOrAfter(ctx, mid)
		if err != nil {
			return 0, false, err
		}
		if !ok || h > hi {
			hi = mid
			continue
		}
		if ts.Before(boundary) {
			lo = h + 1
		} else {
			hi = mid
		}
	}
	h, ts, ok, err := src.BlockAtOrAfter(ctx, lo)
	if err != nil || !ok || h != lo || ts.Before(boundary) {
		return 0, false, err
	}
	ph, pts, ok, err := src.BlockAtOrAfter(ctx, lo-1)
	if err != nil || !ok || ph != lo-1 || !pts.Before(boundary) {
		return 0, false, err
	}
	return lo, true, nil
}

type dayRange struct{ From, Through time.Time }

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func dayString(t time.Time) string { return t.UTC().Format(apiDateLayout) }

func dayPtr(t time.Time) *string {
	s := dayString(t)
	return &s
}

// assembleCoverage builds the snapshot from chain-wide runs. Pure, so the
// date arithmetic is unit tested without a database.
func assembleCoverage(chain, criterion string, genesis int64, genesisDay time.Time, tip int64, tipTime, now time.Time, starts []yearStart, txRuns, evRuns []heightRun) *IndexCoverage {
	critRuns := txRuns
	if criterion == coverageCriterionTxAndBlock {
		critRuns = evRuns
	}
	gaps := complementRuns(critRuns, genesis, tip)
	// The live indexer writes the newest blocks a moment before it marks
	// them indexed. A short unindexed tail is that edge, not a gap: the tip
	// moves back to the last indexed block instead of failing the year.
	if n := len(gaps); n > 0 && gaps[n-1].Hi == tip && gaps[n-1].blocks() <= coverageTipSlack && gaps[n-1].Lo > genesis && !gaps[n-1].T0.IsZero() {
		tip, tipTime = gaps[n-1].Lo-1, gaps[n-1].T0
		gaps = gaps[:n-1]
	}
	tipDay := utcDay(tipTime)
	c := &IndexCoverage{
		Chain: chainLabel(chain), Status: coverageStatusReady, Criterion: criterion,
		GenesisHeight: genesis, GenesisDate: dayString(genesisDay),
		TipHeight: tip, TipTime: tipTime.UTC(), ComputedAt: now.UTC(),
		Complete: len(gaps) == 0, Years: []YearCoverage{},
	}
	for i, ys := range starts {
		if ys.Height > tip {
			break
		}
		y := YearCoverage{Year: ys.Year, FirstHeight: ys.Height, LastHeight: tip, InProgress: true}
		y.firstDay = time.Date(ys.Year, 1, 1, 0, 0, 0, 0, time.UTC)
		if ys.Height == genesis {
			y.firstDay = genesisDay
		}
		y.lastDay = tipDay
		if i+1 < len(starts) && starts[i+1].Height <= tip {
			y.LastHeight, y.InProgress = starts[i+1].Height-1, false
			y.lastDay = time.Date(ys.Year, 12, 31, 0, 0, 0, 0, time.UTC)
		}
		y.ExpectedBlocks = y.LastHeight - y.FirstHeight + 1
		y.TxIndexedBlocks = overlap(txRuns, y.FirstHeight, y.LastHeight)
		y.BlockEventsIndexedBlocks = overlap(evRuns, y.FirstHeight, y.LastHeight)
		y.IndexedBlocks = overlap(critRuns, y.FirstHeight, y.LastHeight)
		y.TxComplete = y.TxIndexedBlocks == y.ExpectedBlocks
		y.BlockEventsComplete = y.BlockEventsIndexedBlocks == y.ExpectedBlocks
		y.Complete = y.IndexedBlocks == y.ExpectedBlocks
		y.PercentIndexed = floorPercent(y.IndexedBlocks, y.ExpectedBlocks)

		var touched []dayRange
		y.Gaps = []CoverageGap{}
		for _, g := range gaps {
			if g.Hi < y.FirstHeight || g.Lo > y.LastHeight {
				continue
			}
			lo, hi := max(g.Lo, y.FirstHeight), min(g.Hi, y.LastHeight)
			from, through := y.firstDay, y.lastDay
			if lo == g.Lo && !g.T0.IsZero() {
				from = maxTime(utcDay(g.T0), y.firstDay)
			}
			if hi == g.Hi && !g.T1.IsZero() {
				through = minTime(utcDay(g.T1), y.lastDay)
			}
			touched = append(touched, dayRange{from, through})
			y.GapCount++
			if len(y.Gaps) < coverageMaxListed {
				y.Gaps = append(y.Gaps, CoverageGap{FirstHeight: lo, LastHeight: hi, Blocks: hi - lo + 1, From: dayString(from), Through: dayString(through)})
			}
		}
		y.ranges = complementDays(touched, y.firstDay, y.lastDay)
		y.CoveredRangeCount = len(y.ranges)
		y.CoveredRanges = []CoverageRange{}
		for _, r := range y.ranges {
			if len(y.CoveredRanges) < coverageMaxListed {
				y.CoveredRanges = append(y.CoveredRanges, CoverageRange{dayString(r.From), dayString(r.Through)})
			}
		}
		if n := len(y.ranges); n > 0 {
			y.EarliestCoveredDate, y.LatestCoveredDate = dayPtr(y.ranges[0].From), dayPtr(y.ranges[n-1].Through)
		}
		c.ranges = joinDayRanges(c.ranges, y.ranges)
		c.Years = append(c.Years, y)
	}
	if n := len(critRuns); n > 0 && critRuns[n-1].Hi >= tip && critRuns[n-1].Lo <= tip {
		last := critRuns[n-1]
		span := &IndexedSpan{FirstHeight: last.Lo, LastHeight: tip, Blocks: tip - last.Lo + 1}
		if m := len(c.ranges); m > 0 {
			span.From, span.Through = dayString(c.ranges[m-1].From), dayString(c.ranges[m-1].Through)
		}
		c.IndexedRange = span
	}
	c.Note = coverageNote(c)
	return c
}

// complementRuns returns the gaps between runs in [lo,hi]. A gap's T0 is the
// timestamp of the indexed block before it and T1 of the block after it,
// zero when the gap reaches the edge of the range.
func complementRuns(runs []heightRun, lo, hi int64) []heightRun {
	var gaps []heightRun
	next := lo
	var prevT time.Time
	for _, r := range runs {
		if r.Hi < lo || r.Lo > hi {
			continue
		}
		if r.Lo > next {
			gaps = append(gaps, heightRun{Lo: next, Hi: r.Lo - 1, T0: prevT, T1: r.T0})
		}
		next, prevT = r.Hi+1, r.T1
	}
	if next <= hi {
		gaps = append(gaps, heightRun{Lo: next, Hi: hi, T0: prevT})
	}
	return gaps
}

func overlap(runs []heightRun, lo, hi int64) int64 {
	var n int64
	for _, r := range runs {
		a, b := max(r.Lo, lo), min(r.Hi, hi)
		if a <= b {
			n += b - a + 1
		}
	}
	return n
}

// floorPercent never rounds a year up to 100 while a block is missing.
func floorPercent(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return math.Floor(float64(n)*1000/float64(d)) / 10
}

// complementDays returns the days in [from, through] that no touched range
// covers.
func complementDays(touched []dayRange, from, through time.Time) []dayRange {
	sort.Slice(touched, func(i, j int) bool { return touched[i].From.Before(touched[j].From) })
	var out []dayRange
	next := from
	for _, t := range touched {
		if t.From.After(next) {
			out = append(out, dayRange{next, t.From.AddDate(0, 0, -1)})
		}
		if after := t.Through.AddDate(0, 0, 1); after.After(next) {
			next = after
		}
	}
	if !next.After(through) {
		out = append(out, dayRange{next, through})
	}
	return out
}

func joinDayRanges(out, ranges []dayRange) []dayRange {
	for _, r := range ranges {
		if n := len(out); n > 0 && !out[n-1].Through.AddDate(0, 0, 1).Before(r.From) {
			if r.Through.After(out[n-1].Through) {
				out[n-1].Through = r.Through
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func chainLabel(chain string) string {
	if chainParam(chain) == apiMainnet {
		return apiHubChainID
	}
	return chain
}

func coverageNote(c *IndexCoverage) string {
	if c.Complete {
		return fmt.Sprintf("Every %s block from genesis (%s) to the tip is indexed. Earlier Cosmos Hub chains are not covered.", c.Chain, c.GenesisDate)
	}
	var partial []string
	for _, y := range c.Years {
		if !y.Complete {
			partial = append(partial, fmt.Sprintf("%d (%s%%)", y.Year, trimPercent(y.PercentIndexed)))
		}
	}
	note := "History backfill in progress. Not fully indexed yet: " + strings.Join(partial, ", ") + "."
	if c.IndexedRange != nil && c.IndexedRange.From != "" {
		note = fmt.Sprintf("Fully indexed from %s to %s. %s", c.IndexedRange.From, c.IndexedRange.Through, note)
	}
	return note + " Reports for those periods can be generated and carry a notice until the backfill finishes."
}

func trimPercent(p float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", p), "0"), ".")
}

// covers reports whether every day in [from, through] is fully indexed.
func (c *IndexCoverage) covers(from, through time.Time) bool {
	for _, r := range c.ranges {
		if !from.Before(r.From) && !through.After(r.Through) {
			return true
		}
	}
	return false
}

// MonthCovered reports whether every day of a YYYY-MM month that lies
// between genesis and the tip is indexed.
func (c *IndexCoverage) MonthCovered(month string) bool {
	first, err := time.Parse(apiMonthLayout, month)
	if err != nil {
		return true
	}
	last := first.AddDate(0, 1, -1)
	if genesisDay, err := time.Parse(apiDateLayout, c.GenesisDate); err == nil && first.Before(genesisDay) {
		first = genesisDay
	}
	if tipDay := utcDay(c.TipTime); last.After(tipDay) {
		last = tipDay
	}
	return last.Before(first) || c.covers(first, last)
}

// Period evaluates a request window [start, end) (end exclusive, as every
// handler treats it). A zero start means all history, which begins at
// genesis. An explicit start before genesis is flagged: that activity is on
// an earlier chain and is never in this index.
func (c *IndexCoverage) Period(start, end time.Time) PeriodCoverage {
	genesisDay, _ := time.Parse(apiDateLayout, c.GenesisDate)
	tipDay := utcDay(c.TipTime)
	from := genesisDay
	before := false
	if !start.IsZero() {
		from = utcDay(start)
		if from.Before(genesisDay) {
			before = true
			from = genesisDay
		}
	}
	through := utcDay(end.Add(-time.Nanosecond))
	if through.After(tipDay) {
		through = tipDay
	}
	p := PeriodCoverage{Start: dayString(from), End: dayString(through), BeforeGenesis: before, IncompleteYears: []PeriodYearCoverage{}}
	if before {
		p.Start = dayString(utcDay(start))
	}
	if through.Before(from) {
		p.Status, p.Complete = coverageStatusIncomplete, false
		switch {
		case before:
			p.Warning = fmt.Sprintf("This period is before %s genesis (%s). Earlier Cosmos Hub chains are not indexed, so this report has no data for it.", c.Chain, c.GenesisDate)
		default:
			p.Warning = fmt.Sprintf("This period starts after the latest indexed block (%s).", dayString(tipDay))
		}
		return p
	}
	for _, y := range c.Years {
		a, b := maxTime(from, y.firstDay), minTime(through, y.lastDay)
		if a.After(b) || c.covers(a, b) {
			continue
		}
		p.IncompleteYears = append(p.IncompleteYears, PeriodYearCoverage{
			Year: y.Year, PercentIndexed: y.PercentIndexed, IndexedBlocks: y.IndexedBlocks, ExpectedBlocks: y.ExpectedBlocks,
			EarliestCoveredDate: y.EarliestCoveredDate, LatestCoveredDate: y.LatestCoveredDate,
		})
	}
	p.Complete = !before && len(p.IncompleteYears) == 0 && c.covers(from, through)
	p.Status = coverageStatusComplete
	if !p.Complete {
		p.Status = coverageStatusIncomplete
		p.Warning = periodWarning(c, p)
	}
	return p
}

func periodWarning(c *IndexCoverage, p PeriodCoverage) string {
	var parts []string
	for _, y := range p.IncompleteYears {
		parts = append(parts, fmt.Sprintf("%d is %s%% indexed", y.Year, trimPercent(y.PercentIndexed)))
	}
	msg := "This period is not fully indexed yet"
	if len(parts) > 0 {
		msg += ": " + strings.Join(parts, ", ")
	}
	msg += ". Activity in blocks the history backfill has not reached is missing from this report; regenerate it once the backfill finishes."
	if p.BeforeGenesis {
		msg += fmt.Sprintf(" Activity before %s genesis (%s) is on earlier Cosmos Hub chains and is not indexed.", c.Chain, c.GenesisDate)
	}
	return msg
}

// unknownPeriodCoverage is served while the first scan is still running.
func unknownPeriodCoverage() PeriodCoverage {
	return PeriodCoverage{Status: coverageStatusUnknown, IncompleteYears: []PeriodYearCoverage{}, Warning: "Index coverage is still being computed; this report may not cover the whole period."}
}

// periodCoverage evaluates [start, end) against the current snapshot.
func (s *Server) periodCoverage(start, end time.Time) PeriodCoverage {
	sc := s.indexCoverage()
	if sc == nil {
		return unknownPeriodCoverage()
	}
	snap := sc.Snapshot()
	if snap == nil {
		return unknownPeriodCoverage()
	}
	return snap.Period(start, end)
}

func (s *Server) indexCoverage() *indexCoverageScanner {
	s.indexCovMu.RLock()
	defer s.indexCovMu.RUnlock()
	return s.coverageScanner
}

// StartIndexCoverage scans on start and then every interval. requireEvents
// makes a block count only once its block events are indexed too.
func (s *Server) StartIndexCoverage(interval time.Duration, requireEvents bool, logf func(string, ...any)) {
	sc := newIndexCoverageScanner(&gormCoverageSource{db: s.db, chain: apiMainnet}, apiMainnet, requireEvents)
	s.indexCovMu.Lock()
	s.coverageScanner = sc
	s.indexCovMu.Unlock()
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		started := time.Now()
		if err := sc.Refresh(ctx); err != nil {
			logf("index coverage: %v", err)
			return
		}
		if snap := sc.Snapshot(); snap != nil {
			logf("index coverage: tip %d, complete=%t, %d year(s), %s", snap.TipHeight, snap.Complete, len(snap.Years), time.Since(started).Round(time.Millisecond))
		}
	}
	go func() {
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

// handleIndexCoverage serves GET /index-coverage[?start=&end=]. With a
// period it adds period: the same evaluation reports carry.
func (s *Server) handleIndexCoverage(w http.ResponseWriter, r *http.Request) {
	sc := s.indexCoverage()
	var snap *IndexCoverage
	if sc != nil {
		snap = sc.Snapshot()
	}
	if snap == nil {
		w.Header().Set("Retry-After", "60")
		w.Header().Set(apiContentType, apiJSONContentType)
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{apiStatus: coverageStatusWarming, apiError: "index coverage is still being computed; retry shortly"})
		return
	}
	q := r.URL.Query()
	w.Header().Set(apiContentType, apiJSONContentType)
	w.Header().Set("Cache-Control", "public, max-age=60")
	if q.Get(apiStart) == "" && q.Get(apiEnd) == "" {
		writeJSON(w, snap)
		return
	}
	period := snap.Period(dateParam(q.Get(apiStart), time.Time{}), dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1)))
	writeJSON(w, struct {
		*IndexCoverage
		Period PeriodCoverage `json:"period"`
	}{snap, period})
}

// withPeriodCoverage stamps the requested period's coverage on a report or
// export response as headers before the handler writes its body.
func (s *Server) withPeriodCoverage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start := dateParam(q.Get(apiStart), time.Time{})
		end := dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1))
		if d := q.Get(apiDate); d != "" && q.Get(apiStart) == "" && q.Get(apiEnd) == "" {
			// /positions is "as of date": the whole history up to that day.
			end = dateParam(d, nowUTC()).AddDate(0, 0, 1)
		}
		setPeriodCoverageHeaders(w, s.periodCoverage(start, end))
		h(w, r)
	}
}

func setPeriodCoverageHeaders(w http.ResponseWriter, p PeriodCoverage) {
	w.Header().Set(headerIndexCoverage, p.Status)
	if p.Warning != "" {
		w.Header().Set(headerIndexCoverageWarning, asciiHeader(p.Warning))
	}
	if len(p.IncompleteYears) > 0 {
		years := make([]string, 0, len(p.IncompleteYears))
		for _, y := range p.IncompleteYears {
			years = append(years, fmt.Sprint(y.Year))
		}
		w.Header().Set(headerIndexCoverageYears, strings.Join(years, ","))
	}
	exposed := headerIndexCoverage + ", " + headerIndexCoverageWarning + ", " + headerIndexCoverageYears
	w.Header().Set("Access-Control-Expose-Headers", exposed)
}

func asciiHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// gormCoverageSource reads block rows. Every query is a range scan on the
// (height, chain_id) index for one bucket, never a whole-table count.
type gormCoverageSource struct {
	db    *gorm.DB
	chain string

	once     sync.Once
	chainIDs []int64
	idErr    error
}

func (g *gormCoverageSource) ids(ctx context.Context) ([]int64, error) {
	g.once.Do(func() {
		g.idErr = g.db.WithContext(ctx).Table("chains").Where("chain_id IN ?", chainIDs(g.chain)).Pluck("id", &g.chainIDs).Error
		if g.idErr == nil && len(g.chainIDs) == 0 {
			g.idErr = fmt.Errorf("chain %s is not in the chains table", g.chain)
		}
	})
	return g.chainIDs, g.idErr
}

func (g *gormCoverageSource) Tip(ctx context.Context) (int64, time.Time, bool, error) {
	ids, err := g.ids(ctx)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	var row struct {
		Height    int64
		TimeStamp *time.Time
	}
	res := g.db.WithContext(ctx).Raw("SELECT height, time_stamp FROM blocks WHERE chain_id IN ? ORDER BY height DESC LIMIT 1", ids).Scan(&row)
	if res.Error != nil || res.RowsAffected == 0 {
		return 0, time.Time{}, false, res.Error
	}
	ts := time.Time{}
	if row.TimeStamp != nil {
		ts = *row.TimeStamp
	} else {
		// The newest row can still be mid-write; take the newest timestamp.
		var latest struct{ TimeStamp *time.Time }
		_ = g.db.WithContext(ctx).Raw("SELECT time_stamp FROM blocks WHERE chain_id IN ? AND height >= ? AND time_stamp IS NOT NULL ORDER BY height DESC LIMIT 1", ids, row.Height-1000).Scan(&latest).Error
		if latest.TimeStamp != nil {
			ts = *latest.TimeStamp
		}
	}
	return row.Height, ts, true, nil
}

func (g *gormCoverageSource) MinHeight(ctx context.Context) (int64, bool, error) {
	ids, err := g.ids(ctx)
	if err != nil {
		return 0, false, err
	}
	var row struct{ Height int64 }
	res := g.db.WithContext(ctx).Raw("SELECT height FROM blocks WHERE chain_id IN ? ORDER BY height LIMIT 1", ids).Scan(&row)
	return row.Height, res.RowsAffected > 0, res.Error
}

func (g *gormCoverageSource) BlockAtOrAfter(ctx context.Context, height int64) (int64, time.Time, bool, error) {
	ids, err := g.ids(ctx)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	var row struct {
		Height    int64
		TimeStamp *time.Time
	}
	res := g.db.WithContext(ctx).Raw("SELECT height, time_stamp FROM blocks WHERE chain_id IN ? AND height >= ? AND time_stamp IS NOT NULL ORDER BY height LIMIT 1", ids, height).Scan(&row)
	if res.Error != nil || res.RowsAffected == 0 || row.TimeStamp == nil {
		return 0, time.Time{}, false, res.Error
	}
	return row.Height, *row.TimeStamp, true, nil
}

// runsSQL groups consecutive heights by the classic islands trick: within a
// run, height minus its row number is constant.
const runsSQL = `SELECT MIN(height) AS lo, MAX(height) AS hi, MIN(time_stamp) AS t0, MAX(time_stamp) AS t1, SUM(ev) AS events
FROM (SELECT height, time_stamp, CASE WHEN block_events_indexed THEN 1 ELSE 0 END AS ev,
             height - ROW_NUMBER() OVER (ORDER BY height) AS grp
      FROM blocks WHERE chain_id IN ? AND height >= ? AND height <= ? AND %s) s
GROUP BY grp ORDER BY lo`

func (g *gormCoverageSource) runs(ctx context.Context, lo, hi int64, filter string) ([]heightRun, error) {
	ids, err := g.ids(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Lo, Hi int64
		T0, T1 *time.Time
		Events int64
	}
	if err := g.db.WithContext(ctx).Raw(fmt.Sprintf(runsSQL, filter), ids, lo, hi).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]heightRun, 0, len(rows))
	for _, r := range rows {
		run := heightRun{Lo: r.Lo, Hi: r.Hi, Events: r.Events}
		if r.T0 != nil {
			run.T0 = *r.T0
		}
		if r.T1 != nil {
			run.T1 = *r.T1
		}
		out = append(out, run)
	}
	return out, nil
}

func (g *gormCoverageSource) TxRuns(ctx context.Context, lo, hi int64) ([]heightRun, error) {
	return g.runs(ctx, lo, hi, "tx_indexed")
}

func (g *gormCoverageSource) EventRuns(ctx context.Context, lo, hi int64) ([]heightRun, error) {
	return g.runs(ctx, lo, hi, "tx_indexed AND block_events_indexed")
}
