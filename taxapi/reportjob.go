package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
// on repeat views. Exact persisted event/fee fingerprints invalidate historical
// corrections and deletions too, alongside the max-age backstop and resync.
type ReportJob struct {
	ID                uint   `gorm:"primaryKey"`
	Key               string `gorm:"uniqueIndex"`
	Chain             string
	Addresses         string // comma-joined, sorted; for display/debugging only, not the cache key
	Format            string
	Status            string // pending | ready | failed
	CSV               string `gorm:"type:text"`
	RowCount          int
	ReadinessJSON     string `gorm:"type:text"`
	ReviewFingerprint string
	// ComputedThrough is the computation start, used only for the age backstop.
	// ReviewFingerprint covers the full persisted financial history.
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
// at least this often, independent of the financial fingerprint check.
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

// reportIsStale checks the age backstop. Financial freshness is checked using
// the full persisted event/fee fingerprint before accepting a ready cache.
func (s *Server) reportIsStale(job *ReportJob, _ []string) bool {
	return time.Since(job.ComputedThrough) > reportMaxAge
}

// getOrStartReportJob returns the current status for this key, kicking off a
// fresh computation if the cache is missing, stale or resync=true. Failed jobs
// remain failed until explicit resync. It never blocks on computation itself.
func (s *Server) getOrStartReportJob(chain string, addresses []string, start, end time.Time, startStr, endStr, format string, resync bool, decisions AssetDecisions, tags UserTags) (*ReportJob, error) {
	addresses = normalizedWalletScope(addresses)
	// Check before looking at the cache: a newly retained parse/decode error
	// must immediately suppress stale prior CSV, even within its max age.
	if err := s.requireClassificationReady(chain, addresses, start, end); err != nil {
		return nil, err
	}
	if err := s.requireIBCBasisReady(chain, addresses, end); err != nil {
		return nil, err
	}
	canonical, err := CanonicalAssetDecisionsJSON(decisions)
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	key := canonicalReportKey(chain, addresses, startStr, endStr, format) + fmt.Sprintf("|readiness:v2|financial-history:v1|review:%x", sha256.Sum256([]byte(canonical)))
	if len(tags) > 0 {
		key += fmt.Sprintf("|tags:%x", sha256.Sum256([]byte(tags.Canonical())))
	}

	var job ReportJob
	err = s.db.Where(apiKey, key).First(&job).Error
	found := err == nil
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	stale := found && job.Status == ReportJobReady && s.reportIsStale(&job, addresses)
	if found && job.Status == ReportJobReady && !stale && !resync {
		fingerprint, err := s.currentReviewFingerprint(chain, addresses, start, end)
		if err != nil {
			return nil, err
		}
		stale = fingerprint != job.ReviewFingerprint
	}
	needsCompute := !found || resync || stale
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
		DoUpdates: clause.AssignmentColumns([]string{apiStatus, apiError, apiChain, apiAddresses, apiFormat, "updated_at"}),
	}).Create(&job).Error; err != nil {
		inFlight.Delete(key)
		return nil, err
	}

	go s.computeReportJob(key, chain, addresses, start, end, format, decisions, tags)

	job.Status = ReportJobPending
	return &job, nil
}

// computeReportJob does the actual work in the background and persists the
// result (or the error) when done.
func (s *Server) computeReportJob(key, chain string, addresses []string, start, end time.Time, format string, decisions AssetDecisions, tags UserTags) {
	defer inFlight.Delete(key)
	computedThrough := nowUTC()
	fingerprint, err := s.currentReviewFingerprint(chain, addresses, start, end)
	if err != nil {
		s.failReportJob(key, err)
		return
	}

	all, err := s.reviewedRows(chain, addresses, start, end, decisions, true)
	if err != nil {
		s.db.Model(&ReportJob{}).Where(apiKey, key).Updates(map[string]any{apiStatus: ReportJobFailed, apiError: err.Error()})
		return
	}
	all = ApplyTags(all, tags)
	var buf strings.Builder
	if err := WritePreviewCSV(&buf, format, all); err != nil {
		s.db.Model(&ReportJob{}).Where(apiKey, key).
			Updates(map[string]any{apiStatus: ReportJobFailed, apiError: err.Error()})
		return
	}

	if err := s.requireClassificationReady(chain, addresses, start, end); err != nil {
		s.db.Model(&ReportJob{}).Where(apiKey, key).Updates(map[string]any{apiStatus: ReportJobFailed, apiError: err.Error(), apiCSV: ""})
		return
	}
	if err := s.requireIBCBasisReady(chain, addresses, end); err != nil {
		s.failReportJob(key, err)
		return
	}
	after, err := s.currentReviewFingerprint(chain, addresses, start, end)
	if err != nil {
		s.failReportJob(key, err)
		return
	}
	if after != fingerprint {
		s.failReportJob(key, fmt.Errorf("financial history changed while computing report; retry with resync=true"))
		return
	}
	readinessJSON, _ := json.Marshal(readinessForRows(all))
	s.db.Model(&ReportJob{}).Where(apiKey, key).Updates(map[string]any{
		"readiness_json":     string(readinessJSON),
		apiStatus:            ReportJobReady,
		apiCSV:               buf.String(),
		"row_count":          len(all),
		"computed_through":   computedThrough,
		"review_fingerprint": fingerprint,
		apiError:             "",
	})
}

func (s *Server) failReportJob(key string, err error) {
	s.db.Model(&ReportJob{}).Where(apiKey, key).Updates(map[string]any{apiStatus: ReportJobFailed, apiError: err.Error(), apiCSV: ""})
}

// handleReportJob is a single idempotent endpoint that both kicks off and
// polls a report: GET /report-job?chain=&addresses=a,b,c&start=&end=&format=&resync=
func (s *Server) handleReportJob(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addrParam := q.Get(apiAddresses)
	if addrParam == "" {
		addrParam = q.Get(apiAddress)
	}
	addresses := splitNonEmpty(addrParam, ",")
	if len(addresses) == 0 {
		http.Error(w, "addresses required", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get(apiChain))
	format := def(q.Get(apiFormat), apiSumm)
	if format != apiSumm && format != apiCryptotaxcalculator {
		http.Error(w, "report-job is preview only; use /events for tax exports", 400)
		return
	}
	decisions, err := decisionsFromRequest(r)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	tags, err := tagsFromRequest(r)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	startStr, endStr := q.Get(apiStart), q.Get(apiEnd)
	start := dateParam(startStr, time.Time{})
	end := dateParam(endStr, nowUTC().AddDate(0, 0, 1))
	resync := q.Get("resync") == apiTrue

	job, err := s.getOrStartReportJob(chain, addresses, start, end, startStr, endStr, format, resync, decisions, tags)
	if err != nil {
		writeTaxError(w, err)
		return
	}

	w.Header().Set(apiContentType, apiJSONContentType)
	switch job.Status {
	case ReportJobReady:
		var readiness ReportReadiness
		if err := json.Unmarshal([]byte(job.ReadinessJSON), &readiness); err != nil {
			writeTaxError(w, fmt.Errorf("cached report readiness unavailable: %w", err))
			return
		}
		writeJSON(w, map[string]any{
			"preview_only": true,
			apiReadiness:   readiness,
			apiStatus:      ReportJobReady,
			apiCSV:         job.CSV,
			"rowCount":     job.RowCount,
			"computedAt":   job.ComputedThrough,
		})
	case ReportJobFailed:
		writeJSON(w, map[string]any{apiStatus: ReportJobFailed, apiError: job.Error})
	default:
		writeJSON(w, map[string]any{apiStatus: ReportJobPending})
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
