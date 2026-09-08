package taxapi

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJunoFactoryTraceMatchesArchiveHash(t *testing.T) {
	trace := "transfer/channel-141/transfer/channel-42/" + junoFactoryUatomBase
	if got := fmt.Sprintf("ibc/%X", sha256.Sum256([]byte(trace))); got != junoFactoryUatomVoucher {
		t.Fatal(got)
	}
}
func TestJunoFactoryUatomCannotBePricedOrLabelledAsNativeAtom(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	defer srv.Close()
	o := NewOracle(srv.URL, "")
	s := NewServer(nil, o, DefaultNativeAsset)
	meta := map[string]DenomMeta{junoFactoryUatomVoucher: {Symbol: "ATOM", Decimals: 6}, "uatom": {Symbol: "ATOM", Decimals: 6}}
	row := s.buildRow("mainnet", meta, time.Now(), "factory-tx", "transfer", "in", junoFactoryUatomVoucher, "10000000000", "a", "b")
	if row.Symbol == "ATOM" || row.Symbol != junoFactoryUatomSymbol || !row.PriceMissing || !row.PriceUSD.IsZero() || !row.ValueUSD.IsZero() || !row.DecimalsAssumed || !row.IsIBC || row.Denom != junoFactoryUatomVoucher {
		t.Fatalf("factory token misidentified: %+v", row)
	}
	if calls != 0 {
		t.Fatal("unverified factory asset should not request a price")
	}
	for _, denom := range []string{junoFactoryUatomVoucher, junoFactoryUatomBase, "transfer/channel-141/transfer/channel-42/" + junoFactoryUatomBase} {
		if usd, found := o.PriceAt("mainnet", denom, "2026-05-20"); found || usd != 0 {
			t.Fatal("unverified token price accepted")
		}
	}
	native := s.buildRow("mainnet", meta, time.Now(), "native-tx", "reward", "in", "uatom", "1000000", "", "b")
	if native.Symbol != "ATOM" || native.PriceMissing || !native.PriceUSD.Equal(d(2)) {
		t.Fatalf("native pricing broken: %+v", native)
	}
}
func TestJunoFactoryTransferCannotCreateNativeAtomFifoLot(t *testing.T) {
	s := NewServer(nil, NewOracle("http://127.0.0.1:1", ""), DefaultNativeAsset)
	at := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	incoming := s.buildRow("mainnet", map[string]DenomMeta{junoFactoryUatomVoucher: {Symbol: "ATOM", Decimals: 6}}, at, "factory-tx", "transfer", "in", junoFactoryUatomVoucher, "10000000000", "a", "b")
	outgoing := Row{Time: at.Add(time.Hour), Category: "transfer", Direction: "out", Symbol: "ATOM", Denom: "uatom", Amount: d(1), PriceUSD: d(2), ValueUSD: d(2)}
	lines := Build8949([]Row{incoming, outgoing})
	if len(lines) != 1 || lines[0].DateAcquired != "Various" || !lines[0].BasisUnknown {
		t.Fatalf("factory token contaminated native ATOM lot: %+v", lines)
	}
}
