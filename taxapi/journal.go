package taxapi

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// JournalLine is one side of a double-entry journal entry in USD. Each row of
// the tax export becomes one balanced entry (two or more lines) so a controller
// can post wallet activity to a general ledger without re-deriving it from the
// tax CSV. Amounts are fair market value on the day; realized gains and losses
// on disposals are computed on Form 8949, not here, so the asset credit on a
// disposal is at fair market value and the clearing account absorbs it.
type JournalLine struct {
	Date     string `json:"date_utc"`
	TxHash   string `json:"tx_hash"`
	EntryID  string `json:"entry_id"`
	Account  string `json:"account"`
	Debit    string `json:"debit_usd"`
	Credit   string `json:"credit_usd"`
	Asset    string `json:"asset"`
	Quantity string `json:"quantity"`
	Category string `json:"category"`
	Memo     string `json:"memo"`
	// PriceUSD is the per-unit reference price applied to this line (empty
	// when no price was available), so a controller can re-derive the amount
	// or convert it to a reporting currency at their own rate.
	PriceUSD string `json:"price_usd"`
	// Counterparty is the other wallet or contract on a transfer (empty for
	// rewards, fees and other self-directed events).
	Counterparty string `json:"counterparty"`
	// Validator and ValidatorCountry are set on reward and commission lines:
	// the operator moniker and the ISO country code from the disclosed
	// jurisdiction registry ("undisclosed" when the operator has not
	// disclosed one).
	Validator        string `json:"validator"`
	ValidatorCountry string `json:"validator_country"`
	// Methodology is the methodology version the line was produced under.
	Methodology string `json:"methodology_version"`
}

// Chart of accounts used by the journal. Names are stable so a controller can
// map them once in their accounting system.
const (
	acctDigitalAssets   = "Digital Assets"
	acctStakedAssets    = "Digital Assets Staked"
	acctUnbondingAssets = "Digital Assets Unbonding"
	acctLiquidShares    = "Liquid Staking Tokens"
	acctInProtocol      = "Digital Assets in Protocol"
	acctGovDeposits     = "Governance Deposits Receivable"
	acctNFTAssets       = "Digital Collectibles"
	acctTransfersIn     = "Transfers In Clearing"
	acctTransfersOut    = "Transfers Out Clearing"
	acctInternal        = "Internal Transfers Clearing"
	acctStakingIncome   = "Income: Staking Rewards"
	acctCommission      = "Income: Validator Commission"
	acctProtocolIncome  = "Income: Protocol"
	acctOtherIncome     = "Income: Other"
	acctFees            = "Expense: Transaction Fees"
	acctProtocolFees    = "Expense: Protocol Fees"
	acctOtherExpense    = "Expense: Other"
	acctGifts           = "Gifts"
	acctLosses          = "Losses: Lost or Stolen"
	acctSwapDifference  = "Swap Valuation Difference"
	acctSpam            = "Spam Receipts (memo only)"
)

// BuildJournal turns priced rows into balanced journal entries.
func BuildJournal(input []Row) []JournalLine {
	// Fee rows are loaded after event rows; a ledger wants strict date order.
	rows := append([]Row(nil), input...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time.Before(rows[j].Time) })
	var out []JournalLine
	swapsByTx := map[string][]Row{}
	for _, r := range rows {
		if r.Category == "swap" {
			swapsByTx[r.TxHash] = append(swapsByTx[r.TxHash], r)
		}
	}
	swapDone := map[string]bool{}
	for i, r := range rows {
		if r.Excluded || (r.AssetIdentity != nil && r.AssetDecision == nil) {
			continue
		}
		id := fmt.Sprintf("%s-%d", shortHash(r.TxHash), i)
		date := r.Time.UTC().Format("2006-01-02")
		memo := r.Category
		if r.RewardTrigger != "" {
			memo += " " + r.RewardTrigger
		}
		if r.ValidatorMoniker != "" {
			memo += " via " + r.ValidatorMoniker
		}
		if p := protocolName(r.Detail); p != "" {
			memo += " (" + p + ")"
		}
		if r.UserTag != "" {
			memo += " tagged " + r.UserTag
		}
		if r.PriceMissing {
			memo += " [no USD price: quantity only]"
		}
		if r.PriceImplied {
			memo += " [price implied from the other swap leg]"
		}
		asset := acctDigitalAssets + ": " + r.Symbol
		val := r.ValueUSD
		counterparty := ""
		switch {
		case r.Category == "reward", r.Category == "commission", r.Category == "fee":
		case r.Direction == "in" && r.From != "":
			counterparty = r.From
		case r.Direction == "out" && r.To != "":
			counterparty = r.To
		}
		validator, validatorCountry := "", ""
		if r.ValidatorAddress != "" || r.ValidatorMoniker != "" {
			validator = r.ValidatorMoniker
			if validator == "" {
				validator = r.ValidatorAddress
			}
			validatorCountry = jurisdictionLabel(r.ValidatorMoniker)
		}
		price := ""
		if !r.PriceMissing && !r.Excluded {
			price = r.PriceUSD.String()
		}
		line := func(account string, debit bool) JournalLine {
			l := JournalLine{Date: date, TxHash: r.TxHash, EntryID: id, Account: account, Asset: r.Symbol, Quantity: r.Amount.String(), Category: r.Category, Memo: memo,
				PriceUSD: price, Counterparty: counterparty, Validator: validator, ValidatorCountry: validatorCountry, Methodology: MethodologyVersion}
			if !r.PriceMissing {
				if debit {
					l.Debit = val.StringFixed(2)
				} else {
					l.Credit = val.StringFixed(2)
				}
			}
			return l
		}
		in := r.Direction == "in"
		switch {
		case r.UserTag == TagSpam:
			out = append(out, JournalLine{Date: date, TxHash: r.TxHash, EntryID: id, Account: acctSpam, Asset: r.Symbol, Quantity: r.Amount.String(), Category: r.Category, Memo: memo, Counterparty: counterparty, Methodology: MethodologyVersion})
			continue
		case r.Category == "fee":
			out = append(out, line(acctFees, true), line(asset, false))
		case r.Category == "reward":
			out = append(out, line(asset, true), line(acctStakingIncome, false))
		case r.Category == "commission":
			out = append(out, line(asset, true), line(acctCommission, false))
		case r.Category == "income":
			out = append(out, line(asset, true), line(acctProtocolIncome+": "+titleTrigger(r.RewardTrigger), false))
		case r.Category == "expense":
			out = append(out, line(acctProtocolFees+": "+titleTrigger(r.RewardTrigger), true), line(asset, false))
		case r.Category == "swap":
			if swapDone[r.TxHash] {
				continue
			}
			swapDone[r.TxHash] = true
			legs := swapsByTx[r.TxHash]
			var debits, credits decimal.Decimal
			for _, leg := range legs {
				lr := leg
				legLine := JournalLine{Date: date, TxHash: lr.TxHash, EntryID: id, Account: acctDigitalAssets + ": " + lr.Symbol, Asset: lr.Symbol, Quantity: lr.Amount.String(), Category: "swap", Memo: memo}
				if !lr.PriceMissing {
					if lr.Direction == "in" {
						legLine.Debit = lr.ValueUSD.StringFixed(2)
						debits = debits.Add(lr.ValueUSD)
					} else {
						legLine.Credit = lr.ValueUSD.StringFixed(2)
						credits = credits.Add(lr.ValueUSD)
					}
				}
				out = append(out, legLine)
			}
			if diff := debits.Sub(credits); !diff.IsZero() {
				plug := JournalLine{Date: date, TxHash: r.TxHash, EntryID: id, Account: acctSwapDifference, Category: "swap", Memo: memo + " (legs priced independently)"}
				if diff.IsPositive() {
					plug.Credit = diff.StringFixed(2)
				} else {
					plug.Debit = diff.Abs().StringFixed(2)
				}
				out = append(out, plug)
			}
		case r.Category == "nft_sale":
			nft := acctNFTAssets + ": " + r.Asset
			if in {
				out = append(out, line(nft, true), line(asset, false))
			} else {
				out = append(out, line(asset, true), line(nft, false))
			}
		case r.Category == "nft_mint":
			out = append(out, line(acctNFTAssets+": "+r.Asset, true), line(asset, false))
		case r.Category == "nft_transfer", r.Category == "nft_burn", r.Category == "share_record", r.Category == "redelegate":
			out = append(out, JournalLine{Date: date, TxHash: r.TxHash, EntryID: id, Account: acctNFTAssets, Asset: r.Asset, Quantity: r.Amount.String(), Category: r.Category, Memo: memo + " (memo entry, no USD movement)"})
		case r.Category == "delegate", r.Category == "cancel_unbonding":
			out = append(out, line(acctStakedAssets+": "+r.Symbol, true), line(asset, false))
		case r.Category == "undelegate":
			out = append(out, line(acctUnbondingAssets+": "+r.Symbol, true), line(acctStakedAssets+": "+r.Symbol, false))
		case r.Category == "tokenize_shares":
			out = append(out, line(acctLiquidShares+": "+r.Symbol, true), line(acctStakedAssets+": "+r.Symbol, false))
		case r.Category == "redeem_shares":
			out = append(out, line(acctStakedAssets+": "+r.Symbol, true), line(acctLiquidShares+": "+r.Symbol, false))
		case r.Category == "gov_deposit":
			out = append(out, line(acctGovDeposits, true), line(asset, false))
		case r.Category == "protocol_deposit" || r.UserTag == TagProtocolDeposit:
			if in {
				out = append(out, line(asset, true), line(acctInProtocol+": "+r.Symbol, false))
			} else {
				out = append(out, line(acctInProtocol+": "+r.Symbol, true), line(asset, false))
			}
		case r.Category == "protocol_withdrawal":
			out = append(out, line(asset, true), line(acctInProtocol+": "+r.Symbol, false))
		default: // transfer, ibc_in, ibc_out and tagged variants
			contra := acctTransfersOut
			if in {
				contra = acctTransfersIn
			}
			switch r.UserTag {
			case TagInternalTransfer:
				contra = acctInternal
			case TagIncome, TagAirdrop:
				contra = acctOtherIncome + ": " + titleTrigger(r.UserTag)
			case TagPayment:
				if in {
					contra = acctOtherIncome + ": Payments Received"
				} else {
					contra = acctOtherExpense + ": Payments"
				}
			case TagExpense:
				contra = acctOtherExpense
			case TagGift:
				contra = acctGifts
			case TagLost:
				contra = acctLosses
			}
			if in {
				out = append(out, line(asset, true), line(contra, false))
			} else {
				out = append(out, line(contra, true), line(asset, false))
			}
		}
	}
	return out
}

func titleTrigger(t string) string {
	if t == "" {
		return "Unspecified"
	}
	words := strings.Split(t, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func shortHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// WriteJournalCSV writes journal lines with a trial-balance footer.
func WriteJournalCSV(out io.Writer, lines []JournalLine) error {
	w := csv.NewWriter(out)
	_ = w.Write([]string{"date_utc", "tx_hash", "entry_id", "account", "debit_usd", "credit_usd", "asset", "quantity", "price_usd", "category", "memo", "counterparty", "validator", "validator_country", "methodology_version"})
	var debits, credits decimal.Decimal
	for _, l := range lines {
		_ = w.Write([]string{l.Date, l.TxHash, l.EntryID, l.Account, l.Debit, l.Credit, spreadsheetLabel(l.Asset), l.Quantity, l.PriceUSD, l.Category, l.Memo, l.Counterparty, l.Validator, l.ValidatorCountry, l.Methodology})
		if d, err := decimal.NewFromString(l.Debit); err == nil {
			debits = debits.Add(d)
		}
		if c, err := decimal.NewFromString(l.Credit); err == nil {
			credits = credits.Add(c)
		}
	}
	_ = w.Write([]string{"", "", "TOTAL", "", debits.StringFixed(2), credits.StringFixed(2), "", "", "", "", "trial balance: debits equal credits", "", "", "", MethodologyVersion})
	w.Flush()
	return w.Error()
}

// GET /journal?address=&chain=&start=&end=&format=csv|json
func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := strings.TrimSpace(q.Get("address"))
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get("chain"))
	rows, err := s.rowsForRequest(r, chain, []string{addr}, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		writeTaxError(w, err)
		return
	}
	lines := BuildJournal(rows)
	if q.Get("format") == "json" {
		writeJSON(w, map[string]any{"address": addr, "chain": chain, "methodology_version": MethodologyVersion, "lines": lines})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=gl-journal.csv")
	_ = WriteJournalCSV(w, lines)
}
