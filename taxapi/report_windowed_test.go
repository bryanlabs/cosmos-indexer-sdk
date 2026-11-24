package taxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// windowedApply applies decisions to rows cut at the given indexes, one
// decisionMatch across the pieces, as reviewedWindows does.
func windowedApply(rows []Row, decisions AssetDecisions, cuts ...int) ([]Row, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return nil, err
	}
	match := newDecisionMatch()
	var out []Row
	from := 0
	for _, to := range append(cuts, len(rows)) {
		applied, err := match.apply(rows[from:to], decisions)
		if err != nil {
			return nil, err
		}
		out = append(out, applied...)
		from = to
	}
	if err := match.check(decisions); err != nil {
		return nil, err
	}
	return out, nil
}

func decisionSplitRows() []Row {
	flagged := Row{Time: time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: "in", Symbol: apiSymbolATOM, Amount: d(10000), PriceMissing: true,
		AssetIdentity: &AssetIdentity{RecordID: "mainnet|w|9|0:cc", RawAmount: "10000000000", RawDenom: junoFactoryUatomVoucher}}
	flagged.SemanticRecordID = flagged.AssetIdentity.RecordID
	other := flagged
	other.AssetIdentity = &AssetIdentity{RecordID: "mainnet|w|10|0:dd", RawAmount: "5000000", RawDenom: junoFactoryUatomVoucher}
	other.SemanticRecordID = other.AssetIdentity.RecordID
	other.Amount = d(5)
	return []Row{
		semanticRow(testSemanticKeyOut, apiOut, "2", true),
		flagged,
		semanticRow(testSemanticKeyIn, "in", "4", false),
		other,
	}
}

// Applying decisions window by window answers exactly as applying them to
// the whole report: the same rows, and the same refusals, wherever the cuts
// fall.
func TestDecisionMatchWindowsAnswerAsWhole(t *testing.T) {
	rows := decisionSplitRows()
	cases := map[string]AssetDecisions{
		"none":                   {},
		"semantic":               {testSemanticKeyOut: {Classification: ClassProtocolDeposit, ValueUSD: "99"}, testSemanticKeyIn: {Classification: ClassIncome, ValueUSD: "8"}},
		"record":                 {"mainnet|w|9|0:cc": {Mode: AssetDecisionExclude}},
		"denom":                  {"denom:" + junoFactoryUatomVoucher: {Mode: AssetDecisionExclude}},
		"stale":                  {"mainnet|w|404|0:ee": {Mode: AssetDecisionExclude}},
		"stale semantic":         {"mainnet|w|404|0:semantic:ff": {Classification: ClassIncome}},
		"wrong direction":        {testSemanticKeyOut: {Classification: ClassIncome}},
		"denom matching nothing": {"denom:uother": {Mode: AssetDecisionExclude}},
	}
	for name, decisions := range cases {
		whole, wholeErr := ApplyAssetDecisions(rows, decisions)
		for _, cuts := range [][]int{nil, {1}, {2}, {3}, {1, 2, 3}, {0, 4}} {
			got, err := windowedApply(rows, decisions, cuts...)
			if wholeErr != nil {
				require.Error(t, err, "%s cut at %v", name, cuts)
				require.Equal(t, wholeErr.Error(), err.Error(), "%s cut at %v", name, cuts)
				continue
			}
			require.NoError(t, err, "%s cut at %v", name, cuts)
			want, _ := json.Marshal(whole)
			have, _ := json.Marshal(got)
			require.JSONEq(t, string(want), string(have), "%s cut at %v", name, cuts)
			used := map[string]bool{}
			markDecisionKeysUsed(got, used)
			require.Equal(t, UnmatchedDecisionKeys(whole, decisions), unmatchedDecisionKeys(used, decisions), "%s cut at %v", name, cuts)
		}
	}
}

// A semantic record ID met in two windows is the duplicate it would be in
// one report.
func TestDecisionMatchDuplicateAcrossWindows(t *testing.T) {
	rows := []Row{semanticRow(testSemanticKeyOut, apiOut, "2", true), semanticRow(testSemanticKeyOut, apiOut, "2", true)}
	_, wholeErr := ApplyAssetDecisions(rows, AssetDecisions{})
	require.Error(t, wholeErr)
	_, err := windowedApply(rows, AssetDecisions{}, 1)
	require.EqualError(t, err, wholeErr.Error())
}

// The windowed answer's readiness counts unknown fee outcomes over every
// row, not only the rows it keeps.
func TestWindowedReviewReadinessCountsEveryUnknownFee(t *testing.T) {
	review := windowedReview{Rows: decisionSplitRows(), FeeOutcomeUnknown: 3}
	all := append(decisionSplitRows(), Row{Category: "fee", Direction: apiOut, Amount: decimal.Zero, FeeOutcomeUnknown: true}, Row{Category: "fee", Direction: apiOut, Amount: decimal.Zero, FeeOutcomeUnknown: true}, Row{Category: "fee", Direction: apiOut, Amount: decimal.Zero, FeeOutcomeUnknown: true})
	require.Equal(t, readinessForRows(all), review.readiness())
	for _, row := range all[len(all)-3:] {
		require.False(t, keptForReview(row))
	}
}

func TestReportWindowsPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for the native Postgres report window check")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)
	for _, sql := range []string{
		tempSearchPath,
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, from_addr text, to_addr text, timestamp timestamptz)",
		"CREATE INDEX ON taxable_events (from_addr)",
		"CREATE INDEX ON taxable_events (to_addr)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	day := func(n int, hour int) time.Time { return time.Date(2024, 3, n, hour, 0, 0, 0, time.UTC) }
	insert := func(from, to string, at time.Time, n int) {
		for i := 0; i < n; i++ {
			require.NoError(t, database.Exec("INSERT INTO taxable_events (from_addr, to_addr, timestamp) VALUES (?, ?, ?)", from, to, at).Error)
		}
	}
	// Wallet a, 10 rows: 2 on Mar 1, a self-transfer and 1 more on Mar 2, 5
	// across three block times on Mar 3, 1 on Mar 5. Wallet b: 2 rows on
	// Mar 2.
	insert("a", "x", day(1, 1), 2)
	insert("a", "a", day(2, 1), 1)
	insert("x", "a", day(2, 2), 1)
	insert("a", "x", day(3, 1), 2)
	insert("x", "a", day(3, 2), 2)
	insert("a", "y", day(3, 3), 1)
	insert("x", "a", day(5, 1), 1)
	insert("b", "x", day(2, 3), 2)
	start, end := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{db: database}

	got, err := s.reportWindows([]string{"a"}, start, end, 4)
	require.NoError(t, err)
	require.Equal(t, []reportWindow{{start, day(3, 0)}, {day(3, 0), day(3, 3)}, {day(3, 3), end}}, got)

	// Days that fit together share a window; no cut falls inside a day that fits.
	got, err = s.reportWindows([]string{"a"}, start, end, 10)
	require.NoError(t, err)
	require.Equal(t, []reportWindow{{start, end}}, got)

	// The cap off is one window, today's path.
	got, err = s.reportWindows([]string{"a"}, start, end, 0)
	require.NoError(t, err)
	require.Equal(t, []reportWindow{{start, end}}, got)

	// Wallets in one report share each window's rows.
	got, err = s.reportWindows([]string{"a", "b"}, start, end, 5)
	require.NoError(t, err)
	require.Equal(t, []reportWindow{{start, day(2, 0)}, {day(2, 0), day(3, 0)}, {day(3, 0), day(5, 0)}, {day(5, 0), end}}, got)

	// A period that starts inside a day over the limit cuts from its start.
	got, err = s.reportWindows([]string{"a"}, day(3, 2), end, 2)
	require.NoError(t, err)
	require.Equal(t, []reportWindow{{day(3, 2), day(3, 3)}, {day(3, 3), end}}, got)

	// One block time over the limit cannot be cut and is too large.
	var tooLarge reportTooLarge
	_, err = s.reportWindows([]string{"a"}, start, end, 1)
	require.True(t, errors.As(err, &tooLarge))
	require.Equal(t, int64(1), tooLarge.Limit)

	// The windows hold every row of the period exactly once.
	got, err = s.reportWindows([]string{"a", "b"}, start, end, 3)
	require.NoError(t, err)
	total := int64(0)
	for i, w := range got {
		if i > 0 {
			require.Equal(t, got[i-1].End, w.Start)
		}
		var n int64
		require.NoError(t, database.Raw("SELECT count(*) FROM taxable_events WHERE (from_addr = 'a' OR to_addr = 'a') AND timestamp >= ? AND timestamp < ?", w.Start, w.End).Scan(&n).Error)
		var m int64
		require.NoError(t, database.Raw("SELECT count(*) FROM taxable_events WHERE (from_addr = 'b' OR to_addr = 'b') AND timestamp >= ? AND timestamp < ?", w.Start, w.End).Scan(&m).Error)
		n += m
		require.LessOrEqual(t, n, int64(3))
		total += n
	}
	require.Equal(t, int64(12), total)
	require.Equal(t, start, got[0].Start)
	require.Equal(t, end, got[len(got)-1].End)
}

// /asset-review over the row cap reads the period in windows and answers
// byte for byte as the same report read whole: the receipts, the groups,
// the readiness, the matched and unmatched decisions, and the refusals.
func TestAssetReviewWindowedMatchesWhole(t *testing.T) {
	s, db, wallet, id := assetReviewFixture(t)
	// Four more receipts, each its own transaction on its own day, and one
	// plain ATOM receipt that needs no review.
	for i := 2; i <= 6; i++ {
		at := time.Date(2026, 5, 20+i, 8, 0, 0, 0, time.UTC)
		denom, memo := junoFactoryUatomVoucher, "Claim 10,000 ATOM at https://claim.invalid"
		if i == 6 {
			denom, memo = apiUatom, ""
		}
		hash := fmt.Sprintf("TX%d", i)
		for _, q := range []string{
			fmt.Sprintf("INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(%d,1,1,%d,'%s',true)", i, 31197048+i, at.Format(time.RFC3339)),
			fmt.Sprintf("INSERT INTO txes(id,block_id,hash,code,memo) VALUES(%d,%d,'%s',0,'%s')", i, i, hash, memo),
			fmt.Sprintf("INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(%d,%d,1,0)", i, i),
		} {
			require.NoError(t, db.Exec(q).Error, q)
		}
		event := tax.TaxableEvent{MessageID: uint(i), Category: apiTransfer, FromAddr: "sender", ToAddr: wallet, Amount: fmt.Sprint(1000000 * i), Denom: denom, BlockHeight: int64(31197048 + i), Timestamp: at, TxHash: hash}
		require.NoError(t, db.Omit(apiMessage).Create(&event).Error)
	}
	answer := func(rowCap int64, decisions AssetDecisions) (int, string) {
		s.SetReportRowCap(rowCap)
		rr := callAsset(t, s, apiAssetReviewPath, wallet, decisions)
		return rr.Code, rr.Body.String()
	}
	for name, decisions := range map[string]AssetDecisions{
		"none":         nil,
		"record":       {id: {Mode: AssetDecisionExclude}},
		"denom":        {"denom:" + junoFactoryUatomVoucher: {Mode: AssetDecisionExclude}},
		"stale":        {"mainnet|" + wallet + "|404|0:ee": {Mode: AssetDecisionExclude}},
		"unused denom": {"denom:uother": {Mode: AssetDecisionExclude}},
	} {
		wholeCode, whole := answer(0, decisions)
		require.NotEmpty(t, whole)
		for _, rowCap := range []int64{1, 2, 3, 5} {
			windows, err := s.reportWindows([]string{wallet}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), rowCap)
			require.NoError(t, err)
			require.Greater(t, len(windows), 1, "%s at cap %d must read in windows", name, rowCap)
			code, body := answer(rowCap, decisions)
			require.Equal(t, wholeCode, code, "%s at cap %d", name, rowCap)
			require.Equal(t, whole, body, "%s at cap %d", name, rowCap)
		}
	}
	// At the cap the period is read whole, as before.
	_, whole := answer(0, nil)
	_, atCap := answer(6, nil)
	require.Equal(t, whole, atCap)

	// An invalid decision set is refused with 400 either way, naming a
	// decision that is invalid and carrying no report rows. Which invalid
	// decision a windowed read names first may differ from the whole read.
	future := AssetDecision{Mode: AssetDecisionOverride, Token: apiTestOverrideToken, Quantity: "2", ValueUSD: "20", CostBasisUSD: "6", AcquiredDate: "2026-12-31"}
	stale := "mainnet|" + wallet + "|404|0:ee"
	for name, decisions := range map[string]AssetDecisions{
		"acquired after receipt":           {id: future},
		"acquired after receipt and stale": {id: future, stale: {Mode: AssetDecisionExclude}},
	} {
		wholeCode, whole := answer(0, decisions)
		require.Equal(t, http.StatusBadRequest, wholeCode, name)
		for _, rowCap := range []int64{1, 2, 3, 5} {
			code, body := answer(rowCap, decisions)
			require.Equal(t, http.StatusBadRequest, code, "%s at cap %d", name, rowCap)
			require.NotContains(t, body, "{", "%s at cap %d", name, rowCap)
			require.True(t, strings.Contains(body, "acquisition date cannot be after the receipt date") || strings.Contains(body, stale), "%s at cap %d: %s", name, rowCap, body)
			if len(decisions) == 1 {
				require.Equal(t, whole, body, "%s at cap %d", name, rowCap)
			}
		}
	}

	// A blocked report names what blocks the whole period, not the first
	// window: one undecoded message on each of two days.
	for _, q := range []string{
		"INSERT INTO message_parser_errors(message_id,error) VALUES(2,'bad')",
		"INSERT INTO message_parser_errors(message_id,error) VALUES(4,'bad')",
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
	blockedCode, blocked := answer(0, nil)
	require.Equal(t, http.StatusConflict, blockedCode)
	for _, rowCap := range []int64{1, 2, 3, 5} {
		code, body := answer(rowCap, nil)
		require.Equal(t, blockedCode, code, "blocked at cap %d", rowCap)
		require.Equal(t, blocked, body, "blocked at cap %d", rowCap)
	}
}
