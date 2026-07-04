package taxapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// stubChainServer answers denoms_metadata for "ustars" (the bank-fallback case)
// and an empty-but-valid JSON body for anything else (harmless for PriceAt,
// which just reads found=false from it).
func stubChainServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "ustars") {
			_, _ = w.Write([]byte(`{"metadata":{"symbol":"STARS","denom_units":[{"denom":"ustars","exponent":0},{"denom":"stars","exponent":6}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBuildRowKnownDenomUsesOracleMeta(t *testing.T) {
	stub := stubChainServer(t)
	s := &Server{oracle: NewOracle(stub.URL, "")}
	meta := map[string]DenomMeta{"uatom": {Symbol: "ATOM", Decimals: 6}}

	row := s.buildRow("mainnet", meta, time.Now(), "hash1", "transfer", "in", "uatom", "5000000", "a", "b")
	if row.DecimalsAssumed {
		t.Fatalf("known denom should not be flagged: %+v", row)
	}
	if row.Symbol != "ATOM" || !row.Amount.Equal(d(5)) {
		t.Fatalf("want ATOM/5, got symbol=%s amount=%s", row.Symbol, row.Amount)
	}
}

func TestBuildRowOracleMissFallsBackToChainBankModule(t *testing.T) {
	stub := stubChainServer(t)
	s := &Server{oracle: NewOracle(stub.URL, stub.URL)}

	row := s.buildRow("mainnet", map[string]DenomMeta{}, time.Now(), "hash2", "transfer", "in", "ustars", "7000000", "a", "b")
	if row.DecimalsAssumed {
		t.Fatalf("chain bank-module fallback should resolve decimals, not flag: %+v", row)
	}
	if row.Symbol != "STARS" || !row.Amount.Equal(d(7)) {
		t.Fatalf("want STARS/7, got symbol=%s amount=%s", row.Symbol, row.Amount)
	}
}

func TestBuildRowBothMissesAssumesAndFlags(t *testing.T) {
	stub := stubChainServer(t)
	// No nodeREST configured, so the bank-module fallback is unavailable too.
	s := &Server{oracle: NewOracle(stub.URL, "")}

	row := s.buildRow("mainnet", map[string]DenomMeta{}, time.Now(), "hash3", "transfer", "in", "factory/cosmos1xyz/mystery", "1234", "a", "b")
	if !row.DecimalsAssumed {
		t.Fatalf("want DecimalsAssumed=true when both oracle and chain miss: %+v", row)
	}
	// The stub also has no price for this denom, so PriceMissing is set too;
	// both warnings should surface.
	if got, want := row.description(), "transfer (decimals unknown, amount may be wrong) (price missing)"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestBuildRowPriceFoundVsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "denom=uatom") {
			_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer srv.Close()
	s := &Server{oracle: NewOracle(srv.URL, "")}
	meta := map[string]DenomMeta{
		"uatom":    {Symbol: "ATOM", Decimals: 6},
		"umystery": {Symbol: "MYST", Decimals: 6},
	}

	priced := s.buildRow("mainnet", meta, time.Now(), "h1", "transfer", "in", "uatom", "5000000", "a", "b")
	if priced.PriceMissing || !priced.ValueUSD.Equal(d(10)) {
		t.Fatalf("known price wrong: %+v", priced)
	}

	unpriced := s.buildRow("mainnet", meta, time.Now(), "h2", "transfer", "in", "umystery", "5000000", "a", "b")
	if !unpriced.PriceMissing || !unpriced.ValueUSD.IsZero() {
		t.Fatalf("missing price should flag and stay 0, not fabricate a value: %+v", unpriced)
	}
	if got, want := unpriced.description(), "transfer (price missing)"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestBuildIncomeCSVWarnsOnMissingPrice(t *testing.T) {
	now := time.Now()
	rows := []Row{
		{Time: now, Category: "reward", Symbol: "ATOM", ValueUSD: d(10)},
		{Time: now, Category: "commission", Symbol: "MYST", ValueUSD: decimal.Zero, PriceMissing: true},
		{Time: now, Category: "transfer", Symbol: "ATOM", ValueUSD: d(999)}, // not income, must be excluded
	}
	csvOut := buildIncomeCSV(rows)
	if !strings.Contains(csvOut, "TOTAL") || strings.Contains(csvOut, "999") {
		t.Fatalf("csv should total only reward/commission rows: %s", csvOut)
	}
	if !strings.Contains(csvOut, "WARNING") || !strings.Contains(csvOut, "MYST") {
		t.Fatalf("csv should warn about the MYST row with no price: %s", csvOut)
	}
}

func TestBuildIncomeCSVNoWarningWhenAllPriced(t *testing.T) {
	rows := []Row{{Time: time.Now(), Category: "reward", Symbol: "ATOM", ValueUSD: d(5)}}
	csvOut := buildIncomeCSV(rows)
	if strings.Contains(csvOut, "WARNING") {
		t.Fatalf("no warning expected when every row has a price: %s", csvOut)
	}
}

func TestSumUBTICountsMissingPrices(t *testing.T) {
	rows := []Row{
		{Category: "reward", ValueUSD: d(3)},
		{Category: "commission", ValueUSD: decimal.Zero, PriceMissing: true},
		{Category: "transfer", ValueUSD: d(100)}, // not UBTI
	}
	ubti, missing := sumUBTI(rows)
	if !ubti.Equal(d(3)) || missing != 1 {
		t.Fatalf("want ubti=3 missing=1, got ubti=%s missing=%d", ubti, missing)
	}
}

func TestIBCRowWithUnknownDecimalsFlagsBoth(t *testing.T) {
	stub := stubChainServer(t)
	s := &Server{oracle: NewOracle(stub.URL, "")}

	row := s.buildRow("mainnet", map[string]DenomMeta{}, time.Now(), "hash4", "transfer", "in", "transfer/channel-0/mystery", "1234", "a", "b")
	if !row.DecimalsAssumed || !row.IsIBC || !row.PriceMissing {
		t.Fatalf("want IsIBC, DecimalsAssumed and PriceMissing all set: %+v", row)
	}
	want := "ibc transfer/channel-0/mystery (decimals unknown, amount may be wrong) (price missing)"
	if got := row.description(); got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}
