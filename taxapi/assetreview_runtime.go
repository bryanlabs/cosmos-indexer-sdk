package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	if kind == apiRecord {
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
		AcquiredDate: row.Time.UTC().Format(apiDateLayout),
		AppliedFrom:  key,
	}, nil
}

func ApplyAssetDecisions(rows []Row, decisions AssetDecisions) ([]Row, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return nil, err
	}
	match := newDecisionMatch()
	out, err := match.apply(rows, decisions)
	if err != nil {
		return nil, err
	}
	if err := match.check(decisions); err != nil {
		return nil, err
	}
	return out, nil
}

// decisionMatch carries what applying decisions has seen and matched so far.
// A report read in windows applies the decisions to each window with one
// decisionMatch and checks that every keyed decision matched once, after the
// last window, as ApplyAssetDecisions does for a report read whole.
type decisionMatch struct {
	seen, used, classified, semanticSeen map[string]bool
}

func newDecisionMatch() *decisionMatch {
	return &decisionMatch{seen: map[string]bool{}, used: map[string]bool{}, classified: map[string]bool{}, semanticSeen: map[string]bool{}}
}

// apply applies the already validated decisions to rows and records which
// keys matched. It refuses what a row alone shows to be wrong; a key that
// matches nothing is left to check.
func (m *decisionMatch) apply(rows []Row, decisions AssetDecisions) ([]Row, error) {
	out := append([]Row(nil), rows...)
	seen, used := m.seen, m.used
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
		acquired, _ := time.Parse(apiDateLayout, decision.AcquiredDate)
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
	if err := applySemanticDecisionsTo(out, decisions, used, m.semanticSeen, m.classified); err != nil {
		return nil, err
	}
	applyUserPrices(out, decisions)
	return out, nil
}

// check refuses a record ID that matched nothing: a stale or foreign
// decision, and a classification on a record that waits on no
// classification review. A denom, policy or price key that matches nothing is harmless:
// the token did not appear in this period, or a reference price has since
// covered the row, so a remembered rule can be sent with every report.
func (m *decisionMatch) check(decisions AssetDecisions) error {
	for key := range decisions {
		kind := decisionKeyKind(key)
		if !m.used[key] && (kind == apiRecord || kind == decisionKindSemantic) {
			return fmt.Errorf("asset decision does not match a flagged receipt in this report: %s", key)
		}
		if kind == apiRecord && decisions[key].Classification != "" && !m.classified[key] {
			return fmt.Errorf("asset decision %q: classification on a receipt that waits on no classification review", key)
		}
	}
	return nil
}

// userPriceFor is the user's unit price for an unpriced row and the key it
// came from: the row's own price key (a total value, divided by the
// quantity) first, then the denom's price for the row's UTC day.
func userPriceFor(r Row, decisions AssetDecisions) (decimal.Decimal, string, bool) {
	if k := r.rowKey(); k != "" && r.Amount.IsPositive() {
		key := DecisionKeyPricePrefix + k
		if d, ok := decisions[key]; ok && d.ValueUSD != "" {
			return decimal.RequireFromString(d.ValueUSD).DivRound(r.Amount, 18), key, true
		}
	}
	key := DecisionKeyPricePrefix + r.Denom + "@" + r.Time.UTC().Format(apiDateLayout)
	if d, ok := decisions[key]; ok && d.UnitValueUSD != "" {
		return decimal.RequireFromString(d.UnitValueUSD), key, true
	}
	return decimal.Decimal{}, "", false
}

// applyUserPrices values the rows still missing a price with the user's own
// price: keys. Only a missing price is filled: an oracle, pegged or implied
// price is never replaced, and a flagged receipt is valued only through its
// identity decision. The row is marked user-supplied so every export says so.
func applyUserPrices(rows []Row, decisions AssetDecisions) {
	if !hasUserPrices(decisions) {
		return
	}
	for i := range rows {
		r := &rows[i]
		if !r.PriceMissing || r.AssetIdentity != nil || r.Excluded {
			continue
		}
		price, key, ok := userPriceFor(*r, decisions)
		if !ok {
			continue
		}
		r.PriceUSD = price
		r.ValueUSD = r.Amount.Mul(price)
		if d := decisions[key]; d.ValueUSD != "" {
			r.ValueUSD = decimal.RequireFromString(d.ValueUSD)
		}
		r.PriceMissing = false
		r.PriceUserSupplied = true
		r.UserPriceKey = key
	}
}

func hasUserPrices(decisions AssetDecisions) bool {
	for key := range decisions {
		if decisionKeyKind(key) == decisionKindPrice {
			return true
		}
	}
	return false
}

// hasUserPriceRowKeys reports whether a price: key names a single row, which
// needs the rows' message indexes loaded first.
func hasUserPriceRowKeys(decisions AssetDecisions) bool {
	for key := range decisions {
		if decisionKeyKind(key) == decisionKindPrice && userPriceRowKey(key) {
			return true
		}
	}
	return false
}

// UnmatchedDecisionKeys lists the denom, policy and price keys that decided
// nothing in these rows, so a client can show which remembered rules were
// idle. A price key whose rows now have a reference price is idle too.
func UnmatchedDecisionKeys(rows []Row, decisions AssetDecisions) []string {
	used := map[string]bool{}
	markDecisionKeysUsed(rows, used)
	return unmatchedDecisionKeys(used, decisions)
}

// markDecisionKeysUsed adds to used every decision key a row was decided by.
func markDecisionKeysUsed(rows []Row, used map[string]bool) {
	for _, r := range rows {
		if r.AssetDecision != nil && r.AssetDecision.AppliedFrom != "" {
			used[r.AssetDecision.AppliedFrom] = true
		}
		if r.AssetIdentity != nil && r.AssetDecision != nil && r.AssetDecision.AppliedFrom == "" {
			used[r.AssetIdentity.RecordID] = true
		}
		if r.UserPriceKey != "" {
			used[r.UserPriceKey] = true
		}
		if r.SemanticDecision != nil {
			used[r.SemanticRecordID] = true
		}
	}
}

func unmatchedDecisionKeys(used map[string]bool, decisions AssetDecisions) []string {
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
	addresses = normalizedWalletScope(addresses)
	input, err := s.readReportInput(chain, addresses, start, end, false)
	if err != nil {
		return nil, err
	}
	return s.reviewedInputRows(chain, addresses, input, decisions, allowPending)
}

func applyReviewedRows(rows []Row, decisions AssetDecisions, allowPending bool) ([]Row, error) {
	out, err := ApplyAssetDecisions(rows, decisions)
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	if readiness := readinessForRows(out); readiness.PendingSemanticReviews > 0 && !allowPending {
		return nil, reportReadinessRequired{readiness}
	}
	if pending := PendingAssetReviews(out); pending > 0 && !allowPending {
		return nil, assetReviewRequired{pending}
	}
	return out, nil
}

func decisionsFromRequest(r *http.Request) (AssetDecisions, error) {
	if len(r.URL.Query()[apiAssetDecisions]) > 1 {
		return nil, assetReviewInputError{fmt.Errorf("asset_decisions must be supplied once")}
	}
	raw := r.URL.Query().Get(apiAssetDecisions)
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
	switch policy := r.URL.Query().Get(apiSpamPolicy); policy {
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
	return s.applyTags(rows, tags)
}

// applyTags loads the message indexes when a row key needs them, then
// applies the tags.
func (s *Server) applyTags(rows []Row, tags UserTags) ([]Row, error) {
	if tags.hasRowKeys() && !messageIndexesLoaded(rows) {
		if err := s.loadMessageIndexes(rows); err != nil {
			return nil, err
		}
	}
	return ApplyTags(rows, tags), nil
}

const messageIndexChunk = 5000

// messageIndexesLoaded reports whether every row with a message already has
// its message index.
func messageIndexesLoaded(rows []Row) bool {
	for _, r := range rows {
		if r.MessageID != 0 && !r.hasMessageIndex {
			return false
		}
	}
	return true
}

// loadMessageIndexes sets each row's MessageIndex from its stored message,
// a primary-key lookup per message. A message index never changes once
// written, so reading it outside the report's snapshot is safe.
func (s *Server) loadMessageIndexes(rows []Row) error {
	seen := map[uint]bool{}
	var ids []uint
	for _, r := range rows {
		if r.MessageID != 0 && !seen[r.MessageID] {
			seen[r.MessageID] = true
			ids = append(ids, r.MessageID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	index := make(map[uint]int, len(ids))
	for start := 0; start < len(ids); start += messageIndexChunk {
		chunk := ids[start:min(start+messageIndexChunk, len(ids))]
		var got []struct {
			ID           uint
			MessageIndex int
		}
		if err := s.db.Raw("SELECT id, message_index FROM messages WHERE id IN ?", chunk).Scan(&got).Error; err != nil {
			return err
		}
		for _, g := range got {
			index[g.ID] = g.MessageIndex
		}
	}
	for i := range rows {
		if mi, ok := index[rows[i].MessageID]; ok {
			rows[i].MessageIndex, rows[i].hasMessageIndex = mi, true
		}
	}
	return nil
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
	var readiness reportReadinessRequired
	var tooLarge reportTooLarge
	if errors.As(err, &readiness) {
		w.Header().Set(apiContentType, apiJSONContentType)
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{apiStatus: "blocked", apiError: err.Error(), apiReadiness: readiness.Readiness})
		return
	}
	switch {
	case errors.As(err, &required):
		code = http.StatusConflict
	case errors.As(err, &invalid):
		code = http.StatusBadRequest
	case errors.As(err, &tooLarge):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, errAwaitingCorrection):
		code = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1800")
	}
	http.Error(w, err.Error(), code)
}

// assetReviewRows reads the rows /asset-review answers with, the report's
// readiness and the decision keys no row matched. A period over the report
// row cap is read in windows, keeping only the rows a person reviews. A
// period that grows over the cap between the count and the whole read is
// read in windows too.
func (s *Server) assetReviewRows(chain string, addresses []string, start, end time.Time, decisions AssetDecisions) ([]Row, ReportReadiness, []string, error) {
	addresses = normalizedWalletScope(addresses)
	over, err := s.overReportRowCap(addresses, start, end)
	if err != nil {
		return nil, ReportReadiness{}, nil, err
	}
	if !over {
		rows, err := s.reviewedRows(chain, addresses, start, end, decisions, true)
		var tooLarge reportTooLarge
		if !errors.As(err, &tooLarge) {
			if err != nil {
				return nil, ReportReadiness{}, nil, err
			}
			return rows, readinessForRows(rows), UnmatchedDecisionKeys(rows, decisions), nil
		}
	}
	review, err := s.readReviewWindows(chain, addresses, start, end, decisions)
	if err != nil {
		return nil, ReportReadiness{}, nil, err
	}
	return review.Rows, review.readiness(), unmatchedDecisionKeys(review.Used, decisions), nil
}

func (s *Server) handleAssetReview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addresses := splitNonEmpty(def(q.Get(apiAddresses), q.Get(apiAddress)), ",")
	if len(addresses) == 0 {
		http.Error(w, "addresses required", 400)
		return
	}
	decisions, err := decisionsFromRequest(r)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	rows, readiness, unmatched, err := s.assetReviewRows(chainParam(q.Get(apiChain)), addresses, dateParam(q.Get(apiStart), time.Time{}), dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1)), decisions)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	pending, body := assetReviewBody(rows, readiness, unmatched, decisions)
	w.Header().Set(apiContentType, apiJSONContentType)
	if pending > 0 {
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// assetReviewBody is the /asset-review answer and its pending count.
func assetReviewBody(rows []Row, readiness ReportReadiness, unmatched []string, decisions AssetDecisions) (int, map[string]any) {
	reviews := []Row{}
	for _, row := range rows {
		if row.AssetIdentity != nil {
			row.AssetIdentity = row.reviewIdentity()
			reviews = append(reviews, row)
		}
	}
	pending := readiness.PendingAssetReviews + readiness.PendingSemanticReviews
	semanticReviews := []Row{}
	for _, row := range rows {
		if semanticReviewReason(row) != "" {
			semanticReviews = append(semanticReviews, row)
		}
	}
	return pending, map[string]any{
		ReportJobPending:          pending,
		apiReadiness:              readiness,
		"semantic_reviews":        semanticReviews,
		"reviews":                 reviews,
		"groups":                  ReviewGroups(reviews, decisions),
		"decisions":               decisions,
		"unmatched_decision_keys": unmatched,
		"reason_glossary":         ReviewReasonGlossary(),
		"decision_keys": map[string]string{
			"record_id":           "one receipt; override carries quantity, value_usd, cost_basis_usd and acquired_date totals",
			"denom:<raw>":         "every flagged receipt of that token in the report; override carries token and unit_value_usd, each receipt is priced quantity times unit on its own date",
			DecisionKeySpamPolicy: "every receipt flagged as suspected spam without a more specific decision; exclude only (spam_policy=valueless is the same thing)",
			"semantic_record_id":  "one row of semantic_reviews, keyed by its SemanticRecordID; carries classification and no mode, with value_usd for an unpriced row and cost_basis_usd and acquired_date for a transfer_in. A flagged receipt's SemanticRecordID is its record_id, where classification rides beside the mode",
		},
		"classifications": ClassificationVocabulary(),
	}
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
		day := r.Time.UTC().Format(apiDateLayout)
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
