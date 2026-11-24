package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

// A semantic decision settles a row the parser could not classify (a
// review_required detail, such as an unnamed contract movement, an unknown
// claim, a block residual or a gov deposit outcome). It rides in the asset
// decisions map, keyed by the row's semantic record ID:
//
//	<chain>|<wallet>|<message_id>|<sub_index>:semantic:<fingerprint>[#<n>]
//
// The fingerprint binds the decision to the row's hash, denomination, amount,
// time, category, direction, counterparties, trigger, detail and review
// reason, so a replay that changes any of them makes the decision stale and
// it is refused, as a stale record ID is. #<n> numbers the second and later
// rows of one wallet whose facts are identical.
//
// A flagged receipt that also waits on classification is classified on its
// asset record ID, beside its exclude or override mode; Row.SemanticRecordID
// names whichever key the row takes.
//
// Two reasons are never settled this way: an IBC refund waits on basis
// reconciliation, and a withdraw-address move with no message cannot name the
// reward lot it moves. Both stay pending whatever is sent.
const (
	vocabClassification = "classification"
	vocabDirection      = "direction"
)

const (
	semanticRecordMarker = ":semantic:"
	decisionKindSemantic = "semantic"
)

// The classifications a person can give a row, with the directions each
// allows. The treatment is the ordinary one for the category it books.
const (
	ClassIncome             = "income"              // in: ordinary income at receipt value, which is its basis
	ClassPayment            = "payment"             // in: income at receipt; out: disposal at fair market value
	ClassTransferIn         = "transfer_in"         // in: a plain receipt; basis unknown unless cost_basis_usd is given
	ClassTransferOut        = "transfer_out"        // out: a plain send, a disposal unless tagged internal_transfer
	ClassSwap               = "swap"                // either: a trade leg (out disposal, in acquisition at value)
	ClassExpense            = "expense"             // out: a disposal labelled as a cost
	ClassProtocolDeposit    = "protocol_deposit"    // out: still owned, held by a protocol
	ClassProtocolWithdrawal = "protocol_withdrawal" // in: the user's own coins coming back from a protocol
	ClassExclude            = "exclude"             // either: no tax effect; kept only in the Generic CSV
)

var classDirections = map[string]string{
	ClassIncome:             "in",
	ClassPayment:            "",
	ClassTransferIn:         "in",
	ClassTransferOut:        apiOut,
	ClassSwap:               "",
	ClassExpense:            apiOut,
	ClassProtocolDeposit:    apiOut,
	ClassProtocolWithdrawal: "in",
	ClassExclude:            "",
}

// ClassificationVocabulary lists each classification with the direction it
// allows ("" for either) and its treatment, for the review screen.
func ClassificationVocabulary() []map[string]string {
	return []map[string]string{
		{vocabClassification: ClassIncome, vocabDirection: "in", apiTreatment: "Ordinary income at the USD value on receipt; that value is the cost basis."},
		{vocabClassification: ClassPayment, vocabDirection: "", apiTreatment: "Received: income at receipt, and that value is the cost basis. Sent: a disposal at fair market value."},
		{vocabClassification: ClassTransferIn, vocabDirection: "in", apiTreatment: "A plain receipt. Its basis is unknown unless cost_basis_usd (and acquired_date) is given."},
		{vocabClassification: ClassTransferOut, vocabDirection: apiOut, apiTreatment: "A plain send: a disposal at fair market value unless it is also tagged internal_transfer."},
		{vocabClassification: ClassSwap, vocabDirection: "", apiTreatment: "A trade leg: sent coins are a disposal, received coins an acquisition at their value."},
		{vocabClassification: ClassExpense, vocabDirection: apiOut, apiTreatment: "A disposal at fair market value, labelled as a cost or expense."},
		{vocabClassification: ClassProtocolDeposit, vocabDirection: apiOut, apiTreatment: "Still owned, held by a protocol: not a disposal."},
		{vocabClassification: ClassProtocolWithdrawal, vocabDirection: "in", apiTreatment: "The user's own coins coming back from a protocol: not an acquisition."},
		{vocabClassification: ClassExclude, vocabDirection: "", apiTreatment: "No tax effect. Left out of totals, tax-software imports, Form 8949 and the journal; kept in the Generic CSV."},
	}
}

func validateClassification(class string) error {
	if _, ok := classDirections[class]; !ok {
		return fmt.Errorf("classification must be one of income, payment, transfer_in, transfer_out, swap, expense, protocol_deposit, protocol_withdrawal or exclude")
	}
	return nil
}

// normalizeSemanticDecision validates a decision on a semantic record ID: a
// classification, with value_usd for a row the reference sources left
// unpriced, and cost_basis_usd and acquired_date for a transfer in. It
// carries no mode: classification exclude is how a row is left out.
func normalizeSemanticDecision(decision AssetDecision) (AssetDecision, error) {
	if decision.Mode != "" {
		return AssetDecision{}, fmt.Errorf("a semantic record ID takes classification and no mode; classification %q leaves the row out", ClassExclude)
	}
	if decision.Classification == "" {
		return AssetDecision{}, fmt.Errorf("a semantic record ID needs a classification")
	}
	if err := validateClassification(decision.Classification); err != nil {
		return AssetDecision{}, err
	}
	if decision.Token != "" || decision.Quantity != "" || decision.UnitValueUSD != "" {
		return AssetDecision{}, fmt.Errorf("a classification keeps the row's token and quantity; it takes value_usd, and cost_basis_usd and acquired_date for a transfer in")
	}
	if decision.Classification == ClassExclude && (decision.ValueUSD != "" || decision.CostBasisUSD != "" || decision.AcquiredDate != "") {
		return AssetDecision{}, fmt.Errorf("classification exclude carries no value or basis")
	}
	if decision.ValueUSD != "" {
		if err := validateDecimal(decision.ValueUSD, false); err != nil {
			return AssetDecision{}, fmt.Errorf("value_usd: %w", err)
		}
	}
	if decision.CostBasisUSD != "" || decision.AcquiredDate != "" {
		if decision.Classification != ClassTransferIn {
			return AssetDecision{}, fmt.Errorf("cost_basis_usd and acquired_date belong on a transfer_in; every other classification takes its basis from the methodology")
		}
		if err := validateDecimal(decision.CostBasisUSD, false); err != nil {
			return AssetDecision{}, fmt.Errorf("cost_basis_usd: %w", err)
		}
		if decision.AcquiredDate != "" {
			if err := validateDate(decision.AcquiredDate); err != nil {
				return AssetDecision{}, fmt.Errorf("acquired_date: %w", err)
			}
		}
	}
	return AssetDecision{Classification: decision.Classification, ValueUSD: decision.ValueUSD, CostBasisUSD: decision.CostBasisUSD, AcquiredDate: decision.AcquiredDate, AppliedFrom: decision.AppliedFrom}, nil
}

// semanticClassifiable reports whether a person's classification can settle
// a review reason. Neither IBC refund basis nor an unkeyed withdraw move is a
// classification question.
func semanticClassifiable(reason string) bool {
	return reason != "" && reason != ibcRefundBasisRequired && reason != withdrawMoveUnkeyed
}

// semanticRecordID is the key that classifies a row of one wallet.
func semanticRecordID(chain, wallet string, event tax.TaxableEvent, row Row, reason string) string {
	facts := []any{event.TxHash, event.Denom, event.Amount, event.Timestamp.UTC().Format(time.RFC3339Nano), event.Category, row.Direction, event.FromAddr, event.ToAddr, event.RewardTrigger, event.Detail, reason}
	data, _ := json.Marshal(facts)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%s|%s|%d|%d%s%x", chain, wallet, event.MessageID, event.SubIndex, semanticRecordMarker, digest[:])
}

// assignSemanticRecordID names the key a person classifies the row with, when
// it waits on a classifiable review: the flagged receipt's own record ID, or
// a semantic record ID. seen numbers identical rows of the same wallet.
func assignSemanticRecordID(row *Row, event tax.TaxableEvent, chain, wallet string, seen map[string]int) {
	reason := rawSemanticReviewReason(*row)
	// A withdraw-address row moves a reward lot by message; a classification
	// that changed its category would book the reward twice or not at all.
	if !semanticClassifiable(reason) || row.RewardTrigger == tax.TriggerWithdrawAddress {
		return
	}
	if row.AssetIdentity != nil {
		row.SemanticRecordID = row.AssetIdentity.RecordID
		return
	}
	id := semanticRecordID(chain, wallet, event, *row, reason)
	seen[id]++
	if n := seen[id]; n > 1 {
		id += rowKeySep + strconv.Itoa(n)
	}
	row.SemanticRecordID = id
}

// applySemanticDecisionsTo books each classified row as the person said. The
// row keeps its token, quantity, time, counterparties and detail;
// ClassifiedFrom keeps the parser's category and SemanticDecision the
// decision, so every export can show both. It marks each key it applied in
// used and in classified the keys whose classification settled a row. seen
// holds the semantic record IDs met so far, across every window of a report
// read in windows.
func applySemanticDecisionsTo(rows []Row, decisions AssetDecisions, used, seen, classified map[string]bool) error {
	for i := range rows {
		row := &rows[i]
		key := row.SemanticRecordID
		if key == "" || !semanticClassifiable(rawSemanticReviewReason(*row)) {
			continue
		}
		semanticKey := decisionKeyKind(key) == decisionKindSemantic
		if semanticKey {
			if seen[key] {
				return fmt.Errorf("duplicate semantic record ID")
			}
			seen[key] = true
		}
		raw, ok := decisions[key]
		if !ok || raw.Classification == "" {
			continue
		}
		decision, err := NormalizeKeyedDecision(key, raw)
		if err != nil {
			return fmt.Errorf(apiAssetDecisionErrorFormat, key, err)
		}
		if want := classDirections[decision.Classification]; want != "" && want != row.Direction {
			return fmt.Errorf("asset decision %q: classification %s needs an %s row, and this row is %s", key, decision.Classification, want, row.Direction)
		}
		used[key] = true
		classified[key] = true
		if semanticKey {
			if err := applySemanticValue(row, &decision, key); err != nil {
				return fmt.Errorf(apiAssetDecisionErrorFormat, key, err)
			}
		}
		classify(row, decision.Classification)
		row.SemanticDecision = &decision
	}
	return nil
}

// applySemanticValue applies a semantic decision's value and basis. value_usd
// fills only a missing price: a reference price stands, and the applied
// decision then carries no value_usd, so the audit trail shows the value the
// row was booked at. A flagged receipt is valued through its identity
// decision, never here.
func applySemanticValue(row *Row, decision *AssetDecision, key string) error {
	if decision.Classification == ClassExclude {
		zero := decimal.Zero
		row.PriceUSD = zero
		row.ValueUSD = zero
		row.ManualBasisUSD = &zero
		row.PriceMissing = false
		row.Excluded = true
		return nil
	}
	if decision.ValueUSD != "" {
		if row.PriceMissing && !row.Excluded && row.Amount.IsPositive() {
			value := decimal.RequireFromString(decision.ValueUSD)
			row.ValueUSD = value
			row.PriceUSD = value.DivRound(row.Amount, 18)
			row.PriceMissing = false
			row.PriceUserSupplied = true
			row.UserPriceKey = key
		} else {
			decision.ValueUSD = ""
		}
	}
	if decision.CostBasisUSD != "" {
		acquired := row.Time.UTC()
		if decision.AcquiredDate != "" {
			acquired, _ = time.Parse(apiDateLayout, decision.AcquiredDate)
		}
		if acquired.After(row.Time.UTC()) {
			return fmt.Errorf("acquisition date cannot be after the receipt date")
		}
		basis := decimal.RequireFromString(decision.CostBasisUSD)
		row.ManualBasisUSD = &basis
		row.ManualAcquiredDate = &acquired
	}
	return nil
}

// classify sets the category the classification books. A payment keeps the
// transfer category with the payment tag, the treatment the tag vocabulary
// already gives; a later user tag on the row still wins, as for any row.
func classify(row *Row, class string) {
	row.ClassifiedFrom = row.Category
	switch class {
	case ClassIncome:
		row.Category = string(tax.CategoryIncome)
	case ClassPayment:
		row.Category = apiTransfer
		row.UserTag = TagPayment
	case ClassTransferIn, ClassTransferOut:
		row.Category = apiTransfer
	case ClassSwap:
		row.Category = apiSwap
	case ClassExpense:
		row.Category = string(tax.CategoryExpense)
	case ClassProtocolDeposit:
		row.Category = string(tax.CategoryProtocolDeposit)
	case ClassProtocolWithdrawal:
		row.Category = string(tax.CategoryProtocolWithdrawal)
	}
}
