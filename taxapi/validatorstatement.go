package taxapi

import (
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
)

// ValidatorStatementMonth is one month of a validator operator's statement:
// what the operator earned in commission and what its delegators withdrew
// in rewards through it, with the number of distinct delegators paid.
//
// PriceMissingDays counts calendar days in the month with at least one reward
// or commission row that had no reference price; PriceMissingEvents counts the
// rows. Both are zero when every row was priced. Incomplete marks a month the
// methodology replay has not finished rewriting yet.
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
	PriceMissingEvents  int64           `json:"price_missing_events"`
	Incomplete          bool            `json:"incomplete"`
}

type validatorDayAgg struct {
	Day        time.Time
	Category   string
	Denom      string
	Amount     string
	Events     int64
	Recipients int64
}

// statementCoverage tells the reader how much of history the statement can
// see. IndexStart is the first indexed event on the chain; anything earlier
// is not in the database at all. ReplayCursor is set while a methodology
// replay is still rewriting reward rows: months at or after it may be missing
// validator attribution and are flagged incomplete.
type statementCoverage struct {
	IndexStart   *time.Time `json:"index_start"`
	IndexEnd     *time.Time `json:"index_end"`
	ReplayCursor *time.Time `json:"replay_cursor,omitempty"`
	Complete     bool       `json:"complete"`
	Note         string     `json:"note"`
}

var (
	coverageMu     sync.Mutex
	coverageCached statementCoverage
	coverageAt     time.Time
)

// coverage is cheap (three index-only lookups) but the statement page polls
// it on every request, so it is cached for a minute.
func (s *Server) statementCoverage() statementCoverage {
	coverageMu.Lock()
	defer coverageMu.Unlock()
	if time.Since(coverageAt) < time.Minute {
		return coverageCached
	}
	var c statementCoverage
	var bounds struct {
		Start *time.Time
		End   *time.Time
	}
	_ = s.db.Table("taxable_events").Select("MIN(timestamp) AS start, MAX(timestamp) AS \"end\"").Scan(&bounds).Error
	c.IndexStart, c.IndexEnd = bounds.Start, bounds.End
	// The replay rewrites reward rows written by the staking and distribution
	// messages listed in rowsFor; LSM reward rows (MsgWithdrawAllTokenizeShareRecordReward)
	// keep an empty trigger and are not part of it, so the cursor must look
	// past them or the statement would report the replay as never finishing.
	// The earliest legacy rows come off the partial index in timestamp order;
	// joining only that slice to message types keeps this under a second.
	var cursor struct{ Cursor *time.Time }
	_ = s.db.Raw(`SELECT MIN(x.timestamp) AS cursor FROM (
			SELECT te.timestamp, te.message_id FROM taxable_events te
			WHERE te.category = 'reward' AND COALESCE(te.reward_trigger, '') = ''
			ORDER BY te.timestamp LIMIT 20000) x
		JOIN messages m ON m.id = x.message_id
		JOIN message_types mt ON mt.id = m.message_type_id
		WHERE mt.message_type IN ?`, replayedMessageTypes).Scan(&cursor).Error
	c.ReplayCursor = cursor.Cursor
	c.Complete = c.ReplayCursor == nil
	switch {
	case c.IndexStart == nil:
		c.Note = "No events indexed yet."
	case c.Complete:
		c.Note = fmt.Sprintf("Index covers %s onward; earlier activity is not yet included.", c.IndexStart.UTC().Format("2006-01-02"))
	default:
		c.Note = fmt.Sprintf("Index covers %s onward; earlier activity is not yet included. Reward rows from %s on are still being rewritten under methodology %s, so months from %s are incomplete until the replay finishes.",
			c.IndexStart.UTC().Format("2006-01-02"), c.ReplayCursor.UTC().Format("2006-01-02"), MethodologyVersion, c.ReplayCursor.UTC().Format("2006-01"))
	}
	coverageCached, coverageAt = c, time.Now()
	return c
}

// GET /validator-statement?validator=cosmosvaloper...&chain=&start=&end=&format=csv|json
//
// Aggregated in the database by day, category and denom over the
// (validator_address, category, timestamp) index, so a large validator's
// hundreds of thousands of reward withdrawals cost one indexed query plus a
// per-month distinct count.
func (s *Server) handleValidatorStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	val := strings.TrimSpace(q.Get("validator"))
	if !strings.HasPrefix(val, "cosmosvaloper1") {
		http.Error(w, "validator (cosmosvaloper address) required", http.StatusBadRequest)
		return
	}
	if hrp, _, err := bech32.DecodeAndConvert(val); err != nil || hrp != "cosmosvaloper" {
		http.Error(w, "the operator address failed its checksum; check it for a typo", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get("chain"))
	start := dateParam(q.Get("start"), time.Time{})
	end := dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1))

	monikers := s.oracle.ValidatorMonikers()
	moniker, known := monikers[val]
	if !known {
		// Not a current validator. It may still be a past one with rows, so the
		// database has the final say; an address with neither is a 404 rather
		// than an empty statement that looks like a quiet validator.
		var n int64
		_ = s.db.Table("taxable_events").Where("validator_address = ?", val).Limit(1).Count(&n).Error
		if n == 0 {
			http.Error(w, "no validator with this operator address on the chain, now or in the indexed history", http.StatusNotFound)
			return
		}
	}

	var aggs []validatorDayAgg
	if err := s.db.Table("taxable_events").
		Select("date_trunc('day', timestamp) AS day, category, denom, SUM(amount::numeric)::text AS amount, COUNT(*) AS events, COUNT(DISTINCT to_addr) AS recipients").
		Where("validator_address = ? AND category IN ('reward','commission') AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("day, category, denom").Order("day").Scan(&aggs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Unique delegators per month need a second pass (distinct across days).
	type monthDelegators struct {
		Month      string
		Delegators int64
	}
	var md []monthDelegators
	if err := s.db.Table("taxable_events").
		Select("to_char(date_trunc('month', timestamp), 'YYYY-MM') AS month, COUNT(DISTINCT to_addr) AS delegators").
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("month").Scan(&md).Error; err != nil {
		log.Printf("validator-statement: monthly delegators for %s: %v", val, err)
	}
	delegatorsByMonth := map[string]int64{}
	for _, m := range md {
		delegatorsByMonth[m.Month] = m.Delegators
	}

	cov := s.statementCoverage()
	cursorMonth := ""
	if cov.ReplayCursor != nil {
		cursorMonth = cov.ReplayCursor.UTC().Format("2006-01")
	}

	meta := s.oracle.Denoms(chain)
	months := map[string]*ValidatorStatementMonth{}
	missingDays := map[string]map[string]bool{}
	for _, a := range aggs {
		month := a.Day.UTC().Format("2006-01")
		m := months[month]
		if m == nil {
			m = &ValidatorStatementMonth{Month: month, Symbol: s.native.Symbol, UniqueDelegators: delegatorsByMonth[month], Incomplete: cursorMonth != "" && month >= cursorMonth}
			months[month] = m
			missingDays[month] = map[string]bool{}
		}
		row := s.buildRow(chain, meta, a.Day, "", a.Category, "in", a.Denom, a.Amount, "", "")
		if row.Symbol != "" && a.Denom == s.native.Denom {
			m.Symbol = row.Symbol
		}
		if row.PriceMissing {
			missingDays[month][a.Day.UTC().Format("2006-01-02")] = true
			m.PriceMissingEvents += a.Events
		}
		if a.Denom != s.native.Denom {
			// Non-native reward denoms are rare on the Hub; fold their USD value
			// in and keep the amount column native-only.
			if a.Category == "commission" {
				m.CommissionUSD = m.CommissionUSD.Add(row.ValueUSD)
			} else {
				m.DelegatorRewardsUSD = m.DelegatorRewardsUSD.Add(row.ValueUSD)
				m.RewardEvents += a.Events
			}
			continue
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
		months[k].PriceMissingDays = len(missingDays[k])
		m := *months[k]
		out = append(out, m)
		totals.CommissionAmount = totals.CommissionAmount.Add(m.CommissionAmount)
		totals.CommissionUSD = totals.CommissionUSD.Add(m.CommissionUSD)
		totals.DelegatorRewards = totals.DelegatorRewards.Add(m.DelegatorRewards)
		totals.DelegatorRewardsUSD = totals.DelegatorRewardsUSD.Add(m.DelegatorRewardsUSD)
		totals.RewardEvents += m.RewardEvents
		totals.PriceMissingDays += m.PriceMissingDays
		totals.PriceMissingEvents += m.PriceMissingEvents
		totals.Incomplete = totals.Incomplete || m.Incomplete
	}
	var allDelegators int64
	_ = s.db.Table("taxable_events").
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Distinct("to_addr").Count(&allDelegators).Error
	totals.UniqueDelegators = allDelegators

	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=validator-statement.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"validator", "moniker", "month", "symbol", "commission_amount", "commission_usd", "delegator_rewards_amount", "delegator_rewards_usd", "reward_events", "unique_delegators", "price_missing_days", "price_missing_events", "incomplete"})
		for _, m := range append(out, totals) {
			inc := "false"
			if m.Incomplete {
				inc = "true"
			}
			_ = cw.Write([]string{val, spreadsheetLabel(moniker), m.Month, m.Symbol, m.CommissionAmount.String(), m.CommissionUSD.StringFixed(2), m.DelegatorRewards.String(), m.DelegatorRewardsUSD.StringFixed(2), decimal.NewFromInt(m.RewardEvents).String(), decimal.NewFromInt(m.UniqueDelegators).String(), itoa(m.PriceMissingDays), decimal.NewFromInt(m.PriceMissingEvents).String(), inc})
		}
		cw.Flush()
		return
	}
	writeJSON(w, map[string]any{
		"validator": val, "moniker": moniker, "chain": chain,
		"jurisdiction":        jurisdictionLabel(moniker),
		"methodology_version": MethodologyVersion,
		"recognition_policy":  RecognitionPolicyDefault,
		"coverage":            cov,
		"note":                "Commission is the operator's own withdrawn commission. Delegator rewards are rewards actually withdrawn by delegators through this validator (claims and auto-withdrawals), not rewards accrued. Unique delegators counts distinct withdrawing addresses. USD figures for rows with no reference price are counted at zero; price_missing_days and price_missing_events say how many.",
		"months":              out,
		"total":               totals,
	})
}
