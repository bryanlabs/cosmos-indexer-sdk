package taxapi

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ValidatorStatementMonth is one month of a validator operator's statement:
// what the operator earned in commission and what its delegators withdrew
// in rewards through it, with the number of distinct delegators paid.
type ValidatorStatementMonth struct {
	Month               string          `json:"month"`
	Symbol              string          `json:"symbol"`
	CommissionAmount    decimal.Decimal `json:"commission_amount"`
	CommissionUSD       decimal.Decimal `json:"commission_usd"`
	DelegatorRewards    decimal.Decimal `json:"delegator_rewards_amount"`
	DelegatorRewardsUSD decimal.Decimal `json:"delegator_rewards_usd"`
	RewardEvents        int64           `json:"reward_events"`
	UniqueDelegators    int64           `json:"unique_delegators"`
	PriceMissingDays    int             `json:"price_missing_days"`
}

type validatorDayAgg struct {
	Day        time.Time
	Category   string
	Denom      string
	Amount     string
	Events     int64
	Recipients int64
}

// GET /validator-statement?validator=cosmosvaloper...&chain=&start=&end=&format=csv|json
//
// Aggregated in the database by day, category and denom, so a large
// validator's hundreds of thousands of reward withdrawals cost one query.
func (s *Server) handleValidatorStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	val := strings.TrimSpace(q.Get("validator"))
	if !strings.HasPrefix(val, "cosmosvaloper1") {
		http.Error(w, "validator (cosmosvaloper address) required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	start := dateParam(q.Get("start"), time.Time{})
	end := dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1))

	var aggs []validatorDayAgg
	if err := s.db.Table("taxable_events").
		Select("date_trunc('day', timestamp) AS day, category, denom, SUM(amount::numeric)::text AS amount, COUNT(*) AS events, COUNT(DISTINCT to_addr) AS recipients").
		Where("validator_address = ? AND category IN ('reward','commission') AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("1, 2, 3").Order("1").Scan(&aggs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Unique delegators per month need a second pass (distinct across days).
	type monthDelegators struct {
		Month      string
		Delegators int64
	}
	var md []monthDelegators
	_ = s.db.Table("taxable_events").
		Select("to_char(date_trunc('month', timestamp), 'YYYY-MM') AS month, COUNT(DISTINCT to_addr) AS delegators").
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("1").Scan(&md).Error
	delegatorsByMonth := map[string]int64{}
	for _, m := range md {
		delegatorsByMonth[m.Month] = m.Delegators
	}

	meta := s.oracle.Denoms(chain)
	months := map[string]*ValidatorStatementMonth{}
	for _, a := range aggs {
		month := a.Day.UTC().Format("2006-01")
		m := months[month]
		if m == nil {
			m = &ValidatorStatementMonth{Month: month, Symbol: s.native.Symbol, UniqueDelegators: delegatorsByMonth[month]}
			months[month] = m
		}
		row := s.buildRow(chain, meta, a.Day, "", a.Category, "in", a.Denom, a.Amount, "", "")
		if row.Symbol != "" && a.Denom == s.native.Denom {
			m.Symbol = row.Symbol
		}
		if a.Denom != s.native.Denom {
			// Non-native reward denoms are rare on the Hub; fold their USD value
			// in and keep the amount column native-only.
			if row.PriceMissing {
				m.PriceMissingDays++
			}
			if a.Category == "commission" {
				m.CommissionUSD = m.CommissionUSD.Add(row.ValueUSD)
			} else {
				m.DelegatorRewardsUSD = m.DelegatorRewardsUSD.Add(row.ValueUSD)
				m.RewardEvents += a.Events
			}
			continue
		}
		if row.PriceMissing {
			m.PriceMissingDays++
		}
		if a.Category == "commission" {
			m.CommissionAmount = m.CommissionAmount.Add(row.Amount)
			m.CommissionUSD = m.CommissionUSD.Add(row.ValueUSD)
		} else {
			m.DelegatorRewards = m.DelegatorRewards.Add(row.Amount)
			m.DelegatorRewardsUSD = m.DelegatorRewardsUSD.Add(row.ValueUSD)
			m.RewardEvents += a.Events
		}
	}
	keys := make([]string, 0, len(months))
	for k := range months {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ValidatorStatementMonth, 0, len(keys))
	var totals ValidatorStatementMonth
	totals.Month = "TOTAL"
	totals.Symbol = s.native.Symbol
	for _, k := range keys {
		m := *months[k]
		out = append(out, m)
		totals.CommissionAmount = totals.CommissionAmount.Add(m.CommissionAmount)
		totals.CommissionUSD = totals.CommissionUSD.Add(m.CommissionUSD)
		totals.DelegatorRewards = totals.DelegatorRewards.Add(m.DelegatorRewards)
		totals.DelegatorRewardsUSD = totals.DelegatorRewardsUSD.Add(m.DelegatorRewardsUSD)
		totals.RewardEvents += m.RewardEvents
		totals.PriceMissingDays += m.PriceMissingDays
	}
	var allDelegators int64
	_ = s.db.Table("taxable_events").
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Distinct("to_addr").Count(&allDelegators).Error
	totals.UniqueDelegators = allDelegators
	moniker := s.oracle.ValidatorMonikers()[val]

	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=validator-statement.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"validator", "moniker", "month", "symbol", "commission_amount", "commission_usd", "delegator_rewards_amount", "delegator_rewards_usd", "reward_events", "unique_delegators", "price_missing_days"})
		for _, m := range append(out, totals) {
			_ = cw.Write([]string{val, spreadsheetLabel(moniker), m.Month, m.Symbol, m.CommissionAmount.String(), m.CommissionUSD.StringFixed(2), m.DelegatorRewards.String(), m.DelegatorRewardsUSD.StringFixed(2), decimal.NewFromInt(m.RewardEvents).String(), decimal.NewFromInt(m.UniqueDelegators).String(), itoa(m.PriceMissingDays)})
		}
		cw.Flush()
		return
	}
	writeJSON(w, map[string]any{
		"validator": val, "moniker": moniker, "chain": chain,
		"jurisdiction":        jurisdictionLabel(moniker),
		"methodology_version": MethodologyVersion,
		"recognition_policy":  RecognitionPolicyDefault,
		"note":                "Commission is the operator's own withdrawn commission. Delegator rewards are rewards actually withdrawn by delegators through this validator (claims and auto-withdrawals), not rewards accrued. Unique delegators counts distinct withdrawing addresses.",
		"months":              out,
		"total":               totals,
	})
}
