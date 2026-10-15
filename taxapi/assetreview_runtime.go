package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type assetReviewRequired struct{ Pending int }

func (e assetReviewRequired) Error() string {
	return fmt.Sprintf("%d flagged receipt(s) need a decision before a tax CSV, document, journal or positions file can be produced. Each is a suspected-spam or identity-mismatch receipt: exclude it as valueless, or supply your own token and USD value. Pass the decisions as the asset_decisions parameter (JSON object; keys are a record_id from /asset-review, denom:<raw_denom> for every receipt of that token, or policy:spam for every suspected-spam receipt; spam_policy=valueless is the same as the policy key; see /tax/api-docs), or save them in the report on the website first.", e.Pending)
}

type assetReviewInputError struct{ error }

// resolveDecision picks the most specific decision for a flagged row: the
// receipt's own record ID, then denom:<raw_denom>, then policy:spam (which
// only reaches receipts flagged as suspected spam).
func resolveDecision(row *Row, decisions AssetDecisions) (AssetDecision, string, bool) {
	id := row.AssetIdentity.RecordID
	if d, ok := decisions[id]; ok {
		return d, id, true
	}
	if denom := row.AssetIdentity.RawDenom; denom != "" {
		key := DecisionKeyDenomPrefix + denom
		if d, ok := decisions[key]; ok {
			return d, key, true
		}
	}
	if row.AssetIdentity.SuspectedSpam {
		if d, ok := decisions[DecisionKeySpamPolicy]; ok {
			return d, DecisionKeySpamPolicy, true
		}
	}
	return AssetDecision{}, "", false
}

// materializeDecision turns a denom or policy decision into the per-receipt
// form the exporters understand, stamping applied_from with the key so the
// audit trail shows the receipt was decided by a rule rather than by hand.
func materializeDecision(key string, decision AssetDecision, row *Row) (AssetDecision, error) {
	kind := decisionKeyKind(key)
	normalized, err := NormalizeKeyedDecision(key, decision)
	if err != nil {
		return AssetDecision{}, err
	}
	if kind == "record" {
		return normalized, nil
	}
	if normalized.Mode == AssetDecisionExclude {
		return AssetDecision{Mode: AssetDecisionExclude, AppliedFrom: key}, nil
	}
	if !row.Amount.IsPositive() {
		return AssetDecision{}, fmt.Errorf("%s: receipt %s has no positive quantity to price; decide it by record ID", key, row.AssetIdentity.RecordID)
	}
	unit := decimal.RequireFromString(normalized.UnitValueUSD)
	value := row.Amount.Mul(unit).RoundBank(18)
	return AssetDecision{
		Mode:         AssetDecisionOverride,
		Token:        normalized.Token,
		Quantity:     row.Amount.String(),
		ValueUSD:     value.String(),
		CostBasisUSD: value.String(),
		AcquiredDate: row.Time.UTC().Format("2006-01-02"),
		AppliedFrom:  key,
	}, nil
}

func ApplyAssetDecisions(rows []Row, decisions AssetDecisions) ([]Row, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return nil, err
	}
	out := append([]Row(nil), rows...)
	seen := map[string]bool{}
	used := map[string]bool{}
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
		raw, key, ok := resolveDecision(row, decisions)
		if !ok {
			continue
		}
		used[key] = true
		decision, err := materializeDecision(key, raw, row)
		if err != nil {
			return nil, err
		}
		row.AssetDecision = &decision
		if decision.Mode == AssetDecisionExclude {
			// Keep the indexed quantity and identity evidence intact, while making
			// the user's explicit valueless treatment machine-readable in previews
			// and the generic audit CSV. Excluded remains the guard that keeps this
			// unverified asset out of FIFO and native-asset totals.
			zero := decimal.Zero
			row.PriceUSD = zero
			row.ValueUSD = zero
			row.ManualBasisUSD = &zero
			row.PriceMissing = false
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
	// A record ID that matches nothing is a stale or foreign decision and is
	// refused. A denom or policy key that matches nothing is harmless: the token
	// simply did not appear in this period, so a remembered rule can be sent
	// with every report.
	for key := range decisions {
		if !used[key] && decisionKeyKind(key) == "record" {
			return nil, fmt.Errorf("asset decision does not match a flagged receipt in this report: %s", key)
		}
	}
	return out, nil
}

// UnmatchedDecisionKeys lists the denom and policy keys that decided nothing
// in these rows, so a client can show which remembered rules were idle.
func UnmatchedDecisionKeys(rows []Row, decisions AssetDecisions) []string {
	used := map[string]bool{}
	for _, r := range rows {
		if r.AssetDecision != nil && r.AssetDecision.AppliedFrom != "" {
			used[r.AssetDecision.AppliedFrom] = true
		}
		if r.AssetIdentity != nil && r.AssetDecision != nil && r.AssetDecision.AppliedFrom == "" {
			used[r.AssetIdentity.RecordID] = true
		}
	}
	var out []string
	for key := range decisions {
		if !used[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
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
	// spam_policy=valueless is shorthand for {"policy:spam": {"mode": "exclude"}}:
	// every receipt flagged as suspected spam without a more specific decision
	// is treated as valueless. It becomes an ordinary decision key so caches,
	// saved reports and the audit trail all carry it.
	switch policy := r.URL.Query().Get("spam_policy"); policy {
	case "", "ask":
	case "valueless":
		if d == nil {
			d = AssetDecisions{}
		}
		d[DecisionKeySpamPolicy] = AssetDecision{Mode: AssetDecisionExclude}
	default:
		return nil, assetReviewInputError{fmt.Errorf("spam_policy must be ask or valueless")}
	}
	return d, nil
}
func (s *Server) rowsForRequest(r *http.Request, chain string, addresses []string, start, end time.Time) ([]Row, error) {
	decisions, err := decisionsFromRequest(r)
	if err != nil {
		return nil, err
	}
	tags, err := tagsFromRequest(r)
	if err != nil {
		return nil, err
	}
	rows, err := s.reviewedRows(chain, addresses, start, end, decisions, false)
	if err != nil {
		return nil, err
	}
	return ApplyTags(rows, tags), nil
}

// errAwaitingCorrection is returned while a wallet still has staking rows
// written by a methodology older than the current one. A replay is rewriting
// them; until it reaches this wallet no totals are returned rather than a
// wrong number. It is a temporary service state, not a server fault, so it is
// surfaced as 503 with a Retry-After.
var errAwaitingCorrection = errors.New("historical staking data for this wallet is awaiting correction under the current methodology; no totals are returned until the replay reaches it. Retry later")

func writeTaxError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var required assetReviewRequired
	var invalid assetReviewInputError
	if errors.As(err, &required) {
		code = http.StatusConflict
	} else if errors.As(err, &invalid) {
		code = http.StatusBadRequest
	} else if errors.Is(err, errAwaitingCorrection) {
		code = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1800")
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
	rows, err := s.reviewedRows(chainParam(q.Get("chain")), addresses, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)), decisions, true)
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pending":                 pending,
		"reviews":                 reviews,
		"groups":                  ReviewGroups(reviews, decisions),
		"decisions":               decisions,
		"unmatched_decision_keys": UnmatchedDecisionKeys(rows, decisions),
		"reason_glossary":         ReviewReasonGlossary(),
		"decision_keys": map[string]string{
			"record_id":   "one receipt; override carries quantity, value_usd, cost_basis_usd and acquired_date totals",
			"denom:<raw>": "every flagged receipt of that token in the report; override carries token and unit_value_usd, each receipt is priced quantity times unit on its own date",
			"policy:spam": "every receipt flagged as suspected spam without a more specific decision; exclude only (spam_policy=valueless is the same thing)",
		},
	})
}

// ReviewGroup is one flagged token: the unit a person actually decides on.
type ReviewGroup struct {
	Denom         string         `json:"denom"`
	DecisionKey   string         `json:"decision_key"`
	ReportedLabel string         `json:"reported_label"`
	TokenName     string         `json:"token_name"`
	BaseToken     string         `json:"base_token"`
	SourceChain   string         `json:"source_chain"`
	ChannelPath   string         `json:"channel_path"`
	SuspectedSpam bool           `json:"suspected_spam"`
	ReasonCodes   []string       `json:"reason_codes"`
	Receipts      int            `json:"receipts"`
	Pending       int            `json:"pending"`
	FirstDate     string         `json:"first_date"`
	LastDate      string         `json:"last_date"`
	TotalQuantity string         `json:"total_quantity"`
	SampleMemo    string         `json:"sample_memo"`
	RecordIDs     []string       `json:"record_ids"`
	Decision      *AssetDecision `json:"decision,omitempty"`
}

// ReviewGroups folds flagged rows by raw denomination, newest token first by
// pending count so the biggest pile is at the top.
func ReviewGroups(reviews []Row, decisions AssetDecisions) []ReviewGroup {
	byDenom := map[string]*ReviewGroup{}
	order := []string{}
	totals := map[string]decimal.Decimal{}
	for _, r := range reviews {
		a := r.AssetIdentity
		if a == nil {
			continue
		}
		denom := a.RawDenom
		if denom == "" {
			denom = r.Denom
		}
		g, ok := byDenom[denom]
		if !ok {
			g = &ReviewGroup{Denom: denom, DecisionKey: DecisionKeyDenomPrefix + denom, ReportedLabel: a.ReportedLabel, TokenName: a.TokenName, BaseToken: a.BaseToken, SourceChain: a.SourceChain, ChannelPath: a.ChannelPath}
			if d, has := decisions[g.DecisionKey]; has {
				dd := d
				g.Decision = &dd
			}
			byDenom[denom] = g
			order = append(order, denom)
			totals[denom] = decimal.Zero
		}
		g.Receipts++
		if r.AssetDecision == nil {
			g.Pending++
		}
		g.SuspectedSpam = g.SuspectedSpam || a.SuspectedSpam
		for _, c := range a.ReasonCodes {
			found := false
			for _, e := range g.ReasonCodes {
				if e == c {
					found = true
				}
			}
			if !found {
				g.ReasonCodes = append(g.ReasonCodes, c)
			}
		}
		day := r.Time.UTC().Format("2006-01-02")
		if g.FirstDate == "" || day < g.FirstDate {
			g.FirstDate = day
		}
		if day > g.LastDate {
			g.LastDate = day
		}
		if g.SampleMemo == "" && a.Memo != "" {
			g.SampleMemo = a.Memo
		}
		g.RecordIDs = append(g.RecordIDs, a.RecordID)
		totals[denom] = totals[denom].Add(r.Amount)
	}
	out := make([]ReviewGroup, 0, len(order))
	for _, denom := range order {
		g := byDenom[denom]
		g.TotalQuantity = totals[denom].String()
		if g.ReasonCodes == nil {
			g.ReasonCodes = []string{}
		}
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pending != out[j].Pending {
			return out[i].Pending > out[j].Pending
		}
		return out[i].Receipts > out[j].Receipts
	})
	return out
}
