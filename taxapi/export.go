package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"time"

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
	ValidatorAddress string
	ValidatorMoniker string
	RewardTrigger    string
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
}

// description is the human/tax note; NFT sales name the asset, IBC rows carry the
// raw trace path so the UI can show an "IBC" badge with full detail on hover.
// Rows with an unresolved denom get an explicit warning suffix rather than
// silently reporting a possibly-wrong amount.
func (r Row) description() string {
	d := r.Category
	if (r.Category == "nft_sale" || r.Category == "nft_mint") && r.Asset != "" {
		d = r.Category + " " + r.Asset
	} else if r.IsIBC {
		d = "ibc " + r.Denom
	}
	if r.ValidatorAddress != "" {
		metadata, _ := json.Marshal(struct {
			ValidatorAddress string `json:"validator_address"`
			ValidatorMoniker string `json:"validator_moniker"`
			RewardTrigger    string `json:"reward_trigger"`
		}{r.ValidatorAddress, r.ValidatorMoniker, r.RewardTrigger})
		d += " | staking_metadata=" + string(metadata)
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
	switch r.Category {
	case "reward", "commission":
		return "staking"
	case "fee":
		return "fee"
	case "nft_sale", "nft_mint":
		return "nft"
	case "swap":
		return "swap"
	default:
		if r.Direction == "in" {
			return "receive"
		}
		return "send"
	}
}

// WriteCSV writes rows in the requested platform format.
func WriteCSV(out io.Writer, format string, rows []Row) error {
	w := csv.NewWriter(out)
	defer w.Flush()

	switch format {
	case "koinly":
		_ = w.Write([]string{"Date", "Sent Amount", "Sent Currency", "Received Amount", "Received Currency", "Fee Amount", "Fee Currency", "Net Worth Amount", "Net Worth Currency", "Label", "Description", "TxHash"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05 UTC"),
				sentAmt, sentCur, recvAmt, recvCur,
				"", "", usd(r.ValueUSD), "USD", r.label(), r.description(), r.TxHash,
			})
		}

	case "cointracker":
		// CoinTracker accepts exactly these eight columns and has no description or
		// metadata field. Validator attribution is therefore unavailable in this
		// export; use generic or a description-supporting format instead.
		_ = w.Write([]string{"Date", "Received Quantity", "Received Currency", "Sent Quantity", "Sent Currency", "Fee Amount", "Fee Currency", "Tag"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			tag := ""
			if r.Category == "reward" || r.Category == "commission" {
				tag = "staked"
			}
			_ = w.Write([]string{
				r.Time.UTC().Format("01/02/2006 15:04:05"),
				recvAmt, recvCur, sentAmt, sentCur, "", "", tag,
			})
		}

	case "coinledger":
		// Universal Manual Import Template: dates must read Month-Day-Year, and
		// "Type" comes from CoinLedger's fixed vocabulary (Deposit/Withdrawal/
		// Staking/...), not a free-text label.
		_ = w.Write([]string{"Date (UTC)", "Platform", "Asset Sent", "Amount Sent", "Asset Received", "Amount Received", "Fee Currency", "Fee Amount", "Type", "Description", "TxHash"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			typ := "Deposit"
			if r.Direction == "out" {
				typ = "Withdrawal"
			}
			if r.Category == "reward" || r.Category == "commission" {
				typ = "Staking"
			}
			_ = w.Write([]string{
				r.Time.UTC().Format("01/02/2006 15:04:05"),
				"Cosmos", sentCur, sentAmt, recvCur, recvAmt, "", "", typ, r.description(), r.TxHash,
			})
		}

	case "summ", "cryptotaxcalculator":
		// Richest format: carries our oracle reference price per unit.
		_ = w.Write([]string{"Timestamp (UTC)", "Type", "Base Currency", "Base Amount", "Quote Currency (Optional)", "Quote Amount (Optional)", "Fee Currency (Optional)", "Fee Amount (Optional)", "From (Optional)", "To (Optional)", "Blockchain (Optional)", "ID (Optional)", "Description (Optional)", "Reference Price Per Unit (Optional)", "Reference Price Currency (Optional)"}) //nolint:lll
		for _, r := range rows {
			typ := ctcType(r)
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05"),
				typ, r.Symbol, r.Amount.String(), "", "", "", "",
				r.From, r.To, "cosmos", r.TxHash, r.description(), refPrice(r.PriceUSD), "USD",
			})
		}

	case "cryptio":
		// Cryptio custom-CSV template (enterprise sub-ledger). Strict schema.
		_ = w.Write([]string{"transactionDate", "orderType", "txhash", "incomingAsset", "incomingVolume", "incomingUnitRate", "incomingTransactionValue", "outgoingAsset", "outgoingVolume", "outgoingUnitRate", "outgoingTransactionValue", "feeAsset", "feeVolume", "feeUnitRate", "feeTransactionValue", "otherParties", "note"}) //nolint:lll
		for _, r := range rows {
			date := r.Time.UTC().Format("2006-01-02 15:04:05")
			amt, rate, val := r.Amount.String(), refPrice(r.PriceUSD), usd(r.ValueUSD)
			if r.Direction == "in" && r.Category != "fee" {
				_ = w.Write([]string{date, "deposit", r.TxHash, r.Symbol, amt, rate, val, "", "", "", "", "", "", "", "", party(r), r.description()})
			} else {
				ot := "withdraw"
				if r.Category == "fee" {
					ot = "fee"
				}
				_ = w.Write([]string{date, ot, r.TxHash, "", "", "", "", r.Symbol, amt, rate, val, "", "", "", "", party(r), r.description()})
			}
		}

	case "bitwave":
		// Bitwave enterprise accounting; core columns + a unique id per line.
		_ = w.Write([]string{"id", "date", "type", "amount", "amountTicker", "txHash", "contactAddress", "category"})
		for i, r := range rows {
			typ := "Deposit"
			if r.Direction == "out" {
				typ = "Withdrawal"
			}
			if r.Category == "fee" {
				typ = "Fee"
			}
			_ = w.Write([]string{
				fmt.Sprintf("%s-%d", r.TxHash, i), r.Time.UTC().Format(time.RFC3339),
				typ, r.Amount.String(), r.Symbol, r.TxHash, party(r), r.description(),
			})
		}

	case "generic":
		// Universal, fully-typed enterprise CSV: every field, USD basis. Any tool
		// or accountant can map it; also the recommended import for Trace Finance.
		_ = w.Write([]string{"date_utc", "tx_hash", "category", "direction", "asset", "denom", "amount", "unit_price_usd", "value_usd", "from", "to", "nft_asset", "chain", "decimals_assumed", "price_missing", "validator_address", "validator_moniker", "reward_trigger"}) //nolint:lll
		for _, r := range rows {
			_ = w.Write([]string{
				r.Time.UTC().Format(time.RFC3339), r.TxHash, r.Category, r.Direction,
				r.Symbol, r.Denom, r.Amount.String(), refPrice(r.PriceUSD), usd(r.ValueUSD),
				r.From, r.To, r.Asset, "cosmoshub-4", boolStr(r.DecimalsAssumed), boolStr(r.PriceMissing),
				r.ValidatorAddress, spreadsheetLabel(r.ValidatorMoniker), r.RewardTrigger,
			})
		}

	default: // fall through to koinly for unknown formats
		return WriteCSV(out, "koinly", rows)
	}
	return w.Error()
}

// party returns the counterparty address for a row (the non-fee side).
func party(r Row) string {
	if r.Direction == "in" {
		return r.From
	}
	return r.To
}

func splitDir(r Row) (sentAmt, sentCur, recvAmt, recvCur string) {
	if r.Direction == "out" {
		return r.Amount.String(), r.Symbol, "", ""
	}
	return "", "", r.Amount.String(), r.Symbol
}

func ctcType(r Row) string {
	switch r.Category {
	case "reward", "commission":
		return "staking"
	case "fee":
		return "fee"
	case "nft_sale", "swap":
		// Disposal leg = sell, acquisition leg = buy.
		if r.Direction == "out" {
			return "sell"
		}
		return "buy"
	case "nft_mint":
		return "buy" // acquisition
	default:
		if r.Direction == "in" {
			return "receive"
		}
		return "send"
	}
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
		return "true"
	}
	return "false"
}
