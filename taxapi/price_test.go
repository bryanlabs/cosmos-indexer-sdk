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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"symbol":"STARS","denom_units":[{"denom":"ustars","exponent":0},{"denom":"stars","exponent":6}]}}`))
	}))
	defer srv.Close()

	o := NewOracle("http://unused-in-this-test", srv.URL)
	m, ok := o.BankMetaFallback("ustars")
	if !ok || m.Symbol != "STARS" || m.Decimals != 6 {
		t.Fatalf("want STARS/6, got %+v ok=%v", m, ok)
	}
	if _, ok := o.BankMetaFallback("ustars"); !ok {
		t.Fatal("second call should still resolve (from cache)")
	}
	if hits != 1 {
		t.Fatalf("want 1 http call (second lookup cached), got %d", hits)
	}
}

func TestBankMetaFallbackUnknownDenom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	o := NewOracle("http://unused-in-this-test", srv.URL)
	if _, ok := o.BankMetaFallback("factory/cosmos1xyz/unknowndenom"); ok {
		t.Fatal("want ok=false for a denom the chain doesn't recognize either")
	}
}

func TestBankMetaFallbackDisabledWithoutNodeREST(t *testing.T) {
	o := NewOracle("http://unused-in-this-test", "")
	if _, ok := o.BankMetaFallback("ustars"); ok {
		t.Fatal("want ok=false when nodeREST is empty (fallback disabled)")
	}
}
