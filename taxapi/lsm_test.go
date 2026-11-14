package taxapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const lsmShareDenom = "cosmosvaloper1qwl879nx9t6kef4supyazayf7vjhennyh568ys/17"

func TestLSMShareRowsValuedInBondedToken(t *testing.T) {
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == "/denoms" {
			fmt.Fprint(w, `{"denoms":{"uatom":{"symbol":"ATOM","decimals":6}}}`)
			return
		}
		if r.URL.Query().Get("denom") == "uatom" {
			fmt.Fprint(w, `{"usd":5,"found":true}`)
			return
		}
		fmt.Fprint(w, `{"found":false}`)
	}))
	t.Cleanup(oracle.Close)
	s := NewServer(nil, NewOracle(oracle.URL, ""), DefaultNativeAsset)
	wallet := "cosmos1wallet"
	ts := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	rows := s.priceWalletRows(apiMainnet, wallet, walletRows{Events: []tax.TaxableEvent{
		{Category: "tokenize_shares", FromAddr: wallet, ToAddr: wallet, Amount: "820000000", Denom: lsmShareDenom, Timestamp: ts, TxHash: "T", Detail: `{"bonded_amount":"819000000uatom","share_record_id":"7"}`},
		{Category: "redeem_shares", FromAddr: wallet, ToAddr: wallet, Amount: "30500000", Denom: lsmShareDenom, Timestamp: ts, TxHash: "R", Detail: `{"redeemed_amount":"30400000uatom"}`},
		{Category: "redeem_shares", FromAddr: wallet, ToAddr: wallet, Amount: "1000000", Denom: lsmShareDenom, Timestamp: ts, TxHash: "N"},
	}})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, want := range []struct{ amount, value string }{{"819", "4095"}, {"30.4", "152"}} {
		r := rows[i]
		if r.Symbol != apiSymbolATOM || r.Denom != apiUatom || r.Asset != lsmShareDenom || r.PriceMissing {
			t.Fatalf("%s: symbol=%s denom=%s asset=%s missing=%v", r.Category, r.Symbol, r.Denom, r.Asset, r.PriceMissing)
		}
		if r.Amount.String() != want.amount || r.ValueUSD.String() != want.value {
			t.Fatalf("%s: amount=%s value=%s, want %s and %s", r.Category, r.Amount, r.ValueUSD, want.amount, want.value)
		}
	}
	if r := rows[2]; r.Denom != lsmShareDenom || !r.PriceMissing {
		t.Fatalf("redeem without detail: denom=%s missing=%v, want share denom with no price", r.Denom, r.PriceMissing)
	}
	accounts := map[string]bool{}
	for _, l := range BuildJournal(rows[:1], nil) {
		accounts[l.Account] = true
	}
	if !accounts[acctLiquidShares+": ATOM"] || !accounts[acctStakedAssets+": ATOM"] {
		t.Fatalf("tokenize journal accounts = %v, want liquid shares and staked in ATOM", accounts)
	}
}

func TestLSMUnderlyingIgnoresOtherRows(t *testing.T) {
	for _, c := range []struct{ category, denom, detail string }{
		{"delegate", "uatom", `{"bonded_amount":"1uatom"}`},
		{"tokenize_shares", "uatom", `{"bonded_amount":"1uatom"}`},
		{"tokenize_shares", lsmShareDenom, `{"share_record_id":"7"}`},
		{"redeem_shares", lsmShareDenom, `{"redeemed_amount":"5` + lsmShareDenom + `"}`},
		{"redeem_shares", lsmShareDenom, `not json`},
	} {
		if d, a, ok := lsmUnderlying(c.category, c.denom, c.detail); ok {
			t.Fatalf("%s %s %s: got %s %s", c.category, c.denom, c.detail, a, d)
		}
	}
}
