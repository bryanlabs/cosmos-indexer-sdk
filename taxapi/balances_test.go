package taxapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getNativeHoldings must work for any chain's native asset, not just ATOM
// (INF-208) — this simulates a 6-decimal non-ATOM denom (e.g. Noble's uusdc).
func TestGetNativeHoldingsNonAtomDenom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/cosmos/bank/"):
			_, _ = w.Write([]byte(`{"balances":[{"denom":"uusdc","amount":"5000000"}]}`))
		case strings.Contains(r.URL.Path, "/cosmos/staking/"):
			_, _ = w.Write([]byte(`{"delegation_responses":[]}`))
		case strings.Contains(r.URL.Path, "/cosmos/distribution/"):
			_, _ = w.Write([]byte(`{"total":[]}`))
		}
	}))
	defer srv.Close()

	h, err := getNativeHoldings(srv.URL, "noble1abc", NativeAsset{Denom: "uusdc", Decimals: 6, Symbol: "USDC"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.Liquid != 5.0 {
		t.Fatalf("want 5.0 USDC liquid, got %v", h.Liquid)
	}
}

// A chain with no staking module (e.g. Noble) 404s the delegation/rewards
// queries; that must not blank out the liquid balance that DID resolve.
func TestGetNativeHoldingsGracefulWithoutStakingModule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/cosmos/bank/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"balances":[{"denom":"uusdc","amount":"1000000"}]}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	h, err := getNativeHoldings(srv.URL, "noble1abc", NativeAsset{Denom: "uusdc", Decimals: 6, Symbol: "USDC"})
	if err != nil {
		t.Fatalf("a missing staking module should not fail the whole lookup: %v", err)
	}
	if h.Liquid != 1.0 || h.Staked != 0 || h.Reward != 0 {
		t.Fatalf("want liquid=1, staked=0, reward=0, got %+v", h)
	}
}
