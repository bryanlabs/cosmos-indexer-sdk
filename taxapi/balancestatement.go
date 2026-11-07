package taxapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// maxStatementDates bounds one statement request; each date shares one
// anchor and one event load, but a wallet's whole window is still read.
const maxStatementDates = 12

// historicalBalanceJSON keeps the legacy /balance keys, as numbers in display
// units of the chain's own asset, and adds the rebuilt holdings. reward is
// null: unclaimed rewards on a past date are not rebuilt (rewardsNote).
func (s *Server) historicalBalanceJSON(b HistoricalBalance) map[string]any {
	total := b.Liquid.Add(b.Staked).Add(b.Unbonding)
	return map[string]any{
		apiAddress: b.Address, apiSymbol: s.native.Symbol, apiDate: b.Date,
		apiLiquid: b.Liquid.InexactFloat64(), apiStaked: b.Staked.InexactFloat64(),
		"unbonding": b.Unbonding.InexactFloat64(), apiReward: nil,
		apiTotal: total.InexactFloat64(), "total_usd": b.TotalUSD.StringFixed(2),
		"status": b.Status, "height": b.Height, "block_time": b.BlockTime,
		"anchor_height": b.AnchorHeight, "holdings": b.Lines,
		"unpriced_lines": b.UnpricedLines, "reward_note": rewardsNote,
		apiMethodologyVersion: MethodologyVersion,
	}
}

// balanceCoverageHeaders sets the period coverage headers for a rebuilt
// balance. A past-date balance depends on the blocks from the end of that day
// to the tip, not on the history before it.
func (s *Server) balanceCoverageHeaders(w http.ResponseWriter, earliest time.Time) {
	setPeriodCoverageHeaders(w, s.periodCoverage(earliest.AddDate(0, 0, 1), nowUTC().AddDate(0, 0, 1)))
}

// handleHistoricalBalance serves /balance?date=: the wallet's holdings at the
// end of that UTC day, rebuilt from the index. A date the index cannot rebuild
// exactly is refused with 409 and the reason, never a partial number.
func (s *Server) handleHistoricalBalance(w http.ResponseWriter, r *http.Request, address string, day time.Time) {
	if _, err := AccountOperator(address); err != nil {
		http.Error(w, "address is not a valid account address", http.StatusBadRequest)
		return
	}
	s.balanceCoverageHeaders(w, day)
	out, err := s.HistoricalBalances(chainParam(r.URL.Query().Get(apiChain)), address, []time.Time{day})
	if err != nil {
		http.Error(w, "could not rebuild the balance: "+err.Error(), http.StatusInternalServerError)
		return
	}
	b := out[0]
	if b.Status != balanceExact && b.Status != balanceApproximate {
		http.Error(w, "the balance on "+b.Date+" cannot be rebuilt exactly: "+b.Reason, http.StatusConflict)
		return
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, s.historicalBalanceJSON(b))
}

// statementDates is the quarter ends and year end of year, or the listed
// dates.
func statementDates(year, dates string) ([]time.Time, []string, error) {
	if dates != "" {
		var days []time.Time
		var labels []string
		for _, d := range strings.Split(dates, ",") {
			t, err := time.Parse(apiDateLayout, strings.TrimSpace(d))
			if err != nil {
				return nil, nil, fmt.Errorf("dates must be YYYY-MM-DD, comma separated")
			}
			days = append(days, t)
			labels = append(labels, "")
		}
		if len(days) > maxStatementDates {
			return nil, nil, fmt.Errorf("at most %d dates per statement", maxStatementDates)
		}
		return days, labels, nil
	}
	y, err := strconv.Atoi(year)
	if err != nil || y < 2019 || y > 9999 {
		return nil, nil, fmt.Errorf("year (YYYY) or dates is required")
	}
	days := []time.Time{
		time.Date(y, 3, 31, 0, 0, 0, 0, time.UTC),
		time.Date(y, 6, 30, 0, 0, 0, 0, time.UTC),
		time.Date(y, 9, 30, 0, 0, 0, 0, time.UTC),
		time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	return days, []string{"Q1 end", "Q2 end", "Q3 end", "Year end"}, nil
}

// handleBalanceStatement serves the quarter-end and year-end balances of a
// wallet, or the balances at listed dates, as JSON or CSV. Each date carries
// its own status; one the index cannot rebuild is flagged with its reason and
// has no amounts.
//
//	GET /balance-statement?address=cosmos1...&year=2026[&format=csv]
//	GET /balance-statement?address=cosmos1...&dates=2026-03-31,2026-06-30
func (s *Server) handleBalanceStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	address := strings.TrimSpace(q.Get(apiAddress))
	if _, err := AccountOperator(address); err != nil {
		http.Error(w, "address (a valid account address) is required", http.StatusBadRequest)
		return
	}
	days, labels, err := statementDates(q.Get("year"), q.Get("dates"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	earliest := days[0]
	for _, d := range days {
		if d.Before(earliest) {
			earliest = d
		}
	}
	s.balanceCoverageHeaders(w, earliest)
	chain := chainParam(q.Get(apiChain))
	out, err := s.HistoricalBalances(chain, address, days)
	if err != nil {
		http.Error(w, "could not rebuild the balances: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range out {
		out[i].Label = labels[i]
	}
	if q.Get(apiFormat) == apiCSV {
		w.Header().Set(apiContentType, apiCSVContentType)
		w.Header().Set(apiContentDisposition, "attachment; filename=balance-statement.csv")
		writeBalanceStatementCSV(w, out)
		return
	}
	dates := make([]map[string]any, 0, len(out))
	for _, b := range out {
		m := s.historicalBalanceJSON(b)
		delete(m, apiMethodologyVersion)
		m[apiLabel] = b.Label
		if b.Reason != "" {
			m["reason"] = b.Reason
		}
		if b.Status != balanceExact && b.Status != balanceApproximate {
			for _, k := range []string{apiLiquid, apiStaked, "unbonding", apiTotal, "total_usd"} {
				m[k] = nil
			}
		}
		dates = append(dates, m)
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, map[string]any{
		apiAddress: address, apiChain: chain, apiSymbol: s.native.Symbol,
		apiMethodologyVersion: MethodologyVersion,
		apiNote:               balanceStatementNote,
		"balances":            dates,
	})
}

const balanceStatementNote = "Each balance is the wallet's holdings at the end of the UTC day, rebuilt from the indexed chain events between that day and a recent block whose state the node reports. exact means every movement was replayed; approximate means a staked amount depends on a validator's tokens per share between the recorded points (each day's last block and either side of each slash), or on chain rounding of an undelegation or redelegation, and each such line gives max_error_base, the most it can be off by in base units (a date whose bound would exceed 100 base units is marked incomplete instead). A date marked incomplete or not_reached has no amounts and says why. " + rewardsNote

func writeBalanceStatementCSV(w http.ResponseWriter, out []HistoricalBalance) {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{apiAddress, apiDate, apiLabel, "status", "height", "bucket", apiValidator, "denom", apiSymbol, "quantity", "price_usd", "value_usd", "price_missing", "approximate", "max_error_base", "reason"})
	usd := func(d *decimal.Decimal) string {
		if d == nil {
			return ""
		}
		return d.StringFixed(2)
	}
	for _, b := range out {
		h := ""
		if b.Height > 0 {
			h = strconv.FormatInt(b.Height, 10)
		}
		if b.Status != balanceExact && b.Status != balanceApproximate {
			_ = cw.Write([]string{b.Address, b.Date, b.Label, b.Status, h, "", "", "", "", "", "", "", "", "", "", spreadsheetLabel(b.Reason)})
			continue
		}
		for _, l := range b.Lines {
			price := ""
			if l.PriceUSD != nil {
				price = l.PriceUSD.String()
			}
			_ = cw.Write([]string{b.Address, b.Date, b.Label, b.Status, h, l.Bucket, l.Validator, l.Denom, spreadsheetLabel(l.Symbol), l.Quantity.String(), price, usd(l.ValueUSD), strconv.FormatBool(l.PriceMissing), strconv.FormatBool(l.Approximate), l.MaxErrorBase, ""})
		}
		total := b.TotalUSD
		_ = cw.Write([]string{b.Address, b.Date, b.Label, b.Status, h, apiTotal, "", "", "USD", "", "", usd(&total), "", "", "", fmt.Sprintf("%d unpriced holdings not in the total", b.UnpricedLines)})
	}
	cw.Flush()
}
