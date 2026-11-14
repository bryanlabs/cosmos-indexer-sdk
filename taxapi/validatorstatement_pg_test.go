package taxapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	statementPGIBCDenom = "ibc/ABC"
	statementPGPath     = "/validator-statement?chain=cosmoshub&validator="
	// A recipient the fixture stores as SQL NULL.
	statementPGNoRecipient = "NULL"
	statementPGAlpha       = "cosmos1alpha"
	statementPGDenomsPath  = "/denoms"
)

var (
	statementPGBig   = flowPGValoper(11)
	statementPGSmall = flowPGValoper(12)
	statementPGNone  = flowPGValoper(13)
	statementPGGold  = flowPGValoper(14)
)

// statementPGServer opens one connection with a throwaway schema holding
// taxable_events under the production statement index, the empty tables the
// readiness gate reads, and a seeded pseudo-random history: two validators,
// two denoms, reward, commission and unrelated rows, amounts past int64,
// timestamps on month and day boundaries, empty and NULL recipients.
func statementPGServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres validator statement verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	schema := fmt.Sprintf("validator_statement_test_%d", time.Now().UnixNano())
	t.Logf("retained fixture schema: %s", schema)
	t.Cleanup(func() { _ = connection.Close() })
	for _, sql := range []string{
		"CREATE SCHEMA " + schema,
		"SET search_path = " + schema,
		"SET TIME ZONE 'UTC'",
		"CREATE TABLE chains (id bigint, chain_id text)",
		"CREATE TABLE blocks (id bigint, chain_id bigint, time_stamp timestamptz)",
		"CREATE TABLE txes (id bigint, block_id bigint, hash text, code bigint)",
		"CREATE TABLE messages (id bigint, tx_id bigint, opaque boolean, message_type_id bigint)",
		"CREATE TABLE message_types (id bigint, message_type text)",
		"CREATE TABLE message_parser_errors (message_id bigint)",
		"CREATE TABLE failed_messages (tx_id bigint)",
		"CREATE TABLE failed_txes (block_id bigint, hash text)",
		"CREATE TABLE addresses (id bigint, address text)",
		"CREATE TABLE tx_signer_addresses (tx_id bigint, address_id bigint)",
		"CREATE TABLE fees (tx_id bigint, payer_address_id bigint)",
		"CREATE TABLE message_events (id bigint, message_id bigint)",
		"CREATE TABLE message_event_attributes (message_event_id bigint, value text)",
		"CREATE TABLE taxable_events (id bigserial PRIMARY KEY, message_id bigint, sub_index bigint, category text, amount text, denom text, from_addr text, to_addr text, block_height bigint, timestamp timestamptz, tx_hash text, asset text, validator_address text, reward_trigger text, detail text)",
		"CREATE INDEX idx_tax_validator ON taxable_events (validator_address, category, timestamp) WHERE validator_address <> ''",
		"INSERT INTO chains VALUES (1,'cosmoshub-4')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}

	rng := rand.New(rand.NewSource(20261113))
	first := time.Date(2024, 11, 20, 0, 0, 0, 0, time.UTC)
	span := int64(time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC).Sub(first) / time.Second)
	var values []string
	add := func(category, amount, denom, to string, ts time.Time, validator string) {
		recipient := "'" + to + "'"
		if to == statementPGNoRecipient {
			recipient = statementPGNoRecipient
		}
		values = append(values, fmt.Sprintf("('%s','%s','%s',%s,'%s','%s')", category, amount, denom, recipient, ts.Format(time.RFC3339Nano), validator))
	}
	for i := 0; i < 6000; i++ {
		validator := statementPGBig
		if i%7 == 0 {
			validator = statementPGSmall
		}
		category := apiReward
		switch {
		case i%41 == 0:
			category = apiCommission
		case i%53 == 0:
			// Rows of another category on the same validator are not part of
			// a statement.
			category = apiDelegate
		}
		denom := apiUatom
		if i%29 == 0 {
			denom = statementPGIBCDenom
		}
		amount := fmt.Sprint(1 + rng.Int63n(5_000_000_000))
		if i%97 == 0 {
			amount = "98765432109876543210987"
		}
		ts := first.Add(time.Duration(rng.Int63n(span)) * time.Second)
		switch i % 101 {
		case 0:
			ts = time.Date(2025, time.Month(1+i%12), 1, 0, 0, 0, 0, time.UTC)
		case 1:
			ts = time.Date(2025, time.Month(1+i%12), 1, 0, 0, 0, 0, time.UTC).Add(-time.Microsecond)
		}
		add(category, amount, denom, fmt.Sprintf("cosmos1delegator%03d", rng.Intn(180)), ts, validator)
	}
	// Recipients that differ only by case, an empty one and a missing one.
	edge := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, to := range []string{"cosmos1CASE", "cosmos1case", "", statementPGNoRecipient, statementPGNoRecipient, ""} {
		add(apiReward, "7", apiUatom, to, edge, statementPGBig)
	}
	// A month whose only rewards have no recipient still appears, with no
	// delegators.
	for i := 0; i < 2; i++ {
		add(apiReward, "11", apiUatom, statementPGNoRecipient, time.Date(2026, 6, 10, i, 0, 0, 0, time.UTC), statementPGBig)
	}
	// A small validator with figures pinned by hand in the handler test.
	gold := []struct {
		category, amount, denom, to, ts string
	}{
		{apiReward, "1500000", apiUatom, statementPGAlpha, "2025-03-31T23:59:59Z"},
		{apiReward, "2500000", apiUatom, "cosmos1beta", "2025-03-31T23:59:59Z"},
		{apiReward, "1000000", apiUatom, statementPGAlpha, "2025-04-01T00:00:00Z"},
		{apiReward, "250000", apiUatom, statementPGAlpha, "2025-04-20T08:00:00Z"},
		{apiReward, "42", statementPGIBCDenom, "cosmos1gamma", "2025-04-20T09:00:00Z"},
		{apiCommission, "3000000", apiUatom, "cosmos1operator", "2025-04-02T00:00:00Z"},
		{apiDelegate, "9000000", apiUatom, statementPGAlpha, "2025-04-03T00:00:00Z"},
		{apiReward, "500000", apiUatom, "cosmos1delta", "2026-01-05T00:00:00Z"},
	}
	for _, g := range gold {
		ts, err := time.Parse(time.RFC3339, g.ts)
		require.NoError(t, err)
		add(g.category, g.amount, g.denom, g.to, ts, statementPGGold)
	}
	for i := 0; i < len(values); i += 1000 {
		end := min(i+1000, len(values))
		require.NoError(t, database.Exec("INSERT INTO taxable_events (category, amount, denom, to_addr, timestamp, validator_address) VALUES "+strings.Join(values[i:end], ",")).Error)
	}
	require.NoError(t, database.Exec("ANALYZE taxable_events").Error)

	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == statementPGDenomsPath {
			_, _ = w.Write([]byte(`{"denoms":{"uatom":{"symbol":"ATOM","decimals":6}}}`))
			return
		}
		if r.URL.Query().Get(apiDenom) != apiUatom {
			_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"usd":10,"found":true}`))
	}))
	t.Cleanup(prices.Close)
	return &Server{db: database, native: DefaultNativeAsset, oracle: NewOracle(prices.URL, "")}
}

// legacyStatementAggregates is the three queries the statement handler ran
// before they became one, kept word for word as the reference the single
// read must match.
func legacyStatementAggregates(t *testing.T, s *Server, val string, start, end time.Time) statementAggregates {
	t.Helper()
	out := statementAggregates{DelegatorsByMonth: map[string]int64{}}
	var days []struct {
		Day        time.Time
		Category   string
		Denom      string
		Amount     string
		Events     int64
		Recipients int64
	}
	require.NoError(t, s.db.Table(apiTaxableEvents).
		Select("date_trunc('day', timestamp) AS day, category, denom, SUM(amount::numeric)::text AS amount, COUNT(*) AS events, COUNT(DISTINCT to_addr) AS recipients").
		Where("validator_address = ? AND category IN ('reward','commission') AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("day, category, denom").Order("day").Scan(&days).Error)
	for _, d := range days {
		out.Days = append(out.Days, validatorDayAgg{Day: d.Day, Category: d.Category, Denom: d.Denom, Amount: d.Amount, Events: d.Events})
	}
	var md []struct {
		Month      string
		Delegators int64
	}
	require.NoError(t, s.db.Table(apiTaxableEvents).
		Select("to_char(date_trunc('month', timestamp), 'YYYY-MM') AS month, COUNT(DISTINCT to_addr) AS delegators").
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Group("month").Scan(&md).Error)
	for _, m := range md {
		out.DelegatorsByMonth[m.Month] = m.Delegators
	}
	require.NoError(t, s.db.Table(apiTaxableEvents).
		Where("validator_address = ? AND category = 'reward' AND timestamp >= ? AND timestamp < ?", val, start, end).
		Distinct("to_addr").Count(&out.Delegators).Error)
	return out
}

func mustStatementAggregates(t *testing.T, s *Server, val string, start, end time.Time) statementAggregates {
	t.Helper()
	agg, err := s.readStatementAggregates(context.Background(), val, start, end)
	require.NoError(t, err)
	return agg
}

func statementDayKeys(days []validatorDayAgg) []string {
	keys := make([]string, 0, len(days))
	for _, d := range days {
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%s|%d", d.Day.UTC().Format(time.RFC3339Nano), d.Category, d.Denom, d.Amount, d.Events))
	}
	sort.Strings(keys)
	return keys
}

// The single read must return exactly what the three former queries did:
// every day sum as the same text, every event count, and the same distinct
// recipients per month and over the period.
func TestStatementAggregatesMatchLegacyQueriesPostgres(t *testing.T) {
	s := statementPGServer(t)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	windows := []struct {
		name       string
		start, end time.Time
	}{
		{"all time", time.Time{}, day(2026, 11, 14)},
		{"calendar 2025", day(2025, 1, 1), day(2026, 1, 1)},
		{"one month", day(2025, 6, 1), day(2025, 7, 1)},
		{"inside two months", day(2025, 3, 17), day(2025, 4, 9)},
		{"one day", day(2025, 6, 15), day(2025, 6, 16)},
		{"month without recipients", day(2026, 6, 1), day(2026, 7, 1)},
		{"before any row", day(2020, 1, 1), day(2021, 1, 1)},
		{"empty range", day(2025, 6, 1), day(2025, 6, 1)},
	}
	total := 0
	for _, val := range []string{statementPGBig, statementPGSmall, statementPGGold, statementPGNone} {
		for _, w := range windows {
			want := legacyStatementAggregates(t, s, val, w.start, w.end)
			got, err := s.readStatementAggregates(context.Background(), val, w.start, w.end)
			require.NoError(t, err, w.name)
			require.Equal(t, statementDayKeys(want.Days), statementDayKeys(got.Days), "%s %s: day sums", val, w.name)
			require.Equal(t, want.DelegatorsByMonth, got.DelegatorsByMonth, "%s %s: delegators by month", val, w.name)
			require.Equal(t, want.Delegators, got.Delegators, "%s %s: delegators", val, w.name)
			require.True(t, sort.SliceIsSorted(got.Days, func(i, j int) bool { return got.Days[i].Day.Before(got.Days[j].Day) }), "%s %s: days in order", val, w.name)
			total += len(got.Days)
		}
	}
	require.Greater(t, total, 1500, "the fixture must exercise many day groups")
	all, err := s.readStatementAggregates(context.Background(), statementPGBig, time.Time{}, day(2026, 11, 14))
	require.NoError(t, err)
	require.Len(t, all.DelegatorsByMonth, 17)
	require.Equal(t, map[string]int64{"2026-06": 0}, mustStatementAggregates(t, s, statementPGBig, day(2026, 6, 1), day(2026, 7, 1)).DelegatorsByMonth)
	// 180 seeded recipients, two that differ only by case and the empty
	// string; a missing recipient is not counted.
	require.Equal(t, int64(183), all.Delegators)
}

func statementPGRequest(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleValidatorStatement(rec, httptest.NewRequest(http.MethodGet, query, nil))
	return rec
}

// The whole handler on figures worked out by hand: months, totals, unique
// delegators, unpriced rows, and the same rows as CSV.
func TestValidatorStatementHandlerPostgres(t *testing.T) {
	s := statementPGServer(t)
	rec := statementPGRequest(t, s, statementPGPath+statementPGGold+"&start=2025-01-01&end=2026-01-01")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Validator string                    `json:"validator"`
		Months    []json.RawMessage         `json:"months"`
		Total     json.RawMessage           `json:"total"`
		Parsed    []ValidatorStatementMonth `json:"-"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, statementPGGold, body.Validator)
	require.Len(t, body.Months, 2)
	require.JSONEq(t, `{"month":"2025-03","symbol":"ATOM","commission_amount":"0","commission_usd":"0","delegator_rewards_amount":"4","delegator_rewards_usd":"40","reward_events":2,"unique_delegators":2,"price_missing_days":0,"price_missing_events":0,"incomplete":false}`, string(body.Months[0]))
	require.JSONEq(t, `{"month":"2025-04","symbol":"ATOM","commission_amount":"3","commission_usd":"30","delegator_rewards_amount":"1.25","delegator_rewards_usd":"12.5","reward_events":3,"unique_delegators":2,"price_missing_days":1,"price_missing_events":1,"incomplete":false}`, string(body.Months[1]))
	// The period's coverage is unknown without a block scanner, which marks
	// the total incomplete; the figures are what is pinned here.
	require.JSONEq(t, `{"month":"TOTAL","symbol":"ATOM","commission_amount":"3","commission_usd":"30","delegator_rewards_amount":"5.25","delegator_rewards_usd":"52.5","reward_events":5,"unique_delegators":3,"price_missing_days":1,"price_missing_events":1,"incomplete":true}`, string(body.Total))

	rec = statementPGRequest(t, s, statementPGPath+statementPGGold+"&start=2025-01-01&end=2026-01-01&format=csv")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	records, err := csv.NewReader(rec.Body).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 4)
	require.Equal(t, []string{statementPGGold, "", "2025-03", apiSymbolATOM, "0", "0.00", "4", "40.00", "2", "2", "0", "0", apiFalse}, records[1])
	require.Equal(t, []string{statementPGGold, "", "2025-04", apiSymbolATOM, "3", "30.00", "1.25", "12.50", "3", "2", "1", "1", apiFalse}, records[2])
	require.Equal(t, []string{statementPGGold, "", apiTotalLabel, apiSymbolATOM, "3", "30.00", "5.25", "52.50", "5", "3", "1", "1", apiTrue}, records[3])

	// All time adds the 2026 month and its delegator.
	rec = statementPGRequest(t, s, statementPGPath+statementPGGold)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var all struct {
		Months []ValidatorStatementMonth `json:"months"`
		Total  ValidatorStatementMonth   `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	require.Len(t, all.Months, 3)
	require.Equal(t, int64(4), all.Total.UniqueDelegators)
	require.Equal(t, int64(6), all.Total.RewardEvents)
	require.Equal(t, "5.75", all.Total.DelegatorRewards.String())

	rec = statementPGRequest(t, s, statementPGPath+statementPGNone)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// A period sized above the row limit is refused before any row is read; a
// request whose time is up is stopped with 504 rather than left running.
func TestValidatorStatementLimitsPostgres(t *testing.T) {
	s := statementPGServer(t)
	path := statementPGPath + statementPGBig

	estimate, err := statementRowEstimate(s.db, statementPGBig, time.Time{}, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.InDelta(t, 5000, estimate, 600, "the planner sizes the validator's reward and commission rows")
	month, err := statementRowEstimate(s.db, statementPGBig, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Less(t, month, estimate/5, "a month is sized far below the whole history")

	s.SetStatementLimits(1000, 0)
	rec := statementPGRequest(t, s, path)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "thousand reward and commission records")
	require.Contains(t, rec.Body.String(), "limit 1 thousand")
	require.Contains(t, rec.Body.String(), "shorter period")
	rec = statementPGRequest(t, s, path+"&format=csv")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "the CSV has the same limit")
	rec = statementPGRequest(t, s, path+"&start=2025-06-01&end=2025-07-01")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	s.SetStatementLimits(-1, 0)
	rec = statementPGRequest(t, s, path)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rec = httptest.NewRecorder()
	s.handleValidatorStatement(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(gone))
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "did not finish within 90 seconds")

	// Every slot busy: the request waits for one and gives up at its limit.
	for i := 0; i < statementBuildSlots; i++ {
		statementSlots <- struct{}{}
	}
	s.SetStatementLimits(-1, 300*time.Millisecond)
	began := time.Now()
	rec = statementPGRequest(t, s, path)
	for i := 0; i < statementBuildSlots; i++ {
		<-statementSlots
	}
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Less(t, time.Since(began), 5*time.Second)
	rec = statementPGRequest(t, s, path)
	require.Equal(t, http.StatusOK, rec.Code, "a slot is free again")
}

func TestStatementTooLargeMessage(t *testing.T) {
	require.Equal(t, "19.8 million", approxCount(19_846_403))
	require.Equal(t, "3 million", approxCount(3_000_000))
	require.Equal(t, "522 thousand", approxCount(521_966))
	require.Equal(t, "17", approxCount(17))
	msg := statementTooLarge{Rows: 19_846_403, Limit: 3_000_000}.Error()
	require.Contains(t, msg, "about 19.8 million reward and commission records")
	require.Contains(t, msg, "limit 3 million")
}
