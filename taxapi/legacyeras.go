package taxapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// The Cosmos Hub restarted three times with a new chain id. cosmoshub-4 lives
// in the public tables; cosmoshub-1 to 3 are imported into the legacy schema
// and read only for eras whose legacy.eras.published is true. Heights restart
// on every chain, so legacy rows are always ordered by (era, height, phase,
// tx_index, msg_index, sub_index), never by height alone.

// legacySchemaDefault is the schema the legacy importer writes.
const legacySchemaDefault = "legacy"

// The Hub chains before cosmoshub-4.
const (
	hub1ChainID = "cosmoshub-1"
	hub2ChainID = "cosmoshub-2"
	hub3ChainID = "cosmoshub-3"
)

// Era statuses served in /index-coverage eras[].
const (
	eraStatusVerified   = "verified"
	eraStatusVerifying  = "verifying"
	eraStatusImporting  = "importing"
	eraStatusNotStarted = "not_started"
	eraStatusLive       = "live"
)

// legacyEra is one row of legacy.eras with what the API needs to place it.
type legacyEra struct {
	ChainID      string
	Era          int
	FirstHeight  int64
	ExportHeight int64
	Published    bool
	// Start and End are the times of the first and export blocks, zero when
	// those blocks are not loaded.
	Start, End time.Time
	// HeightsIndexed counts heights in [first, export] inside done import
	// ranges.
	HeightsIndexed int64
	// NotVerified counts addresses with a reconciliation line whose status
	// does not start with "closed".
	NotVerified int64
	HasRows     bool
	HasBalances bool
	// Stamp changes whenever the era's imported data does.
	Stamp string
	// spans are the merged done import ranges, clipped to the era.
	spans []heightRun
	// years are the era's blocks per UTC calendar year.
	years []eraYearSegment
}

func (e legacyEra) startDay() time.Time { return utcDay(e.Start) }
func (e legacyEra) endDay() time.Time   { return utcDay(e.End) }
func (e legacyEra) placed() bool {
	return e.ExportHeight >= e.FirstHeight && !e.Start.IsZero() && !e.End.IsZero()
}

// eraYearSegment is the part of one era inside one UTC calendar year.
type eraYearSegment struct {
	Year           int
	Lo, Hi         int64
	From, Through  time.Time
	ExpectedBlocks int64
}

// legacyEraState is one read of the legacy schema.
type legacyEraState struct {
	schema string
	// readable is false when the role cannot read the schema or it does not
	// exist. Then no era is in reports and eras[] lists cosmoshub-4 only.
	readable bool
	// eras holds cosmoshub-1 to 3, ascending.
	eras []legacyEra
	// effective are the eras in reports: the longest run of published eras
	// that ends at cosmoshub-3, ascending. An earlier era waits for every
	// later one, so a report never has a hole between two chains.
	effective []legacyEra
	// hub4Start is the time of the first cosmoshub-4 block, zero if unread.
	hub4Start time.Time
	loadedAt  time.Time
}

// inReports reports whether chainID is one of the effective eras.
func (st *legacyEraState) inReports(chainID string) bool {
	if st == nil {
		return false
	}
	for _, e := range st.effective {
		if e.ChainID == chainID {
			return true
		}
	}
	return false
}

func (st *legacyEraState) active() bool { return st != nil && len(st.effective) > 0 }

// earliest is the first era in reports.
func (st *legacyEraState) earliest() legacyEra { return st.effective[0] }

// chainIDs lists the effective era chain ids, ascending.
func (st *legacyEraState) chainIDs() []string {
	out := make([]string, 0, len(st.effective))
	for _, e := range st.effective {
		out = append(out, e.ChainID)
	}
	return out
}

// key identifies the effective era set and the data in it, for report cache
// keys. Empty when no era is in reports.
func (st *legacyEraState) key() string {
	if !st.active() {
		return ""
	}
	parts := make([]string, 0, len(st.effective))
	for _, e := range st.effective {
		parts = append(parts, e.ChainID+"@"+e.Stamp)
	}
	return strings.Join(parts, ",")
}

// eraRank orders chain ids: cosmoshub-1 to 3 by era, everything else
// (cosmoshub-4 and rows with no chain id) after them.
func eraRank(chainID string) int {
	switch chainID {
	case hub1ChainID:
		return 1
	case hub2ChainID:
		return 2
	case hub3ChainID:
		return 3
	}
	return 4
}

// legacyEraCache holds the latest state, refreshed in the background.
type legacyEraCache struct {
	mu     sync.RWMutex
	state  *legacyEraState
	denied bool
}

// legacyEras returns the latest era state for chain. Only mainnet has
// earlier chains; any other chain, or a server that never loaded the
// schema, has none.
func (s *Server) legacyEras(chain string) *legacyEraState {
	if chainParam(chain) != apiMainnet || s.legacy == nil {
		return nil
	}
	s.legacy.mu.RLock()
	defer s.legacy.mu.RUnlock()
	return s.legacy.state
}

// RefreshLegacyEras reads the legacy schema once and publishes the result.
func (s *Server) RefreshLegacyEras(ctx context.Context) error {
	if s.legacy == nil {
		s.legacy = &legacyEraCache{}
	}
	schema := s.legacySchema
	if schema == "" {
		schema = legacySchemaDefault
	}
	st, err := loadLegacyEras(ctx, s.db, schema)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == pgInsufficientPrivilege || pgErr.Code == pgUndefinedTable || pgErr.Code == pgInvalidSchema) {
			s.legacy.mu.Lock()
			first := !s.legacy.denied
			s.legacy.denied = true
			s.legacy.state = &legacyEraState{schema: schema, loadedAt: time.Now()}
			s.legacy.mu.Unlock()
			if first {
				log.Printf("legacy eras: schema %s not readable, earlier Hub chains stay out of reports: %v", schema, err)
			}
			return nil
		}
		return err
	}
	s.legacy.mu.Lock()
	s.legacy.state, s.legacy.denied = st, false
	s.legacy.mu.Unlock()
	return nil
}

// StartLegacyEras loads the era state now and then every interval. Reports
// read an era only once it is published, so a flip of legacy.eras.published
// reaches reports within one interval.
func (s *Server) StartLegacyEras(interval time.Duration, logf func(string, ...any)) {
	if s.legacy == nil {
		s.legacy = &legacyEraCache{}
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.RefreshLegacyEras(ctx); err != nil {
			logf("legacy eras: %v", err)
			return
		}
		if st := s.legacyEras(apiMainnet); st != nil && st.readable {
			logf("legacy eras: in reports %v", st.chainIDs())
		}
	}
	run()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

const pgInvalidSchema = "3F000"

// loadLegacyEras reads every legacy era. All reads are primary key lookups
// or small aggregates.
func loadLegacyEras(ctx context.Context, db *gorm.DB, schema string) (*legacyEraState, error) {
	db = db.WithContext(ctx)
	q := func(table string) string { return fmt.Sprintf("%q.%s", schema, table) }
	var rows []struct {
		ChainID      string
		Era          int
		FirstHeight  int64
		ExportHeight *int64
		Published    bool
	}
	if err := db.Raw("SELECT chain_id, era, first_height, export_height, published FROM " + q("eras") + " WHERE era BETWEEN 1 AND 3 ORDER BY era").Scan(&rows).Error; err != nil {
		return nil, err
	}
	var ranges []struct {
		ChainID              string
		FromHeight, ToHeight int64
		Status               string
		UpdatedAt            time.Time
	}
	if err := db.Raw("SELECT chain_id, from_height, to_height, status, updated_at FROM " + q("import_ranges")).Scan(&ranges).Error; err != nil {
		return nil, err
	}
	var open []struct {
		ChainID string
		N       int64
	}
	if err := db.Raw("SELECT chain_id, count(DISTINCT address) AS n FROM " + q("reconciliation") + " WHERE status NOT LIKE 'closed%' GROUP BY chain_id").Scan(&open).Error; err != nil {
		return nil, err
	}
	st := &legacyEraState{schema: schema, readable: true, loadedAt: time.Now()}
	for _, r := range rows {
		e := legacyEra{ChainID: r.ChainID, Era: r.Era, FirstHeight: r.FirstHeight, Published: r.Published}
		if r.ExportHeight != nil {
			e.ExportHeight = *r.ExportHeight
		}
		blockTime := func(h int64) (time.Time, error) {
			var ts []time.Time
			err := db.Raw("SELECT time FROM "+q("blocks")+" WHERE chain_id = ? AND height = ?", e.ChainID, h).Scan(&ts).Error
			if err != nil || len(ts) == 0 {
				return time.Time{}, err
			}
			return ts[0].UTC(), nil
		}
		var err error
		if e.Start, err = blockTime(e.FirstHeight); err != nil {
			return nil, err
		}
		if e.ExportHeight > 0 {
			if e.End, err = blockTime(e.ExportHeight); err != nil {
				return nil, err
			}
		}
		var spans []heightRun
		var stamp time.Time
		var nRanges int
		for _, ir := range ranges {
			if ir.ChainID != e.ChainID {
				continue
			}
			nRanges++
			if ir.UpdatedAt.After(stamp) {
				stamp = ir.UpdatedAt
			}
			if ir.Status == "done" {
				lo, hi := max(ir.FromHeight, e.FirstHeight), ir.ToHeight
				if e.ExportHeight > 0 {
					hi = min(hi, e.ExportHeight)
				}
				if lo <= hi {
					spans = append(spans, heightRun{Lo: lo, Hi: hi})
				}
			}
		}
		e.spans = mergeRuns(spans)
		e.HeightsIndexed = overlap(e.spans, e.FirstHeight, max(e.ExportHeight, e.FirstHeight))
		for _, o := range open {
			if o.ChainID == e.ChainID {
				e.NotVerified = o.N
			}
		}
		if err := db.Raw("SELECT EXISTS (SELECT 1 FROM "+q("taxable_events")+" WHERE chain_id = ?)", e.ChainID).Scan(&e.HasRows).Error; err != nil {
			return nil, err
		}
		if err := db.Raw("SELECT EXISTS (SELECT 1 FROM "+q("balance_days")+" WHERE chain_id = ?)", e.ChainID).Scan(&e.HasBalances).Error; err != nil {
			return nil, err
		}
		e.Stamp = fmt.Sprintf("%d.%d", nRanges, stamp.UnixMicro())
		// Year boundaries are read only for a published era, whose blocks
		// are all loaded; an era still importing may have holes.
		if e.placed() && e.Published {
			if e.years, err = eraYears(db, q("blocks"), e); err != nil {
				return nil, err
			}
		}
		st.eras = append(st.eras, e)
	}
	// The run of published eras ending at cosmoshub-3. An era whose blocks
	// are not loaded cannot be placed in time and stops the run.
	byEra := map[int]legacyEra{}
	for _, e := range st.eras {
		byEra[e.Era] = e
	}
	for era := 3; era >= 1; era-- {
		e, ok := byEra[era]
		if !ok || !e.Published || !e.placed() {
			break
		}
		st.effective = append([]legacyEra{e}, st.effective...)
	}
	// When cosmoshub-4 started, for eras[]. Its first block carries the
	// genesis time, which was reused from 2019, so the second block's time
	// is the real start. Best effort: the public tables answer it from their
	// (height, chain_id) index.
	var hub4 []time.Time
	if err := db.Raw("SELECT b.time_stamp FROM blocks b JOIN chains c ON c.id = b.chain_id WHERE c.chain_id IN ? AND b.height = ?", chainIDs(apiMainnet), hubYearStarts[0].Height+1).Scan(&hub4).Error; err == nil && len(hub4) > 0 {
		st.hub4Start = hub4[0].UTC()
	}
	return st, nil
}

// mergeRuns sorts runs and joins overlapping or adjacent ones.
func mergeRuns(runs []heightRun) []heightRun {
	sort.Slice(runs, func(i, j int) bool { return runs[i].Lo < runs[j].Lo })
	var out []heightRun
	for _, r := range runs {
		if n := len(out); n > 0 && r.Lo <= out[n-1].Hi+1 {
			out[n-1].Hi = max(out[n-1].Hi, r.Hi)
			continue
		}
		out = append(out, heightRun{Lo: r.Lo, Hi: r.Hi})
	}
	return out
}

// eraYears splits an era at UTC year boundaries. Block times rise with
// height on every chain, so each boundary is a binary search over primary
// key lookups.
func eraYears(db *gorm.DB, blocks string, e legacyEra) ([]eraYearSegment, error) {
	// firstAtOrAfter is the lowest height in [lo, hi] whose time is at or
	// after t, or hi+1.
	firstAtOrAfter := func(t time.Time, lo, hi int64) (int64, error) {
		l, r := lo, hi+1
		for l < r {
			mid := l + (r-l)/2
			var ts []time.Time
			if err := db.Raw("SELECT time FROM "+blocks+" WHERE chain_id = ? AND height = ?", e.ChainID, mid).Scan(&ts).Error; err != nil {
				return 0, err
			}
			if len(ts) == 0 {
				return 0, fmt.Errorf("%s block %d is not loaded", e.ChainID, mid)
			}
			if !ts[0].Before(t) {
				r = mid
			} else {
				l = mid + 1
			}
		}
		return l, nil
	}
	var out []eraYearSegment
	lo := e.FirstHeight
	for y := e.Start.UTC().Year(); y <= e.End.UTC().Year(); y++ {
		hi := e.ExportHeight
		through := e.endDay()
		if y < e.End.UTC().Year() {
			next, err := firstAtOrAfter(time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC), lo, e.ExportHeight)
			if err != nil {
				return nil, err
			}
			hi = next - 1
			through = time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC)
		}
		from := maxTime(time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), e.startDay())
		if hi >= lo {
			out = append(out, eraYearSegment{Year: y, Lo: lo, Hi: hi, From: from, Through: through, ExpectedBlocks: hi - lo + 1})
		}
		lo = hi + 1
	}
	return out, nil
}

// EraCoverage is one Cosmos Hub chain in GET /index-coverage eras[].
type EraCoverage struct {
	ChainID     string `json:"chain_id"`
	Era         int    `json:"era"`
	FirstHeight int64  `json:"first_height"`
	// LastHeight is the export height of an earlier chain and the indexed
	// tip of cosmoshub-4.
	LastHeight int64      `json:"last_height"`
	StartTime  *time.Time `json:"start_time"`
	// EndTime is null for cosmoshub-4, which is still running.
	EndTime *time.Time `json:"end_time"`
	// Status is verified (published), verifying (every height imported, books
	// not closed yet), importing, not_started, or live for cosmoshub-4.
	Status string `json:"status"`
	// InReports is true when reports read the chain. An earlier chain joins
	// reports only once it and every later earlier chain are verified.
	InReports      bool  `json:"in_reports"`
	HeightsIndexed int64 `json:"heights_indexed"`
	// WalletsNotFullyVerified counts wallets whose history on that chain did
	// not reconcile with the next genesis file. Null for cosmoshub-4, whose
	// unverified wallets are named per report.
	WalletsNotFullyVerified *int64 `json:"wallets_not_fully_verified"`
}

func (e legacyEra) status() string {
	switch {
	case e.Published:
		return eraStatusVerified
	case e.ExportHeight >= e.FirstHeight && e.HeightsIndexed == e.ExportHeight-e.FirstHeight+1:
		return eraStatusVerifying
	case e.HeightsIndexed > 0 || e.HasRows || e.HasBalances:
		return eraStatusImporting
	}
	return eraStatusNotStarted
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// erasCoverage lists every Hub chain, oldest first, with cosmoshub-4 last.
func erasCoverage(st *legacyEraState, snap *IndexCoverage) []EraCoverage {
	out := []EraCoverage{}
	if st != nil {
		for _, e := range st.eras {
			n := e.NotVerified
			out = append(out, EraCoverage{
				ChainID: e.ChainID, Era: e.Era, FirstHeight: e.FirstHeight, LastHeight: e.ExportHeight,
				StartTime: timePtr(e.Start), EndTime: timePtr(e.End), Status: e.status(),
				InReports: st.inReports(e.ChainID), HeightsIndexed: e.HeightsIndexed, WalletsNotFullyVerified: &n,
			})
		}
	}
	hub4 := EraCoverage{ChainID: apiHubChainID, Era: 4, Status: eraStatusLive, InReports: true}
	if snap != nil {
		hub4.FirstHeight, hub4.LastHeight = snap.GenesisHeight, snap.TipHeight
		for _, y := range snap.Years {
			hub4.HeightsIndexed += y.IndexedBlocks
		}
	}
	if st != nil {
		hub4.StartTime = timePtr(st.hub4Start)
	}
	return append(out, hub4)
}
