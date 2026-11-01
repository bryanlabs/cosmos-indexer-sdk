package taxapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

// Row is a normalized, priced taxable event from the queried address's point of
// view (Direction = in|out). Platform exporters map this to their columns.
type Row struct {
	Time      time.Time
	Category  string // transfer | reward | commission | ibc_in | ibc_out | fee
	Direction string // in | out
	Symbol    string
	Denom     string
	Amount    decimal.Decimal // display units (for nft_sale: the sale price in Denom)
	PriceUSD  decimal.Decimal // per unit at the event date (0 if unknown)
	ValueUSD  decimal.Decimal // Amount * PriceUSD
	From      string
	To        string
	TxHash    string
	Asset     string // non-fungible asset id "<collection>/<token_id>" for nft_sale
	IsIBC     bool   // true when Denom is an IBC trace path (Symbol is the resolved base)
	// Validator attribution is populated for staking rewards and auto-withdrawals.
	// It is carried in descriptions for import formats that support a note field.
	ValidatorAddress   string
	ValidatorMoniker   string
	RewardTrigger      string
	AssetIdentity      *AssetIdentity
	AssetDecision      *AssetDecision
	Excluded           bool
	ManualBasisUSD     *decimal.Decimal
	ManualAcquiredDate *time.Time
	// DecimalsAssumed is true when neither the oracle nor the chain's bank module
	// could resolve this denom, so decimals fell back to a bare guess (6). Amount,
	// PriceUSD and ValueUSD for this row may be wrong and should not be trusted
	// silently (see INF-200/201).
	DecimalsAssumed bool
	// PriceMissing is true when the oracle had no USD price for this denom/date.
	// PriceUSD and ValueUSD are then 0, but that 0 is NOT a confirmed value, it
	// means "unknown" (INF-201); consumers must not sum it as a real zero without
	// surfacing the gap.
	PriceMissing bool
	// PriceImplied is true when PriceUSD was derived from the other leg of the
	// same swap (same transaction) because the oracle had no price for this
	// asset: the user gave up X USD of ATOM, so the shares received are worth X.
	PriceImplied bool
	// Detail is the parser's protocol context JSON (contract, protocol name,
	// proposal id, Skip destination, ...). Empty for plain rows.
	Detail string
	// UserTag is the user's own classification of this transaction (see
	// tags.go). It never changes the on-chain record; it changes the label an
	// exporter emits and whether Form 8949 sees a disposal.
	UserTag string
}

// isTaxable mirrors tax.Category.Taxable; fees count as taxable spends.
// Record-only rows stay in the preview, Generic CSV and journal and are kept
// out of tax-software imports and Form 8949.
func (r Row) isTaxable() bool {
	return r.Category == apiFee || tax.Category(r.Category).Taxable()
}

// taxImportable reports whether a row belongs in a tax-software import.
// Record-only rows and rows the user tagged spam or protocol_deposit do not.
func (r Row) taxImportable() bool {
	if !r.isTaxable() {
		return false
	}
	switch r.UserTag {
	case TagSpam, TagProtocolDeposit:
		return false
	}
	return true
}

// description is the human/tax note; NFT sales name the asset, IBC rows carry the
// raw trace path so the UI can show an "IBC" badge with full detail on hover.
// Rows with an unresolved denom get an explicit warning suffix rather than
// silently reporting a possibly-wrong amount.
func (r Row) description() string {
	d := r.Category
	if (r.Category == apiNFTSale || r.Category == apiNFTMint) && r.Asset != "" {
		d = r.Category + " " + r.Asset
	} else if r.IsIBC {
		d = "ibc " + r.Denom
	}
	if r.ValidatorAddress != "" {
		d += apiStakingMetadata + stakingMetadataJSON(r)
	}
	if r.RewardTrigger != "" && r.Category != apiReward && r.Category != apiCommission {
		d += " | source=" + r.RewardTrigger
	}
	if r.Detail != "" {
		d += " | detail=" + r.Detail
	}
	if r.UserTag != "" {
		d += " | user_tag=" + r.UserTag
	}
	if r.PriceImplied {
		d += " (price implied from the other swap leg)"
	}
	if r.AssetIdentity != nil {
		metadata, _ := json.Marshal(r.reviewIdentity())
		d += " | asset_identity=" + string(metadata)
	}
	if r.DecimalsAssumed {
		d += " (decimals unknown, amount may be wrong)"
	}
	if r.PriceMissing {
		d += " (price missing)"
	}
	return d
}

// label maps our category to a human/tax label.
func (r Row) label() string {
	if l, ok := koinlyTagLabel(r); ok {
		return l
	}
	switch r.Category {
	case apiReward, apiCommission:
		return apiStaking
	case apiFee:
		return apiFee
	case apiNFTSale, apiNFTMint:
		return "nft"
	case apiSwap:
		return apiSwap
	case TagIncome:
		return TagIncome
	case TagExpense:
		return apiCost
	default:
		if r.Direction == "in" {
			return "receive"
		}
		return apiSend
	}
}

// WriteCSV writes rows in the requested platform format.
func WriteCSV(out io.Writer, format string, rows []Row) error {
	if readiness := readinessForRows(rows); readiness.PendingSemanticReviews > 0 {
		return reportReadinessRequired{readiness}
	}
	if pending := PendingAssetReviews(rows); pending > 0 {
		return assetReviewRequired{pending}
	}
	kept := make([]Row, 0, len(rows))
	for _, row := range rows {
		// Generic is an audit-capable CSV with explicit treatment and manual
		// value/basis fields. Preserve excluded receipts and record-only rows
		// there. Other import formats can auto-value unsupported assets or read
		// a record as a disposal, so they receive only importable rows.
		if format == apiGeneric || (!row.Excluded && row.taxImportable()) {
			kept = append(kept, row)
		}
	}
	return writeCSV(out, format, kept)
}

// Internal preview transport retains pending and user-excluded receipts. It is
// not a tax-software download; the tax export entrypoint always gates reviews.
func WritePreviewCSV(out io.Writer, format string, rows []Row) error {
	if format != apiCryptotaxcalculator && format != apiSumm {
		return fmt.Errorf("preview format must be cryptotaxcalculator or summ")
	}
	var buf bytes.Buffer
	if err := writeCSV(&buf, format, rows); err != nil {
		return err
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		return err
	}
	records[0] = append(records[0], "Asset Identity Metadata", "Staking Metadata", "Category", apiTaxable, apiPreviewDetail, "Price Implied", "User Tag", "Source")
	for i, row := range rows {
		records[i+1] = append(records[i+1], assetIdentityJSON(row.reviewIdentity()), stakingMetadataJSON(row), row.Category, previewTaxableText(row), row.Detail, boolStr(row.PriceImplied), row.UserTag, row.RewardTrigger)
	}
	return csv.NewWriter(out).WriteAll(records)
}

func writeCSV(out io.Writer, format string, rows []Row) error {
	w := csv.NewWriter(out)

	switch format {
	case apiKoinly:
		_ = w.Write([]string{apiHeaderDate, "Sent Amount", apiSentCurrency, "Received Amount", apiReceivedCurrency, apiFeeAmountHeader, apiFeeCurrencyHeader, "Net Worth Amount", "Net Worth Currency", "Label", apiDescription, apiHeaderTxHash})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05 UTC"),
				sentAmt, sentCur, recvAmt, recvCur,
				"", "", rowValueText(r), "USD", r.label(), r.description(), r.TxHash,
			})
		}

	case apiCointracker:
		// CoinTracker accepts exactly these eight columns and has no description or
		// metadata field. Validator attribution is therefore unavailable in this
		// export; use generic or a description-supporting format instead.
		_ = w.Write([]string{apiHeaderDate, "Received Quantity", apiReceivedCurrency, "Sent Quantity", apiSentCurrency, apiFeeAmountHeader, apiFeeCurrencyHeader, "Tag"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			tag := coinTrackerTag(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("01/02/2006 15:04:05"),
				recvAmt, recvCur, sentAmt, sentCur, "", "", tag,
			})
		}

	case apiCoinledger:
		// Universal Manual Import Template: dates must read Month-Day-Year, and
		// "Type" comes from CoinLedger's fixed vocabulary (Deposit/Withdrawal/
		// Staking/...), not a free-text label.
		_ = w.Write([]string{"Date (UTC)", "Platform", "Asset Sent", "Amount Sent", "Asset Received", "Amount Received", apiFeeCurrencyHeader, apiFeeAmountHeader, apiTypeHeader, apiDescription, apiHeaderTxHash})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			typ := coinLedgerType(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("01/02/2006 15:04:05"),
				"Cosmos", sentCur, sentAmt, recvCur, recvAmt, "", "", typ, r.description(), r.TxHash,
			})
		}

	case apiSumm, apiCryptotaxcalculator:
		// Richest format: carries our oracle reference price per unit.
		_ = w.Write([]string{apiTimestampUTC, apiTypeHeader, apiBaseCurrency, apiBaseAmount, apiQuoteCurrencyOptional, apiQuoteAmountOptional, apiFeeCurrencyOptional, apiFeeAmountOptional, apiFromOptional, apiToOptional, apiBlockchainOptional, apiIDOptional, apiDescriptionOptional, apiReferencePricePerUnitOptional, apiReferencePriceCurrencyOptional}) //nolint:lll
		for _, r := range rows {
			typ := ctcType(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05"),
				typ, r.Symbol, r.Amount.String(), "", "", "", "",
				r.From, r.To, "cosmos", r.TxHash, r.description(), rowPriceText(r), "USD",
			})
		}

	case apiCryptio:
		// Cryptio custom-CSV template (enterprise sub-ledger). Strict schema.
		_ = w.Write([]string{"transactionDate", "orderType", "txhash", "incomingAsset", "incomingVolume", "incomingUnitRate", "incomingTransactionValue", "outgoingAsset", "outgoingVolume", "outgoingUnitRate", "outgoingTransactionValue", "feeAsset", "feeVolume", "feeUnitRate", "feeTransactionValue", "otherParties", apiNote}) //nolint:lll
		for _, r := range rows {
			date := r.Time.UTC().Format("2006-01-02 15:04:05")
			amt, rate, val := r.Amount.String(), rowPriceText(r), rowValueText(r)
			if r.Direction == "in" && r.Category != apiFee {
				_ = w.Write([]string{date, "deposit", r.TxHash, r.Symbol, amt, rate, val, "", "", "", "", "", "", "", "", party(r), r.description()})
			} else {
				ot := "withdraw"
				if r.Category == apiFee {
					ot = apiFee
				}
				_ = w.Write([]string{date, ot, r.TxHash, "", "", "", "", r.Symbol, amt, rate, val, "", "", "", "", party(r), r.description()})
			}
		}

	case apiBitwave:
		// Bitwave enterprise accounting; core columns + a unique id per line.
		_ = w.Write([]string{"id", apiDate, apiType, apiAmount, "amountTicker", "txHash", "contactAddress", apiCategory})
		for i, r := range rows {
			typ := "Deposit"
			if r.Direction == apiOut {
				typ = "Withdrawal"
			}
			if r.Category == apiFee {
				typ = "Fee"
			}
			_ = w.Write([]string{
				fmt.Sprintf("%s-%d", r.TxHash, i), r.Time.UTC().Format(time.RFC3339),
				typ, r.Amount.String(), r.Symbol, r.TxHash, party(r), r.description(),
			})
		}

	case apiGeneric:
		// Universal, fully-typed enterprise CSV: every field, USD basis. Any tool
		// or accountant can map it; also the recommended import for Trace Finance.
		_ = w.Write([]string{apiDateUTC, apiTxHash, apiCategory, "direction", apiAsset, apiDenom, apiAmount, apiUnitPriceUSD, apiValueUSD, "from", "to", "nft_asset", apiChain, "decimals_assumed", apiPriceMissing, apiValidatorAddress, apiValidatorMoniker, apiRewardTrigger, "asset_identity", "user_treatment", "manual_value_usd", "manual_cost_basis_usd", "manual_acquired_date", "taxable", "price_implied", "user_tag", apiDetail}) //nolint:lll
		for _, r := range rows {
			_ = w.Write([]string{
				r.Time.UTC().Format(time.RFC3339), r.TxHash, r.Category, r.Direction,
				genericAssetDisplay(r), r.Denom, r.Amount.String(), rowPriceText(r), rowValueText(r),
				r.From, r.To, r.Asset, apiHubChainID, boolStr(r.DecimalsAssumed), boolStr(r.PriceMissing),
				r.ValidatorAddress, spreadsheetLabel(r.ValidatorMoniker), r.RewardTrigger, assetIdentityJSON(r.reviewIdentity()), reviewMode(r), manualField(r, apiValue), manualField(r, apiBasis), manualField(r, apiDate),
				boolStr(r.isTaxable()), boolStr(r.PriceImplied), r.UserTag, r.Detail,
			})
		}

	default: // fall through to koinly for unknown formats
		return WriteCSV(out, apiKoinly, rows)
	}
	w.Flush()
	return w.Error()
}

// genericAssetDisplay clarifies only explicitly excluded, identity-mismatched
// receipts in the Generic audit CSV. It never changes the source Row or its
// identity metadata, and user overrides retain their chosen Symbol verbatim.
func genericAssetDisplay(r Row) string {
	if r.AssetIdentity == nil || r.AssetDecision == nil || r.AssetDecision.Mode != AssetDecisionExclude {
		return r.Symbol
	}

	name := strings.TrimSpace(r.AssetIdentity.TokenName)
	if name != "" && (r.AssetIdentity.OfficialAtomMatch || (!strings.EqualFold(name, "atom") && !strings.EqualFold(name, apiUatom))) {
		return spreadsheetLabel(name)
	}

	denom := r.AssetIdentity.RawDenom
	if denom == "" {
		denom = r.Denom
	}
	return spreadsheetLabel(apiUnverifiedDenomPrefix + denom + ")")
}

func stakingMetadataJSON(r Row) string {
	if r.ValidatorAddress == "" {
		return ""
	}
	data, _ := json.Marshal(struct {
		ValidatorAddress string `json:"validator_address"`
		ValidatorMoniker string `json:"validator_moniker"`
		RewardTrigger    string `json:"reward_trigger"`
	}{r.ValidatorAddress, r.ValidatorMoniker, r.RewardTrigger})
	return string(data)
}

func (r Row) reviewIdentity() *AssetIdentity {
	if r.AssetIdentity == nil {
		return nil
	}
	identity := *r.AssetIdentity
	identity.Decision = r.AssetDecision
	identity.Excluded = r.Excluded
	identity.Treatment = apiUnreviewed
	if r.AssetDecision != nil {
		identity.Treatment = r.AssetDecision.Mode
		if r.AssetDecision.Mode == AssetDecisionExclude {
			identity.Note = "Explicitly excluded by the user with manual zero value and basis. The original quantity and source receipt remain in preview and generic audit records; exclusion keeps it out of tax calculations."
		} else {
			identity.Note = "Explicit user override of token, quantity, value, basis and acquisition date. These are user-supplied inputs, not verified on-chain identity or oracle prices."
		}
	}
	return &identity
}

func reviewMode(r Row) string {
	if r.AssetDecision == nil {
		return ""
	}
	return r.AssetDecision.Mode
}

func manualField(r Row, field string) string {
	if r.AssetDecision == nil {
		return ""
	}
	if r.AssetDecision.Mode == AssetDecisionExclude {
		if field == apiValue || field == apiBasis {
			return decimal.Zero.String()
		}
		return ""
	}
	if r.AssetDecision.Mode != AssetDecisionOverride {
		return ""
	}
	switch field {
	case apiValue:
		return r.AssetDecision.ValueUSD
	case apiBasis:
		return r.AssetDecision.CostBasisUSD
	case apiDate:
		return r.AssetDecision.AcquiredDate
	}
	return ""
}

func assetIdentityJSON(identity *AssetIdentity) string {
	if identity == nil {
		return ""
	}
	data, _ := json.Marshal(identity)
	return string(data)
}

// party returns the counterparty address for a row (the non-fee side).
func party(r Row) string {
	if r.Direction == "in" {
		return r.From
	}
	return r.To
}

func splitDir(r Row) (sentAmt, sentCur, recvAmt, recvCur string) {
	if r.Direction == apiOut {
		return r.Amount.String(), r.Symbol, "", ""
	}
	return "", "", r.Amount.String(), r.Symbol
}

func ctcType(r Row) string {
	if t, ok := ctcTagType(r); ok {
		return t
	}
	switch r.Category {
	case apiReward, apiCommission:
		return apiStaking
	case apiFee:
		return apiFee
	case TagIncome:
		return TagIncome
	case TagExpense:
		return TagExpense
	case TagProtocolDeposit, apiDelegate, "tokenize_shares", "gov_deposit":
		return "staking-deposit"
	case "protocol_withdrawal", "undelegate", "redeem_shares":
		return "staking-withdrawal"
	case "nft_transfer", "nft_burn", apiRedelegate, "cancel_unbonding", "share_record":
		if r.Direction == "in" {
			return "transfer-in"
		}
		return "transfer-out"
	case apiNFTSale, apiSwap:
		// Disposal leg = sell, acquisition leg = buy.
		if r.Direction == apiOut {
			return "sell"
		}
		return "buy"
	case apiNFTMint:
		return "buy" // acquisition
	default:
		if r.Direction == "in" {
			return "receive"
		}
		return apiSend
	}
}

func rowPriceText(r Row) string {
	if r.AssetDecision != nil && (r.AssetDecision.Mode == AssetDecisionOverride || r.AssetDecision.Mode == AssetDecisionExclude) {
		return r.PriceUSD.String()
	}
	return refPrice(r.PriceUSD)
}

func rowValueText(r Row) string {
	if r.AssetDecision != nil && (r.AssetDecision.Mode == AssetDecisionOverride || r.AssetDecision.Mode == AssetDecisionExclude) {
		return r.ValueUSD.String()
	}
	return usd(r.ValueUSD)
}

func usd(d decimal.Decimal) string {
	if d.IsZero() {
		return ""
	}
	return d.StringFixed(2)
}

func refPrice(d decimal.Decimal) string {
	if d.IsZero() {
		return ""
	}
	return d.String()
}

// spreadsheetLabel prevents an untrusted standalone text cell from being
// interpreted as a spreadsheet formula. Description JSON is deliberately not
// passed through this helper because it starts with the event category and must
// retain its exact structured metadata.
func spreadsheetLabel(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r', '\n':
		return "'" + s
	default:
		return s
	}
}

func boolStr(b bool) string {
	if b {
		return apiTrue
	}
	return apiFalse
}
