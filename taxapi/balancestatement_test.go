package taxapi

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestStatementDatesYear(t *testing.T) {
	days, labels, err := statementDates("2026", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-03-31", "2026-06-30", "2026-09-30", "2026-12-31"}
	for i, d := range days {
		if d.Format(apiDateLayout) != want[i] {
			t.Fatalf("day %d = %s, want %s", i, d.Format(apiDateLayout), want[i])
		}
	}
	if labels[3] != "Year end" {
		t.Fatalf("labels = %v", labels)
	}
	if _, _, err := statementDates("", ""); err == nil {
		t.Fatal("a statement needs a year or dates")
	}
	if _, _, err := statementDates("", "2026-02-30"); err == nil {
		t.Fatal("an impossible date must be rejected")
	}
	many := strings.Repeat("2026-01-01,", maxStatementDates) + "2026-01-02"
	if _, _, err := statementDates("", many); err == nil {
		t.Fatal("too many dates must be rejected")
	}
}

func TestBalanceDateRejectsBadInput(t *testing.T) {
	s := &Server{native: DefaultNativeAsset}
	h := s.Handler()
	for _, tc := range []struct{ url, want string }{
		{"/balance?address=cosmos1x&date=2026-13-01", "date must be YYYY-MM-DD"},
		{"/balance?address=notanaddress&date=2026-01-01", "not a valid account address"},
		{"/balance-statement?address=cosmosvaloper1x&year=2026", "valid account address"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: %d %q", tc.url, rec.Code, rec.Body.String())
		}
	}
}

func TestBalanceStatementCSV(t *testing.T) {
	price := decimal.RequireFromString("4.5")
	value := decimal.RequireFromString("9")
	bt := time.Date(2026, 3, 31, 23, 59, 55, 0, time.UTC)
	out := []HistoricalBalance{
		{
			Address: "cosmos1w", Date: "2026-03-31", Label: "Q1 end", Status: balanceApproximate, Height: 30457492, BlockTime: &bt,
			Lines: []BalanceLine{
				{Bucket: "liquid", Denom: "uatom", Symbol: "ATOM", AmountBase: "2000000", Quantity: decimal.NewFromInt(2), PriceUSD: &price, ValueUSD: &value},
				{Bucket: "staked", Validator: "cosmosvaloper1v", Denom: "uatom", Symbol: "ATOM", AmountBase: "1", Quantity: decimal.RequireFromString("0.000001"), PriceUSD: &price, ValueUSD: &value, Approximate: true},
				{Bucket: "liquid", Denom: "ibc/X", Symbol: "=X", AmountBase: "7", Quantity: decimal.NewFromInt(7), PriceMissing: true},
			},
			TotalUSD: decimal.RequireFromString("18"), UnpricedLines: 1,
		},
		{Address: "cosmos1w", Date: "2025-12-31", Label: "Year end", Status: balanceIncompleteS, Reason: "blocks are not indexed"},
	}
	rec := httptest.NewRecorder()
	writeBalanceStatementCSV(rec, out)
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("rows = %d: %v", len(rows), rows)
	}
	if rows[2][13] != "true" || rows[2][6] != "cosmosvaloper1v" {
		t.Fatalf("staked row = %v", rows[2])
	}
	if rows[3][8] != "'=X" || rows[3][12] != "true" || rows[3][11] != "" {
		t.Fatalf("unpriced row must keep its quantity, no value, and a safe symbol: %v", rows[3])
	}
	if rows[4][5] != apiTotal || rows[4][11] != "18.00" {
		t.Fatalf("total row = %v", rows[4])
	}
	if rows[5][3] != balanceIncompleteS || rows[5][9] != "" || rows[5][14] != "blocks are not indexed" {
		t.Fatalf("incomplete row must have no amounts and say why: %v", rows[5])
	}
}
