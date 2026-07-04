package taxapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	if got, want := row.description(), "transfer (decimals unknown, amount may be wrong)"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestIBCRowWithUnknownDecimalsFlagsBoth(t *testing.T) {
	stub := stubChainServer(t)
	s := &Server{oracle: NewOracle(stub.URL, "")}

	row := s.buildRow("mainnet", map[string]DenomMeta{}, time.Now(), "hash4", "transfer", "in", "transfer/channel-0/mystery", "1234", "a", "b")
	if !row.DecimalsAssumed || !row.IsIBC {
		t.Fatalf("want both IsIBC and DecimalsAssumed set: %+v", row)
	}
	if got, want := row.description(), "ibc transfer/channel-0/mystery (decimals unknown, amount may be wrong)"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}
