package taxapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		_, _ = w.Write([]byte(`{"usd":1.95,"found":true}`))
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
