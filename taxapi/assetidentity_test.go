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

func TestJunoFactoryRetainsReportedLabelPendingChoice(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	defer srv.Close()
	o := NewOracle(srv.URL, "")
	s := NewServer(nil, o, DefaultNativeAsset)
	meta := map[string]DenomMeta{junoFactoryUatomVoucher: {Symbol: apiSymbolATOM, Decimals: 6}, apiUatom: {Symbol: apiSymbolATOM, Decimals: 6}}
	row := s.buildRow(apiMainnet, meta, time.Now(), "factory-tx", apiTransfer, "in", junoFactoryUatomVoucher, "10000000000", "a", "b")
	if row.Symbol != apiSymbolATOM || row.AssetDecision != nil || row.Excluded || !row.PriceMissing || !row.PriceUSD.IsZero() || !row.ValueUSD.IsZero() || row.DecimalsAssumed || !row.IsIBC || row.Denom != junoFactoryUatomVoucher {
		t.Fatalf("factory token misidentified: %+v", row)
	}
	if row.AssetIdentity == nil || row.AssetIdentity.SourceChain != apiJunoChainID || row.AssetIdentity.TokenName != junoFactoryUatomSymbol || row.AssetIdentity.RawAmount != "10000000000" || row.AssetIdentity.OfficialAtomMatch {
		t.Fatalf("trace evidence missing: %+v", row.AssetIdentity)
	}
	if calls != 0 {
		t.Fatal("unverified factory asset should not request a price")
	}
	for _, denom := range []string{junoFactoryUatomVoucher, junoFactoryUatomBase, "transfer/channel-141/transfer/channel-42/" + junoFactoryUatomBase} {
		if usd, found := o.PriceAt(apiMainnet, denom, "2026-05-20"); found || usd != 0 {
			t.Fatal("unverified token price accepted")
		}
	}
	native := s.buildRow(apiMainnet, meta, time.Now(), "native-tx", apiReward, "in", apiUatom, "1000000", "", "b")
	if native.Symbol != apiSymbolATOM || native.PriceMissing || !native.PriceUSD.Equal(d(2)) {
		t.Fatalf("native pricing broken: %+v", native)
	}
}

func TestNonCanonicalAtomLabelRequiresReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	defer srv.Close()
	s := NewServer(nil, NewOracle(srv.URL, ""), DefaultNativeAsset)
	for _, denom := range []string{"ibc/OTHER_ATOM_LABEL", "transfer/channel-141/transfer/channel-9/uatom"} {
		row := s.buildRow(apiMainnet, map[string]DenomMeta{denom: {Symbol: apiSymbolATOM, Decimals: 6}, apiUatom: {Symbol: apiSymbolATOM, Decimals: 6}}, time.Now(), "tx", apiTransfer, "in", denom, "1000000", "a", "b")
		if row.Symbol != apiSymbolATOM || row.AssetDecision != nil || row.Excluded || !row.PriceMissing || row.AssetIdentity == nil || row.AssetIdentity.OfficialAtomMatch {
			t.Fatalf("non-native ATOM priced by name: %+v", row)
		}
	}
}

func TestJunoFactoryTransferCannotCreateNativeAtomFifoLot(t *testing.T) {
	s := NewServer(nil, NewOracle("http://127.0.0.1:1", ""), DefaultNativeAsset)
	at := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	incoming := s.buildRow(apiMainnet, map[string]DenomMeta{junoFactoryUatomVoucher: {Symbol: apiSymbolATOM, Decimals: 6}}, at, "factory-tx", apiTransfer, "in", junoFactoryUatomVoucher, "10000000000", "a", "b")
	outgoing := Row{Time: at.Add(time.Hour), Category: apiTransfer, Direction: apiOut, Symbol: apiSymbolATOM, Denom: apiUatom, Amount: d(1), PriceUSD: d(2), ValueUSD: d(2)}
	lines := Build8949([]Row{incoming, outgoing})
	if len(lines) != 1 || lines[0].DateAcquired != apiVariousDates || !lines[0].BasisUnknown {
		t.Fatalf("factory token contaminated native ATOM lot: %+v", lines)
	}
}
