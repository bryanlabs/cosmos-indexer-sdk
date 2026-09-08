package taxapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// Building a report is dominated by USD price lookups (see prewarmPrices);
// pricing a busy wallet's whole history can still take a few seconds even
// parallelized. Rather than make every visit pay that cost, a computed report
// is cached durably (the underlying CSV, not just prices) and served instantly
// on repeat views, invalidated only when newer on-chain activity exists for
// these addresses than what was last computed, a max age backstop, or an
// explicit resync request.
type ReportJob struct {
	ID        uint   `gorm:"primaryKey"`
	Key       string `gorm:"uniqueIndex"`
	Chain     string
	Addresses string // comma-joined, sorted; for display/debugging only, not the cache key
	Format    string
	Status    string // pending | ready | failed
	CSV       string `gorm:"type:text"`
	RowCount  int
	// ComputedThrough is when this computation started (not the data's own
	// timestamps): a report is stale once any of its addresses has a
	// taxable_event newer than this.
	ComputedThrough time.Time
	Error           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const (
	ReportJobPending = "pending"
	ReportJobReady   = "ready"
	ReportJobFailed  = "failed"
)

// reportMaxAge is a backstop: even a dormant wallet's report gets recomputed
// at least this often, independent of the new-activity check.
const reportMaxAge = 24 * time.Hour

// inFlight dedupes concurrent requests for the identical key within this
// process (the common case: single-replica deployment). A cross-pod race
// (rare, only relevant if this API ever scales beyond one replica) just
// costs a redundant computation, not incorrect data, so it isn't guarded
// beyond this.
var inFlight sync.Map // key: string -> struct{}{}

// canonicalReportKey normalizes methodology/chain/addresses/date-range/format
// into a stable cache key. Including the methodology version invalidates every
// cached report when pricing policy changes, while the same address set in any
// order, case, or whitespace still maps to the same key.
func canonicalReportKey(chain string, addresses []string, start, end, format string) string {
	norm := make([]string, 0, len(addresses))
	for _, a := range addresses {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" {
			norm = append(norm, a)
		}
	}
	sort.Strings(norm)
	return strings.Join([]string{MethodologyVersion, chain, strings.Join(norm, ","), start, end, format}, "|")
}

// reportIsStale reports whether a cached job's computation is out of date:
// past the max-age backstop, or newer on-chain activity exists than what was
// last computed.
func (s *Server) reportIsStale(job *ReportJob, addresses []string) bool {
	if time.Since(job.ComputedThrough) > reportMaxAge {
		return true
	}
	newer, err := s.hasNewerActivity(addresses, job.ComputedThrough)
	if err != nil {
		// Can't confirm freshness; treat as stale rather than silently serve
		// a report we're not sure is still current.
		return true
	}
	return newer
}

// hasNewerActivity checks for any taxable_event touching these addresses
// timestamped after `since`.
func (s *Server) hasNewerActivity(addresses []string, since time.Time) (bool, error) {
	var count int64
	err := s.db.Table("taxable_events").
		Where("(from_addr IN ? OR to_addr IN ?) AND timestamp > ?", addresses, addresses, since).
		Limit(1).
		Count(&count).Error
	return count > 0, err
}

// getOrStartReportJob returns the current status for this key, kicking off a
// fresh computation if there's no usable cached one (missing, stale, failed,
// or resync=true). It never blocks on the computation itself.
func (s *Server) getOrStartReportJob(chain string, addresses []string, start, end time.Time, startStr, endStr, format string, resync bool) (*ReportJob, error) {
	key := canonicalReportKey(chain, addresses, startStr, endStr, format)

	var job ReportJob
	err := s.db.Where("key = ?", key).First(&job).Error
	found := err == nil
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	needsCompute := !found ||
		job.Status == ReportJobFailed ||
		resync ||
		(job.Status == ReportJobReady && s.reportIsStale(&job, addresses))
	// A job already pending (someone else's request, or ours from a moment
	// ago) is left alone; the caller just polls it.
	if !needsCompute || job.Status == ReportJobPending {
		if !found {
			// Never seen this key and don't need to compute (shouldn't
			// happen given needsCompute above, but keep the zero value sane).
			job = ReportJob{Key: key, Chain: chain, Status: ReportJobPending}
		}
		return &job, nil
	}

	if _, already := inFlight.LoadOrStore(key, struct{}{}); already {
		job.Status = ReportJobPending
		return &job, nil
	}

	job.Key, job.Chain, job.Addresses, job.Format = key, chain, strings.Join(addresses, ","), format
	job.Status, job.Error = ReportJobPending, ""
	if err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "error", "chain", "addresses", "format", "updated_at"}),
	}).Create(&job).Error; err != nil {
		inFlight.Delete(key)
		return nil, err
	}

	go s.computeReportJob(key, chain, addresses, start, end, format)

	job.Status = ReportJobPending
	return &job, nil
}

// computeReportJob does the actual work in the background and persists the
// result (or the error) when done.
func (s *Server) computeReportJob(key, chain string, addresses []string, start, end time.Time, format string) {
	defer inFlight.Delete(key)
	computedThrough := nowUTC()

	var all []Row
	for _, addr := range addresses {
		rows, err := s.rowsFor(chain, addr, start, end)
		if err != nil {
			s.db.Model(&ReportJob{}).Where("key = ?", key).
				Updates(map[string]any{"status": ReportJobFailed, "error": err.Error()})
			return
		}
		all = append(all, rows...)
	}

	var buf strings.Builder
	if err := WriteCSV(&buf, format, all); err != nil {
		s.db.Model(&ReportJob{}).Where("key = ?", key).
			Updates(map[string]any{"status": ReportJobFailed, "error": err.Error()})
		return
	}

	s.db.Model(&ReportJob{}).Where("key = ?", key).Updates(map[string]any{
		"status":           ReportJobReady,
		"csv":              buf.String(),
		"row_count":        len(all),
		"computed_through": computedThrough,
		"error":            "",
	})
}

// handleReportJob is a single idempotent endpoint that both kicks off and
// polls a report: GET /report-job?chain=&addresses=a,b,c&start=&end=&format=&resync=
func (s *Server) handleReportJob(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addrParam := q.Get("addresses")
	if addrParam == "" {
		addrParam = q.Get("address")
	}
	addresses := splitNonEmpty(addrParam, ",")
	if len(addresses) == 0 {
		http.Error(w, "addresses required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	format := def(q.Get("format"), "summ")
	startStr, endStr := q.Get("start"), q.Get("end")
	start := dateParam(startStr, time.Time{})
	end := dateParam(endStr, nowUTC().AddDate(0, 0, 1))
	resync := q.Get("resync") == "true"

	job, err := s.getOrStartReportJob(chain, addresses, start, end, startStr, endStr, format, resync)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch job.Status {
	case ReportJobReady:
		writeJSON(w, map[string]any{
			"status":     "ready",
			"csv":        job.CSV,
			"rowCount":   job.RowCount,
			"computedAt": job.ComputedThrough,
		})
	case ReportJobFailed:
		writeJSON(w, map[string]any{"status": "failed", "error": job.Error})
	default:
		writeJSON(w, map[string]any{"status": "pending"})
	}
}

// splitNonEmpty splits on sep and drops empty/whitespace-only pieces.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
