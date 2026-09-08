package taxapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ValidatorKey authorizes a validator's site to call the delegator-report
// endpoint (INF-210). Provisioned by an admin (this is a manually-onboarded
// pilot phase, not self-serve signup yet) — Origin is checked against
// AllowedOrigin when the browser sends one, so a leaked key still can't be
// embedded from an arbitrary site.
type ValidatorKey struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	ValidatorName string    `json:"validator_name"`
	APIKey        string    `json:"api_key" gorm:"uniqueIndex"`
	AllowedOrigin string    `json:"allowed_origin"` // e.g. "https://myvalidator.example"; "" = no origin check
	CreatedAt     time.Time `json:"created_at"`
}

// ValidatorReportUsage logs one delegator-report call so validator usage is
// attributable (INF-210: "so we can see which validators drive usage").
type ValidatorReportUsage struct {
	ID             uint `gorm:"primaryKey"`
	ValidatorKeyID uint `gorm:"index:idx_usage_key"`
	Address        string
	CreatedAt      time.Time
}

func newAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// POST /admin/validator-keys {validator_name, allowed_origin?} — admin-only
// provisioning (the frontend gates who can reach this, same trust boundary
// as the wasm-submissions admin routes).
func (s *Server) handleCreateValidatorKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ValidatorName string `json:"validator_name"`
		AllowedOrigin string `json:"allowed_origin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.ValidatorName = strings.TrimSpace(body.ValidatorName)
	if body.ValidatorName == "" {
		http.Error(w, "validator_name is required", http.StatusBadRequest)
		return
	}
	key, err := newAPIKey()
	if err != nil {
		http.Error(w, "could not generate a key", http.StatusInternalServerError)
		return
	}
	vk := ValidatorKey{ValidatorName: body.ValidatorName, APIKey: key, AllowedOrigin: strings.TrimSpace(body.AllowedOrigin)}
	if err := s.db.Create(&vk).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(vk)
}

// GET /admin/validator-keys — list, with a per-key usage count (INF-210
// attribution).
func (s *Server) handleListValidatorKeys(w http.ResponseWriter, r *http.Request) {
	var keys []ValidatorKey
	if err := s.db.Order("created_at desc").Find(&keys).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type keyWithUsage struct {
		ValidatorKey
		RequestCount int64 `json:"request_count"`
	}
	out := make([]keyWithUsage, 0, len(keys))
	for _, k := range keys {
		var count int64
		s.db.Model(&ValidatorReportUsage{}).Where("validator_key_id = ?", k.ID).Count(&count)
		out = append(out, keyWithUsage{ValidatorKey: k, RequestCount: count})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
}

// --- rate limiting: same in-process limiter type as wasmintake.go, keyed by
// remote address, but its own (much higher) limit — many different
// delegators legitimately hit this through one validator's widget, unlike a
// wasm submission. This service sits behind an in-cluster proxy; if that
// proxy doesn't preserve the original client IP, every request through it
// shares one bucket — a known limitation, not attempted to fix here since
// wasmintake.go's limiter has the same characteristic (keep them consistent).
const (
	delegatorReportRateLimit  = 30
	delegatorReportRateWindow = time.Minute
)

var delegatorReportLimiter = newLimiter(delegatorReportRateLimit, delegatorReportRateWindow)

func originAllowed(origin, allowed string) bool {
	if allowed == "" || origin == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSuffix(origin, "/"), strings.TrimSuffix(allowed, "/"))
}

// DelegatorReport is the compact summary a validator's embedded widget shows
// a delegator: their own staking income from that validator's site, not a
// full transaction export (see /events for that).
type DelegatorReport struct {
	Address            string `json:"address"`
	Chain              string `json:"chain"`
	PeriodStart        string `json:"period_start,omitempty"`
	PeriodEnd          string `json:"period_end,omitempty"`
	RewardsAmount      string `json:"rewards_amount"`
	Symbol             string `json:"symbol"`
	RewardsUSD         string `json:"rewards_usd"`
	TaxableEvents      int    `json:"taxable_events"`
	PriceMissingRows   int    `json:"price_missing_rows"`
	RecognitionPolicy  string `json:"recognition_policy"`
	MethodologyVersion string `json:"methodology_version"`
	PoweredBy          string `json:"powered_by"`
}

// GET /delegator-report?address=&api_key=&chain=&start=&end= — the
// purpose-built variant on top of the same rowsFor pipeline every report
// uses, scoped to one delegator and branded for an embeddable widget
// (INF-210).
func (s *Server) handleDelegatorReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := strings.TrimSpace(q.Get("address"))
	if addr == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}

	apiKey := strings.TrimSpace(q.Get("api_key"))
	if apiKey == "" {
		http.Error(w, "api_key is required", http.StatusUnauthorized)
		return
	}
	var vk ValidatorKey
	if s.db.Where("api_key = ?", apiKey).Find(&vk).RowsAffected == 0 {
		http.Error(w, "invalid api_key", http.StatusUnauthorized)
		return
	}
	if !originAllowed(r.Header.Get("Origin"), vk.AllowedOrigin) {
		http.Error(w, "origin not authorized for this key", http.StatusForbidden)
		return
	}
	if !delegatorReportLimiter.allow(r.RemoteAddr) {
		http.Error(w, "rate limit exceeded, try again shortly", http.StatusTooManyRequests)
		return
	}

	chain := def(q.Get("chain"), "mainnet")
	start := dateParam(q.Get("start"), time.Time{})
	end := dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1))
	rows, err := s.rowsForRequest(r, chain, []string{addr}, start, end)
	if err != nil {
		writeTaxError(w, err)
		return
	}

	rewards := decimal.Zero
	usd := decimal.Zero
	priceMissing := 0
	symbol := s.native.Symbol
	for _, row := range rows {
		if row.Excluded {
			continue
		}
		if row.Category != "reward" && row.Category != "commission" {
			continue
		}
		rewards = rewards.Add(row.Amount)
		usd = usd.Add(row.ValueUSD)
		if row.PriceMissing {
			priceMissing++
		}
		if row.Symbol != "" {
			symbol = row.Symbol
		}
	}

	s.db.Create(&ValidatorReportUsage{ValidatorKeyID: vk.ID, Address: addr})

	report := DelegatorReport{
		Address:            addr,
		Chain:              chain,
		RewardsAmount:      rewards.String(),
		Symbol:             symbol,
		RewardsUSD:         usd.StringFixed(2),
		TaxableEvents:      len(rows),
		PriceMissingRows:   priceMissing,
		RecognitionPolicy:  RecognitionPolicyDefault,
		MethodologyVersion: MethodologyVersion,
		PoweredBy:          "BryanLabs (tax.bryanlabs.net)",
	}
	if q.Get("start") != "" {
		report.PeriodStart = q.Get("start")
	}
	if q.Get("end") != "" {
		report.PeriodEnd = q.Get("end")
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}
