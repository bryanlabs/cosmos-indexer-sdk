package taxapi

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
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
	Day      time.Time
	Category string
	Denom    string
	Amount   string
	Events   int64
}

// statementAggregates is everything a statement reads from taxable_events:
// the per-day sums, the distinct reward recipients per month, and the
// distinct reward recipients over the whole period.
type statementAggregates struct {
	Days              []validatorDayAgg
	DelegatorsByMonth map[string]int64
	Delegators        int64
}

const (
	// A statement reads every reward and commission row of the validator in
	// the period from the table heap, because no index holds the amount or
	// the recipient. That measured about 4 seconds per million rows on
	// production (4.4 million rows in 17 seconds). Above this many rows (the
	// planner's estimate) it cannot finish inside the time limit with room
	// for a busy database, so the request is refused at once instead of
	// scanning for minutes. TAX_STATEMENT_MAX_ROWS overrides it; a negative
	// value turns the check off.
	defaultStatementMaxRows = 12_000_000
	// The edge gives up at 120 seconds. The database work is cancelled
	// before that so an abandoned request does not keep scanning.
	defaultStatementTimeout = 90 * time.Second
	statementBuildSlots     = 3

	statementPartDay   = 0
	statementPartMonth = 1
	statementPartTotal = 2
)

// statementSlots bounds how many statements read the heap at once. The
// endpoint is public and one large statement is a multi-gigabyte read.
var statementSlots = make(chan struct{}, statementBuildSlots)

// statementTooLarge is returned when the planner expects more rows than a
// statement can summarise on request.
type statementTooLarge struct {
	Rows  int64
	Limit int64
}

func (e statementTooLarge) Error() string {
	return fmt.Sprintf("This validator has about %s reward and commission records in the chosen period, more than a statement can summarise on request (limit %s). Choose a shorter period, such as one year or one month.",
		approxCount(e.Rows), approxCount(e.Limit))
}

func approxCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return decimal.NewFromInt(n).Div(decimal.NewFromInt(1_000_000)).Round(1).String() + " million"
	case n >= 1_000:
		return decimal.NewFromInt(n).Div(decimal.NewFromInt(1_000)).Round(0).String() + " thousand"
	}
	return decimal.NewFromInt(n).String()
}

// SetStatementLimits overrides the statement row limit and time limit. A
// zero value keeps the default; a negative maxRows turns the row check off.
func (s *Server) SetStatementLimits(maxRows int64, timeout time.Duration) {
	s.statementMaxRows, s.statementTimeout = maxRows, timeout
}

func (s *Server) statementRowLimit() int64 {
	if s.statementMaxRows != 0 {
		return s.statementMaxRows
	}
	return defaultStatementMaxRows
}

func (s *Server) statementTimeLimit() time.Duration {
	if s.statementTimeout > 0 {
		return s.statementTimeout
	}
	return defaultStatementTimeout
}

const statementScope = "validator_address = @validator AND category IN ('reward','commission') AND timestamp >= @start AND timestamp < @end"

// statementRowEstimate asks the planner how many rows the statement would
// read. It costs a few milliseconds and no table access; validators large
// enough to matter are in the column statistics, so the estimate is close.
// EXPLAIN plans with the bound values, so no cached generic plan is involved.
func statementRowEstimate(tx *gorm.DB, val string, start, end time.Time) (int64, error) {
	var plan string
	if err := tx.Raw("EXPLAIN (FORMAT JSON) SELECT 1 FROM taxable_events WHERE "+statementScope,
		sql.Named("validator", val), sql.Named("start", start), sql.Named("end", end)).Row().Scan(&plan); err != nil {
		return 0, err
	}
	var parsed []struct {
		Plan struct {
			Rows float64 `json:"Plan Rows"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &parsed); err != nil || len(parsed) == 0 {
		return 0, fmt.Errorf("unreadable statement plan: %w", err)
	}
	return int64(parsed[0].Plan.Rows), nil
}

// The three results a statement needs used to be three queries, each
// reading the validator's rows from the heap again. They are the same three
// aggregates here, over one read of those rows and one snapshot. The
// recipient counts go through one de-duplicated (month, recipient) set
// instead of two sorts of every row; a NULL recipient is not counted, as
// COUNT(DISTINCT to_addr) did not count it.
const statementAggregateSQL = `WITH ev AS MATERIALIZED (
	SELECT date_trunc('day', timestamp) AS day, category, denom, amount, to_addr
	FROM taxable_events
	WHERE ` + statementScope + `
), pairs AS MATERIALIZED (
	SELECT date_trunc('month', day) AS month, to_addr
	FROM ev WHERE category = 'reward' GROUP BY 1, 2
)
SELECT 0 AS part, day, '' AS month, category, denom, SUM(amount::numeric)::text AS amount, COUNT(*) AS events, 0::bigint AS delegators
FROM ev GROUP BY day, category, denom
UNION ALL
SELECT 1, NULL, to_char(month, 'YYYY-MM'), '', '', '', 0, COUNT(to_addr)
FROM pairs GROUP BY month
UNION ALL
SELECT 2, NULL, '', '', '', '', 0, COUNT(*)
FROM (SELECT to_addr FROM pairs WHERE to_addr IS NOT NULL GROUP BY to_addr) d
ORDER BY 1, 2`

// readStatementAggregates reads the aggregates in one statement. Cancelling
// ctx cancels the query.
func (s *Server) readStatementAggregates(ctx context.Context, val string, start, end time.Time) (statementAggregates, error) {
	agg := statementAggregates{DelegatorsByMonth: map[string]int64{}}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The driver caches prepared statements; a generic plan cannot see
		// the validator or the period and would plan the read badly.
		if err := tx.Exec("SET LOCAL plan_cache_mode = force_custom_plan").Error; err != nil {
			return err
		}
		if err := tx.Exec("SET LOCAL work_mem = '64MB'").Error; err != nil {
			return err
		}
		// The rows are spread over the whole table. A bitmap scan reads them
		// in page order with read-ahead and parallel workers; a plain index
		// scan fetches one page at a time and measured four times slower.
		for _, setting := range []string{"SET LOCAL enable_indexscan = off", "SET LOCAL effective_io_concurrency = 128"} {
			if err := tx.Exec(setting).Error; err != nil {
				return err
			}
		}
		var rows []struct {
			Part       int
			Day        *time.Time
			Month      string
			Category   string
			Denom      string
			Amount     string
			Events     int64
			Delegators int64
		}
		if err := tx.Raw(statementAggregateSQL, sql.Named("validator", val), sql.Named("start", start), sql.Named("end", end)).Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			switch r.Part {
			case statementPartDay:
				if r.Day == nil {
					return errors.New("statement day row without a day")
				}
				agg.Days = append(agg.Days, validatorDayAgg{Day: *r.Day, Category: r.Category, Denom: r.Denom, Amount: r.Amount, Events: r.Events})
			case statementPartMonth:
				agg.DelegatorsByMonth[r.Month] = r.Delegators
			case statementPartTotal:
				agg.Delegators = r.Delegators
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return agg, err
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
	_ = s.db.Table(apiTaxableEvents).Select("MIN(timestamp) AS start, MAX(timestamp) AS \"end\"").Scan(&bounds).Error
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
	coverageCached, coverageAt = c, time.Now()
	return c
}

// statementNote describes block coverage from the index coverage snapshot
// (which years are fully indexed) and adds the replay caveat when one runs.
func (s *Server) statementNote(c statementCoverage) string {
	if c.IndexStart == nil {
		return "No events indexed yet."
	}
	note := fmt.Sprintf("Earliest indexed event: %s.", c.IndexStart.UTC().Format(apiDateLayout))
	if sc := s.indexCoverage(); sc != nil {
		if snap := sc.Snapshot(); snap != nil {
			note = snap.Note
		}
	}
	if c.ReplayCursor != nil {
		note += fmt.Sprintf(" Reward rows from %s on are still being rewritten under methodology %s, so months from %s are incomplete until the replay finishes.",
			c.ReplayCursor.UTC().Format(apiDateLayout), MethodologyVersion, c.ReplayCursor.UTC().Format(apiMonthLayout))
	}
	return note
}

// GET /validator-statement?validator=cosmosvaloper...&chain=&start=&end=&format=csv|json
//
// Aggregated in the database by day, category and denom, with the distinct
// reward recipients per month and overall, in one statement that reads the
// validator's rows once (readStatementAggregates). A period too large to
// read on request is refused with 422 before any row is read, and the
// database work is cancelled when the time limit passes or the caller goes
// away (504).
func (s *Server) handleValidatorStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	val := strings.TrimSpace(q.Get(apiValidator))
	if !strings.HasPrefix(val, "cosmosvaloper1") {
		http.Error(w, "validator (cosmosvaloper address) required", http.StatusBadRequest)
		return
	}
	if hrp, _, err := bech32.DecodeAndConvert(val); err != nil || hrp != "cosmosvaloper" {
		http.Error(w, "the operator address failed its checksum; check it for a typo", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get(apiChain))
	start := dateParam(q.Get(apiStart), time.Time{})
	end := dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1))
	began := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.statementTimeLimit())
	defer cancel()

	// The gate's queries run on a DB-only facade bound to the request, so
	// they stop with it too (see withReadSnapshot for the facade).
	if err := (&Server{db: s.db.WithContext(ctx)}).requireClassificationReady(chain, []string{val}, start, end); err != nil {
		if ctx.Err() != nil {
			writeStatementError(ctx, w, val, s.statementTimeLimit(), err)
			return
		}
		writeTaxError(w, err)
		return
	}
	readyTook := time.Since(began)
	monikers := s.oracle.ValidatorMonikers()
	moniker, known := monikers[val]
	if !known {
		// Not a current validator. It may still be a past one with rows, so the
		// database has the final say; an address with neither is a 404 rather
		// than an empty statement that looks like a quiet validator.
		var found bool
		if err := s.db.WithContext(ctx).Raw("SELECT EXISTS (SELECT 1 FROM taxable_events WHERE validator_address = ? AND validator_address <> '')", val).Row().Scan(&found); err != nil {
			writeStatementError(ctx, w, val, s.statementTimeLimit(), err)
			return
		}
		if !found {
			http.Error(w, "no validator with this operator address on the chain, now or in the indexed history", http.StatusNotFound)
			return
		}
	}

	// Size the read before taking a slot, so a period that is too large is
	// refused at once even while other statements are being built.
	estimate, err := statementRowEstimate(s.db.WithContext(ctx), val, start, end)
	if err == nil {
		if limit := s.statementRowLimit(); limit > 0 && estimate > limit {
			err = statementTooLarge{Rows: estimate, Limit: limit}
		}
	}
	if err != nil {
		writeStatementError(ctx, w, val, s.statementTimeLimit(), err)
		return
	}
	select {
	case statementSlots <- struct{}{}:
	case <-ctx.Done():
		writeStatementError(ctx, w, val, s.statementTimeLimit(), ctx.Err())
		return
	}
	queryBegan := time.Now()
	agg, err := s.readStatementAggregates(ctx, val, start, end)
	<-statementSlots
	if err != nil {
		writeStatementError(ctx, w, val, s.statementTimeLimit(), err)
		return
	}
	queryTook := time.Since(queryBegan)
	aggs, delegatorsByMonth := agg.Days, agg.DelegatorsByMonth

	// One oracle call per day and denom not yet cached; fetch them together
	// so a long period on a fresh process does not make them one at a time.
	priced := make([]tax.TaxableEvent, 0, len(aggs))
	for _, a := range aggs {
		priced = append(priced, tax.TaxableEvent{Category: a.Category, Denom: a.Denom, Timestamp: a.Day})
	}
	priceBegan := time.Now()
	s.prewarmPrices(chain, priced, nil)
	defer func() {
		//nolint:gosec // val passed the bech32 checksum, so it carries no control characters
		log.Printf("validator-statement %s %s..%s: about %d rows, %d day groups, readiness %s, query %s, prices %s, total %s",
			val, start.Format(apiDateLayout), end.Format(apiDateLayout), estimate, len(aggs),
			readyTook.Round(time.Millisecond), queryTook.Round(time.Millisecond), time.Since(priceBegan).Round(time.Millisecond), time.Since(began).Round(time.Millisecond))
	}()

	cov := s.statementCoverage()
	cov.Note = s.statementNote(cov)
	period := s.periodCoverage(start, end)
	var blocks *IndexCoverage
	if sc := s.indexCoverage(); sc != nil {
		blocks = sc.Snapshot()
	}
	cursorMonth := ""
	if cov.ReplayCursor != nil {
		cursorMonth = cov.ReplayCursor.UTC().Format(apiMonthLayout)
	}

	meta := s.oracle.Denoms(chain)
	months := map[string]*ValidatorStatementMonth{}
	missingDays := map[string]map[string]bool{}
	for _, a := range aggs {
		month := a.Day.UTC().Format(apiMonthLayout)
		m := months[month]
		if m == nil {
			m = &ValidatorStatementMonth{Month: month, Symbol: s.native.Symbol, UniqueDelegators: delegatorsByMonth[month], Incomplete: (cursorMonth != "" && month >= cursorMonth) || (blocks != nil && !blocks.MonthCovered(month))}
			months[month] = m
			missingDays[month] = map[string]bool{}
		}
		row := s.buildRow(chain, meta, a.Day, "", a.Category, "in", a.Denom, a.Amount, "", "")
		if row.Symbol != "" && a.Denom == s.native.Denom {
			m.Symbol = row.Symbol
		}
		if row.PriceMissing {
			missingDays[month][a.Day.UTC().Format(apiDateLayout)] = true
			m.PriceMissingEvents += a.Events
		}
		if a.Denom != s.native.Denom {
			// Non-native reward denoms are rare on the Hub; fold their USD value
			// in and keep the amount column native-only.
			if a.Category == apiCommission {
				m.CommissionUSD = m.CommissionUSD.Add(row.ValueUSD)
			} else {
				m.DelegatorRewardsUSD = m.DelegatorRewardsUSD.Add(row.ValueUSD)
				m.RewardEvents += a.Events
			}
			continue
		}
		if a.Category == apiCommission {
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
	for _, k := range keys {
		months[k].PriceMissingDays = len(missingDays[k])
		out = append(out, *months[k])
	}
	totals := statementTotals(out, period, s.native.Symbol)
	totals.UniqueDelegators = agg.Delegators

	if q.Get(apiFormat) == apiCSV {
		w.Header().Set(apiContentType, apiCSVContentType)
		w.Header().Set(apiContentDisposition, "attachment; filename=validator-statement.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{apiValidator, apiMoniker, "month", apiSymbol, "commission_amount", "commission_usd", "delegator_rewards_amount", "delegator_rewards_usd", "reward_events", "unique_delegators", "price_missing_days", "price_missing_events", "incomplete"})
		for _, m := range append(out, totals) {
			inc := apiFalse
			if m.Incomplete {
				inc = apiTrue
			}
			_ = cw.Write([]string{val, spreadsheetLabel(moniker), m.Month, m.Symbol, m.CommissionAmount.String(), m.CommissionUSD.StringFixed(2), m.DelegatorRewards.String(), m.DelegatorRewardsUSD.StringFixed(2), decimal.NewFromInt(m.RewardEvents).String(), decimal.NewFromInt(m.UniqueDelegators).String(), itoa(m.PriceMissingDays), decimal.NewFromInt(m.PriceMissingEvents).String(), inc})
		}
		cw.Flush()
		return
	}
	writeJSON(w, map[string]any{
		apiValidator: val, apiMoniker: moniker, apiChain: chain,
		"jurisdiction":        s.jurisdictionLabel(chain, val),
		apiMethodologyVersion: MethodologyVersion,
		"recognition_policy":  RecognitionPolicyDefault,
		"coverage":            cov,
		"index_coverage":      period,
		apiNote:               "Commission is the operator's own withdrawn commission. Delegator rewards are rewards actually withdrawn by delegators through this validator (claims and auto-withdrawals), not rewards accrued. Unique delegators counts distinct withdrawing addresses. USD figures for rows with no reference price are counted at zero; price_missing_days and price_missing_events say how many.",
		"months":              out,
		apiTotal:              totals,
	})
}

// writeStatementError answers a failed statement read: 422 for a period too
// large to read on request, 504 when the time limit passed or the caller
// went away, 500 otherwise.
func writeStatementError(ctx context.Context, w http.ResponseWriter, val string, limit time.Duration, err error) {
	var tooLarge statementTooLarge
	switch {
	case errors.As(err, &tooLarge):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case ctx.Err() != nil:
		log.Printf("validator-statement %s: stopped after the %s limit: %v", val, limit, err) //nolint:gosec // val passed the bech32 checksum
		http.Error(w, fmt.Sprintf("The statement did not finish within %d seconds and was stopped. Choose a shorter period, such as one year or one month.", int(limit.Seconds())), http.StatusGatewayTimeout)
	default:
		log.Printf("validator-statement %s: %v", val, err) //nolint:gosec // val passed the bech32 checksum
		http.Error(w, "could not read the validator statement", http.StatusInternalServerError)
	}
}

// statementTotals sums the month rows. A month entirely inside an index gap
// has no rows to flag, so the total is also incomplete whenever the period
// itself is not complete.
func statementTotals(months []ValidatorStatementMonth, period PeriodCoverage, symbol string) ValidatorStatementMonth {
	totals := ValidatorStatementMonth{Month: apiTotalLabel, Symbol: symbol, Incomplete: !period.Complete}
	for _, m := range months {
		totals.CommissionAmount = totals.CommissionAmount.Add(m.CommissionAmount)
		totals.CommissionUSD = totals.CommissionUSD.Add(m.CommissionUSD)
		totals.DelegatorRewards = totals.DelegatorRewards.Add(m.DelegatorRewards)
		totals.DelegatorRewardsUSD = totals.DelegatorRewardsUSD.Add(m.DelegatorRewardsUSD)
		totals.RewardEvents += m.RewardEvents
		totals.PriceMissingDays += m.PriceMissingDays
		totals.PriceMissingEvents += m.PriceMissingEvents
		totals.Incomplete = totals.Incomplete || m.Incomplete
	}
	return totals
}
