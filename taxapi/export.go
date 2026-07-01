package taxapi

import (
	"encoding/csv"
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
}

// description is the human/tax note; NFT sales name the asset, IBC rows carry the
// raw trace path so the UI can show an "IBC" badge with full detail on hover.
func (r Row) description() string {
	if (r.Category == categoryNFTSale || r.Category == categoryNFTMint) && r.Asset != "" {
		return r.Category + " " + r.Asset
	}
	if r.IsIBC {
		return "ibc " + r.Denom
	}
	return r.Category
}

// label maps our category to a human/tax label.
func (r Row) label() string {
	switch r.Category {
	case categoryReward, categoryCommission:
		return "staking"
	case categoryFee:
		return categoryFee
	case categoryNFTSale, categoryNFTMint:
		return "nft"
	case categorySwap:
		return categorySwap
	default:
		if r.Direction == directionIn {
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
		_ = w.Write([]string{"Date", "Received Quantity", "Received Currency", "Sent Quantity", "Sent Currency", "Fee Amount", "Fee Currency", "Tag"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			tag := ""
			if r.Category == categoryReward || r.Category == categoryCommission {
				tag = "staked"
			}
			_ = w.Write([]string{
				r.Time.UTC().Format("01/02/2006 15:04:05"),
				recvAmt, recvCur, sentAmt, sentCur, "", "", tag,
			})
		}

	case "coinledger":
		_ = w.Write([]string{"Date (UTC)", "Platform", "Asset Sent", "Amount Sent", "Asset Received", "Amount Received", "Fee Currency", "Fee Amount", "Type", "Description", "TxHash"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			typ := "Deposit"
			if r.Direction == directionOut {
				typ = "Withdrawal"
			}
			if r.Category == categoryReward || r.Category == categoryCommission {
				typ = "Staking Reward"
			}
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05"),
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
			if r.Direction == directionIn && r.Category != categoryFee {
				_ = w.Write([]string{date, "deposit", r.TxHash, r.Symbol, amt, rate, val, "", "", "", "", "", "", "", "", party(r), r.description()})
			} else {
				ot := "withdraw"
				if r.Category == categoryFee {
					ot = categoryFee
				}
				_ = w.Write([]string{date, ot, r.TxHash, "", "", "", "", r.Symbol, amt, rate, val, "", "", "", "", party(r), r.description()})
			}
		}

	case "bitwave":
		// Bitwave enterprise accounting; core columns + a unique id per line.
		_ = w.Write([]string{"id", "date", "type", "amount", "amountTicker", "txHash", "contactAddress", "category"})
		for i, r := range rows {
			typ := "Deposit"
			if r.Direction == directionOut {
				typ = "Withdrawal"
			}
			if r.Category == categoryFee {
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
		_ = w.Write([]string{"date_utc", "tx_hash", "category", "direction", "asset", "denom", "amount", "unit_price_usd", "value_usd", "from", "to", "nft_asset", "chain"}) //nolint:lll
		for _, r := range rows {
			_ = w.Write([]string{
				r.Time.UTC().Format(time.RFC3339), r.TxHash, r.Category, r.Direction,
				r.Symbol, r.Denom, r.Amount.String(), refPrice(r.PriceUSD), usd(r.ValueUSD),
				r.From, r.To, r.Asset, "cosmoshub-4",
			})
		}

	default: // fall through to koinly for unknown formats
		return WriteCSV(out, "koinly", rows)
	}
	return w.Error()
}

// party returns the counterparty address for a row (the non-fee side).
func party(r Row) string {
	if r.Direction == directionIn {
		return r.From
	}
	return r.To
}

func splitDir(r Row) (sentAmt, sentCur, recvAmt, recvCur string) {
	if r.Direction == directionOut {
		return r.Amount.String(), r.Symbol, "", ""
	}
	return "", "", r.Amount.String(), r.Symbol
}

func ctcType(r Row) string {
	switch r.Category {
	case categoryReward, categoryCommission:
		return "staking"
	case categoryFee:
		return categoryFee
	case categoryNFTSale, categorySwap:
		// Disposal leg = sell, acquisition leg = buy.
		if r.Direction == directionOut {
			return "sell"
		}
		return "buy"
	case categoryNFTMint:
		return "buy" // acquisition
	default:
		if r.Direction == directionIn {
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
