package taxapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WasmSubmission is a user-submitted request to index/classify a CosmWasm
// contract, or to report a classification gap on one we already handle
// (INF-209). Classification here is event-schema-based, not a per-contract
// allowlist (see tax/parser.go nftSaleEvents scanning for "wasm-finalize-
// sale"), so "approving" a submission is a human engineering decision — does
// this contract's event shape match a pattern we can add, or is it a
// genuinely new shape needing a new parser — tracked here for triage, not
// auto-actioned. Per the ticket's own guidance, this stays entirely within
// cosmos-indexer-sdk (which we fully control); wasm-indexer (source not
// located) is treated as read-only and untouched.
type WasmSubmission struct {
	ID              uint      `json:"id" gorm:"primaryKey"`
	Chain           string    `json:"chain"`
	ContractAddress string    `json:"contract_address" gorm:"index:idx_wasm_sub_contract"`
	Kind            string    `json:"kind"` // "new_contract" | "capability_gap"
	Contact         string    `json:"contact,omitempty"`
	Note            string    `json:"note,omitempty"`
	Status          string    `json:"status" gorm:"index:idx_wasm_sub_status"` // submitted | under_review | approved | declined
	StatusReason    string    `json:"status_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const (
	WasmSubmissionSubmitted   = "submitted"
	WasmSubmissionUnderReview = "under_review"
	WasmSubmissionApproved    = "approved"
	WasmSubmissionDeclined    = "declined"
)

var validWasmStatus = map[string]bool{
	WasmSubmissionSubmitted:   true,
	WasmSubmissionUnderReview: true,
	WasmSubmissionApproved:    true,
	WasmSubmissionDeclined:    true,
}

// --- naive in-memory rate limiting: N submissions per (client, contract) per
// window. This deployment runs a single replica (see the k8s manifest), so an
// in-memory limiter is real spam resistance here, not a distributed one.
type submissionLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var wasmLimiter = &submissionLimiter{hits: map[string][]time.Time{}}

const (
	submissionRateLimit  = 5
	submissionRateWindow = time.Hour
)

func (l *submissionLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-submissionRateWindow)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= submissionRateLimit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}

type wasmCoverage struct {
	Classified   bool     `json:"classified"`
	Capabilities []string `json:"capabilities"`
}

// contractCoverage checks whether we've ever produced a taxable event
// involving this contract (as the NFT collection, or as a from/to
// counterparty, e.g. a swap pool) — a direct, factual answer from what we've
// actually classified, not a guess.
func (s *Server) contractCoverage(contract string) wasmCoverage {
	var categories []string
	s.db.Table("taxable_events").
		Where("asset LIKE ? OR from_addr = ? OR to_addr = ?", contract+"/%", contract, contract).
		Distinct("category").
		Pluck("category", &categories)
	return wasmCoverage{Classified: len(categories) > 0, Capabilities: categories}
}

// GET /wasm/coverage?contract_address=&chain=
func (s *Server) handleWasmCoverage(w http.ResponseWriter, r *http.Request) {
	contract := strings.TrimSpace(r.URL.Query().Get("contract_address"))
	if contract == "" {
		http.Error(w, "contract_address is required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.contractCoverage(contract))
}

// POST /wasm/submissions {chain, contract_address, kind, contact?, note?}
// kind: "new_contract" (never seen this one) or "capability_gap" (we index
// it, but miss some activity type on it). Rate-limited per (remote addr,
// contract) to stay spam-safe without needing accounts.
func (s *Server) handleCreateWasmSubmission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Chain           string `json:"chain"`
		ContractAddress string `json:"contract_address"`
		Kind            string `json:"kind"`
		Contact         string `json:"contact"`
		Note            string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.ContractAddress = strings.TrimSpace(body.ContractAddress)
	if body.ContractAddress == "" {
		http.Error(w, "contract_address is required", http.StatusBadRequest)
		return
	}
	if body.Kind != "new_contract" && body.Kind != "capability_gap" {
		body.Kind = "new_contract"
	}

	if !wasmLimiter.allow(r.RemoteAddr + "|" + body.ContractAddress) {
		http.Error(w, "too many submissions for this contract, try again later", http.StatusTooManyRequests)
		return
	}

	sub := WasmSubmission{
		Chain:           def(body.Chain, "mainnet"),
		ContractAddress: body.ContractAddress,
		Kind:            body.Kind,
		Contact:         strings.TrimSpace(body.Contact),
		Note:            strings.TrimSpace(body.Note),
		Status:          WasmSubmissionSubmitted,
	}
	if err := s.db.Create(&sub).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sub)
}

// GET /wasm/submissions/status?contract_address= — current coverage plus the
// most recent submission (if any) for a contract, so a user can check back
// after submitting: indexed / under review / declined-with-reason.
func (s *Server) handleWasmSubmissionStatus(w http.ResponseWriter, r *http.Request) {
	contract := strings.TrimSpace(r.URL.Query().Get("contract_address"))
	if contract == "" {
		http.Error(w, "contract_address is required", http.StatusBadRequest)
		return
	}
	resp := map[string]any{"coverage": s.contractCoverage(contract)}
	var sub WasmSubmission
	if s.db.Where("contract_address = ?", contract).Order("created_at desc").Limit(1).Find(&sub).RowsAffected > 0 {
		resp["submission"] = sub
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// GET /wasm/submissions?status= — admin triage list. No auth here: this
// backend is internal-only (reached over in-cluster DNS), same trust
// boundary as every other endpoint; the frontend gates who can reach this
// route to a real person before proxying the request.
func (s *Server) handleListWasmSubmissions(w http.ResponseWriter, r *http.Request) {
	tx := s.db.Order("created_at desc")
	if status := r.URL.Query().Get("status"); status != "" {
		tx = tx.Where("status = ?", status)
	}
	var subs []WasmSubmission
	if err := tx.Find(&subs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"submissions": subs})
}

// PATCH /wasm/submissions/{id} {status, reason} — admin decision. Approving
// doesn't auto-configure anything (classification is code, not data); it
// records that a human looked at it and either shipped/will-ship a parser
// change or confirmed the gap, with a reason either way.
func (s *Server) handleUpdateWasmSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid submission id", http.StatusBadRequest)
		return
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !validWasmStatus[body.Status] {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}
	res := s.db.Model(&WasmSubmission{}).Where("id = ?", id).
		Updates(map[string]any{"status": body.Status, "status_reason": body.Reason})
	if res.Error != nil {
		http.Error(w, res.Error.Error(), http.StatusInternalServerError)
		return
	}
	if res.RowsAffected == 0 {
		http.Error(w, "submission not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
