package taxapi

import (
	"encoding/csv"
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
	Amount    decimal.Decimal // display units
	PriceUSD  decimal.Decimal // per unit at the event date (0 if unknown)
	ValueUSD  decimal.Decimal // Amount * PriceUSD
	From      string
	To        string
	TxHash    string
}

// label maps our category to a human/tax label.
func (r Row) label() string {
	switch r.Category {
	case "reward", "commission":
		return "staking"
	case "fee":
		return "fee"
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
				"", "", usd(r.ValueUSD), "USD", r.label(), r.Category, r.TxHash,
			})
		}

	case "cointracker":
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
		_ = w.Write([]string{"Date (UTC)", "Platform", "Asset Sent", "Amount Sent", "Asset Received", "Amount Received", "Fee Currency", "Fee Amount", "Type", "Description", "TxHash"})
		for _, r := range rows {
			sentAmt, sentCur, recvAmt, recvCur := splitDir(r)
			typ := "Deposit"
			if r.Direction == "out" {
				typ = "Withdrawal"
			}
			if r.Category == "reward" || r.Category == "commission" {
				typ = "Staking Reward"
			}
			_ = w.Write([]string{
				r.Time.UTC().Format("2006-01-02 15:04:05"),
				"Cosmos", sentCur, sentAmt, recvCur, recvAmt, "", "", typ, r.Category, r.TxHash,
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
				r.From, r.To, "cosmos", r.TxHash, r.Category, refPrice(r.PriceUSD), "USD",
			})
		}

	default: // fall through to koinly for unknown formats
		return WriteCSV(out, "koinly", rows)
	}
	return w.Error()
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
