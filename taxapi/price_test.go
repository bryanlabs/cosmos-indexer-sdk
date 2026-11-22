package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBankMetaFallbackResolvesAndCaches(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"metadata":{"symbol":"STARS","denom_units":[{"denom":"ustars","exponent":0},{"denom":"stars","exponent":6}]}}`))
	}))
	defer srv.Close()

	o := NewOracle(apiTestUnusedOrigin, srv.URL)
	m, ok := o.BankMetaFallback(apiUstars)
	if !ok || m.Symbol != "STARS" || m.Decimals != 6 {
		t.Fatalf("want STARS/6, got %+v ok=%v", m, ok)
	}
	if _, ok := o.BankMetaFallback(apiUstars); !ok {
		t.Fatal("second call should still resolve (from cache)")
	}
	if hits != 1 {
		t.Fatalf("want 1 http call (second lookup cached), got %d", hits)
	}
}

func TestBankMetaFallbackUnknownDenom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, apiNotFound, http.StatusNotFound)
	}))
	defer srv.Close()

	o := NewOracle(apiTestUnusedOrigin, srv.URL)
	if _, ok := o.BankMetaFallback("factory/cosmos1xyz/unknowndenom"); ok {
		t.Fatal("want ok=false for a denom the chain doesn't recognize either")
	}
}

func TestBankMetaFallbackDisabledWithoutNodeREST(t *testing.T) {
	o := NewOracle(apiTestUnusedOrigin, "")
	if _, ok := o.BankMetaFallback(apiUstars); ok {
		t.Fatal("want ok=false when nodeREST is empty (fallback disabled)")
	}
}

func TestPriceAtCachesFoundIndefinitely(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(apiContentType, apiJSONContentType)
		_ = json.NewEncoder(w).Encode(map[string]any{"usd": 1.95, "found": true, "date": r.URL.Query().Get(apiDate)})
	}))
	defer srv.Close()

	o := NewOracle(srv.URL, "")
	usd, found := o.PriceAt(apiMainnet, apiUatom, apiTestPeriodStart)
	if !found || usd != 1.95 {
		t.Fatalf("want 1.95/true, got %v/%v", usd, found)
	}
	if _, _ = o.PriceAt(apiMainnet, apiUatom, apiTestPeriodStart); hits != 1 {
		t.Fatalf("want 1 http call (second lookup cached), got %d", hits)
	}
}

const (
	priceTestOldDay       = "2025-01-01"
	priceTestRequestedDay = "2025-01-02"
	priceTestPeg          = "peg"
	priceTestFallback     = "fallback"
)

func TestPriceSettled(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, requested, returned string
		want                      bool
	}{
		{"old exact", "2026-11-05", "2026-11-05", true},
		{"recent exact", "2026-11-06", "2026-11-06", false},
		{priceTestFallback, priceTestRequestedDay, priceTestOldDay, false},
		{"missing date", priceTestRequestedDay, "", false},
		{"invalid", "bad", "bad", false},
		{"empty", "", "", false},
		{"future", "2027-01-01", "2027-01-01", false},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, priceSettled(tc.requested, tc.returned, now)) })
	}
}

func TestPriceAtRefetchesUnsettled(t *testing.T) {
	for _, kind := range []string{"recent", priceTestFallback, priceTestPeg, "miss", "settled"} {
		t.Run(kind, func(t *testing.T) {
			hits := 0
			date, returned := priceTestRequestedDay, priceTestRequestedDay
			denom := apiUatom
			switch kind {
			case "recent":
				date = time.Now().UTC().Format(apiDateLayout)
				returned = date
			case priceTestFallback:
				returned = priceTestOldDay
			case priceTestPeg:
				denom = cosmosHubNobleUSDCDenom
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set(apiContentType, apiJSONContentType)
				if hits == 1 && (kind == priceTestPeg || kind == "miss") {
					_, _ = w.Write([]byte(`{"found":false}`))
					return
				}
				price := "1.95"
				if hits > 1 {
					price = "2.25"
				}
				_, _ = w.Write([]byte(`{"usd":` + price + `,"found":true,"date":"` + returned + `"}`))
			}))
			defer srv.Close()
			o := NewOracle(srv.URL, "")
			_, _ = o.PriceAt(apiMainnet, denom, date)
			_, _ = o.PriceAt(apiMainnet, denom, date)
			require.Equal(t, 1, hits)
			key := apiMainnet + "|" + denom + "|" + date
			o.mu.Lock()
			e := o.prices[key]
			e.cached = time.Now().Add(-priceUnsettledTTL - time.Second)
			o.prices[key] = e
			o.mu.Unlock()
			usd, found := o.PriceAt(apiMainnet, denom, date)
			require.True(t, found)
			if kind == "settled" {
				require.Equal(t, 1, hits)
				require.Equal(t, 1.95, usd)
			} else {
				require.Equal(t, 2, hits)
				require.Equal(t, 2.25, usd)
			}
		})
	}
}

func TestPriceAtUsesPinnedUSDCPriceWhenOracleMisses(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer srv.Close()

	o := NewOracle(srv.URL, "")
	usd, found := o.PriceAt(apiMainnet, cosmosHubNobleUSDCDenom, apiTestReceiptDate)
	if !found || usd != 1 {
		t.Fatalf("want pinned USDC price 1/true, got %v/%v", usd, found)
	}
	if _, _ = o.PriceAt(apiMainnet, cosmosHubNobleUSDCDenom, apiTestReceiptDate); hits != 1 {
		t.Fatalf("want pinned price cached after one oracle attempt, got %d calls", hits)
	}
}

func TestPriceAtDoesNotTrustUSDCNameForUnknownDenom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer srv.Close()

	o := NewOracle(srv.URL, "")
	if _, found := o.PriceAt(apiMainnet, "factory/cosmos1issuer/usdc", apiTestReceiptDate); found {
		t.Fatal("an unknown token named USDC must remain unpriced")
	}
}

func TestPriceAtPrefersOraclePriceOverPinnedUSDCPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":0.9975,"found":true}`))
	}))
	defer srv.Close()

	o := NewOracle(srv.URL, "")
	usd, found := o.PriceAt(apiMainnet, cosmosHubNobleUSDCDenom, apiTestReceiptDate)
	if !found || usd != 0.9975 {
		t.Fatalf("want oracle price 0.9975/true, got %v/%v", usd, found)
	}
}

func TestPriceAtCachesMissBriefly(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer srv.Close()

	o := NewOracle(srv.URL, "")
	if _, found := o.PriceAt(apiMainnet, "mystery", apiTestPeriodStart); found {
		t.Fatal("want found=false")
	}
	if _, found := o.PriceAt(apiMainnet, "mystery", apiTestPeriodStart); found || hits != 1 {
		t.Fatalf("want the miss cached (1 http call, still not found), got hits=%d found=%v", hits, found)
	}
}

func TestDenomTraceResolvesAndCaches(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"denom_trace":{"path":"transfer/channel-141","base_denom":"uosmo"}}`))
	}))
	defer srv.Close()

	o := NewOracle(apiTestUnusedOrigin, srv.URL)
	path, ok := o.DenomTrace(apiTestTraceHash)
	if !ok || path != "transfer/channel-141/uosmo" {
		t.Fatalf("want transfer/channel-141/uosmo, got %q ok=%v", path, ok)
	}
	if _, ok := o.DenomTrace(apiTestTraceHash); !ok || hits != 1 {
		t.Fatalf("second lookup should be cached (1 http call), got hits=%d", hits)
	}
}

func TestDenomTraceUnknownHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, apiNotFound, http.StatusNotFound)
	}))
	defer srv.Close()

	o := NewOracle(apiTestUnusedOrigin, srv.URL)
	if _, ok := o.DenomTrace("DEADBEEF"); ok {
		t.Fatal("want ok=false for a hash the chain doesn't recognize")
	}
}

// A gateway that can never resolve traces (e.g. a REST node returning 501
// Not Implemented for the whole denom_traces endpoint) must not be re-hit on
// every row of a report using that hash: a real cosmoshub wallet with ~2300
// rows across ~20 distinct never-resolving hashes turned a report from
// milliseconds into tens of seconds before misses were cached.
func TestDenomTraceUnresolvedHashIsCachedNotRetriedPerRow(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"code":12,"message":"Not Implemented","details":[]}`))
	}))
	defer srv.Close()

	o := NewOracle(apiTestUnusedOrigin, srv.URL)
	for i := 0; i < 50; i++ {
		if _, ok := o.DenomTrace("SAME_UNRESOLVABLE_HASH"); ok {
			t.Fatal("want ok=false for a hash the gateway can't resolve")
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly 1 http call for 50 lookups of the same unresolvable hash, got %d", hits)
	}
}

func TestDenomTraceDisabledWithoutNodeREST(t *testing.T) {
	o := NewOracle(apiTestUnusedOrigin, "")
	if _, ok := o.DenomTrace(apiTestTraceHash); ok {
		t.Fatal("want ok=false when nodeREST is empty (fallback disabled)")
	}
}

func TestPriceAtUnreachableOracleIsNotFoundNotFabricated(t *testing.T) {
	// Nothing listening on this port: PriceAt must return found=false, not panic
	// or fabricate a price.
	o := NewOracle("http://127.0.0.1:1", "")
	usd, found := o.PriceAt(apiMainnet, apiUatom, apiTestPeriodStart)
	if found || usd != 0 {
		t.Fatalf("want 0/false on an unreachable oracle, got %v/%v", usd, found)
	}
}
