package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type assetReviewRequired struct{ Pending int }

func (e assetReviewRequired) Error() string {
	return fmt.Sprintf("Review %d suspected-spam or identity-mismatch receipt(s) before generating a tax CSV or document. Choose exclusion or enter your own token, quantity, value and basis.", e.Pending)
}

type assetReviewInputError struct{ error }

func ApplyAssetDecisions(rows []Row, decisions AssetDecisions) ([]Row, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return nil, err
	}
	out := append([]Row(nil), rows...)
	seen := map[string]bool{}
	for i := range out {
		row := &out[i]
		if row.AssetIdentity == nil {
			continue
		}
		id := row.AssetIdentity.RecordID
		if id == "" {
			return nil, fmt.Errorf("flagged receipt has no record ID")
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate review record ID")
		}
		seen[id] = true
		decision, ok := decisions[id]
		if !ok {
			continue
		}
		decision, err := NormalizeAssetDecision(decision)
		if err != nil {
			return nil, err
		}
		row.AssetDecision = &decision
		if decision.Mode == AssetDecisionExclude {
			row.Excluded = true
			continue
		}
		qty := decimal.RequireFromString(decision.Quantity)
		value := decimal.RequireFromString(decision.ValueUSD)
		basis := decimal.RequireFromString(decision.CostBasisUSD)
		acquired, _ := time.Parse("2006-01-02", decision.AcquiredDate)
		if acquired.After(row.Time.UTC()) {
			return nil, fmt.Errorf("acquisition date cannot be after the receipt date")
		}
		row.Symbol = decision.Token
		row.Amount = qty
		row.ValueUSD = value
		row.PriceUSD = value.DivRound(qty, 64)
		row.ManualBasisUSD = &basis
		row.ManualAcquiredDate = &acquired
		row.PriceMissing = false
		row.DecimalsAssumed = false
		row.Excluded = false
	}
	for id := range decisions {
		if !seen[id] {
			return nil, fmt.Errorf("asset decision does not match a flagged receipt in this report: %s", id)
		}
	}
	return out, nil
}
func PendingAssetReviews(rows []Row) int {
	n := 0
	for _, r := range rows {
		if r.AssetIdentity != nil && r.AssetDecision == nil {
			n++
		}
	}
	return n
}
func (s *Server) reviewedRows(chain string, addresses []string, start, end time.Time, decisions AssetDecisions, allowPending bool) ([]Row, error) {
	var rows []Row
	seen := map[string]bool{}
	for _, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		next, err := s.rowsFor(chain, addr, start, end)
		if err != nil {
			return nil, err
		}
		rows = append(rows, next...)
	}
	out, err := ApplyAssetDecisions(rows, decisions)
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	if pending := PendingAssetReviews(out); pending > 0 && !allowPending {
		return nil, assetReviewRequired{pending}
	}
	return out, nil
}
func decisionsFromRequest(r *http.Request) (AssetDecisions, error) {
	if len(r.URL.Query()["asset_decisions"]) > 1 {
		return nil, assetReviewInputError{fmt.Errorf("asset_decisions must be supplied once")}
	}
	raw := r.URL.Query().Get("asset_decisions")
	if raw == "" {
		raw = "{}"
	}
	d, err := ParseAssetDecisions(raw)
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	return d, nil
}
func (s *Server) rowsForRequest(r *http.Request, chain string, addresses []string, start, end time.Time) ([]Row, error) {
	decisions, err := decisionsFromRequest(r)
	if err != nil {
		return nil, err
	}
	return s.reviewedRows(chain, addresses, start, end, decisions, false)
}
func writeTaxError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var required assetReviewRequired
	var invalid assetReviewInputError
	if errors.As(err, &required) {
		code = http.StatusConflict
	} else if errors.As(err, &invalid) {
		code = http.StatusBadRequest
	}
	http.Error(w, err.Error(), code)
}
func (s *Server) handleAssetReview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addresses := splitNonEmpty(def(q.Get("addresses"), q.Get("address")), ",")
	if len(addresses) == 0 {
		http.Error(w, "addresses required", 400)
		return
	}
	decisions, err := decisionsFromRequest(r)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	rows, err := s.reviewedRows(def(q.Get("chain"), "mainnet"), addresses, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)), decisions, true)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	reviews := []Row{}
	for _, row := range rows {
		if row.AssetIdentity != nil {
			row.AssetIdentity = row.reviewIdentity()
			reviews = append(reviews, row)
		}
	}
	pending := PendingAssetReviews(rows)
	w.Header().Set("Content-Type", "application/json")
	if pending > 0 {
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"pending": pending, "reviews": reviews, "decisions": decisions})
}
