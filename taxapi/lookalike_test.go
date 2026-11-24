package taxapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

// lookalikeExportFormats are every tax-software export WriteCSV renders.
var lookalikeExportFormats = []string{apiKoinly, apiCointracker, apiCoinledger, apiSumm, apiCryptotaxcalculator, apiCryptio, apiBitwave, apiGeneric}

// loadOracleDenomsFixture is the oracle's /denoms for mainnet and provider on
// 2026-11-24, reduced to the fields the API reads, with each registry asset's
// base from the oracle's denom traces (testdata/oracle-denoms-20261124.json).
func loadOracleDenomsFixture(t *testing.T) map[string]map[string]DenomMeta {
	t.Helper()
	raw, err := os.ReadFile("testdata/oracle-denoms-20261124.json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Denoms map[string]map[string]struct {
			Symbol      string `json:"symbol"`
			Decimals    int    `json:"decimals"`
			Verified    bool   `json:"verified"`
			CoingeckoID string `json:"coingecko_id"`
			Base        string `json:"base"`
		} `json:"denoms"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]DenomMeta{}
	for chain, denoms := range body.Denoms {
		m := map[string]DenomMeta{}
		for d, v := range denoms {
			m[d] = DenomMeta{Symbol: v.Symbol, Decimals: v.Decimals, Verified: v.Verified, CoingeckoID: v.CoingeckoID, Base: v.Base}
		}
		out[chain] = m
	}
	return out
}

// symbolOnlyMeta is what the API read from /denoms before listed lookalikes:
// symbol and decimals only, so no symbol is listed and nothing is a lookalike.
func symbolOnlyMeta(meta map[string]DenomMeta) map[string]DenomMeta {
	out := make(map[string]DenomMeta, len(meta))
	for d, m := range meta {
		out[d] = DenomMeta{Symbol: m.Symbol, Decimals: m.Decimals}
	}
	return out
}

func noPriceServer(t *testing.T) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return NewServer(nil, NewOracle(srv.URL, ""), DefaultNativeAsset)
}

var lookalikeAt = time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)

func lookalikeRow(s *Server, chain string, meta map[string]DenomMeta, denom string) Row {
	return s.buildRow(chain, meta, lookalikeAt, "LOOKALIKETX", apiTransfer, "in", denom, "10000000", "sender", "wallet")
}

// renderEverything is every rendering of one row the tests compare: each
// export format without the review gate, the preview transport, the journal
// and positions.
func renderEverything(t *testing.T, row Row) string {
	t.Helper()
	var all bytes.Buffer
	for _, format := range lookalikeExportFormats {
		if err := writeCSV(&all, format, []Row{row}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
	}
	if err := WritePreviewCSV(&all, apiSumm, []Row{row}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJournalCSV(&all, BuildJournal([]Row{row}, func(string) string { return "" })); err != nil {
		t.Fatal(err)
	}
	positions, _ := json.Marshal(BuildPositions([]Row{row}, lookalikeAt.AddDate(0, 0, 1), func(string) (decimal.Decimal, bool) { return decimal.Zero, false }))
	all.Write(positions)
	return all.String()
}

func csvFields(t *testing.T, out string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// Every denom the oracle knows on mainnet and provider is built with and
// without the listed-asset fields. Only the fake factory USDC vouchers gain an
// identity; every other row (real ATOM and USDC, milkTIA and LVN over other
// routes, every plain-base reroute) is the same Row and renders to the same
// bytes, apart from the export label on ATOM lookalikes the ATOM rule already
// flags.
func TestListedLookalikesOnTheOracleFixture(t *testing.T) {
	fixture := loadOracleDenomsFixture(t)
	s := noPriceServer(t)
	newlyFlagged := map[string][]string{}
	relabelled := map[string][]string{}
	plainBaseReroutes := map[string]int{}
	for _, chain := range []string{apiMainnet, apiProvider} {
		full := fixture[chain]
		if len(full) == 0 {
			t.Fatalf("fixture has no %s denoms", chain)
		}
		before := symbolOnlyMeta(full)
		index := newOfficialIndex(full)
		denoms := make([]string, 0, len(full))
		for d := range full {
			denoms = append(denoms, d)
		}
		sort.Strings(denoms)
		for _, denom := range denoms {
			now := lookalikeRow(s, chain, full, denom)
			was := lookalikeRow(s, chain, before, denom)
			m := full[denom]
			if listed := index[strings.ToUpper(m.Symbol)]; listed != nil && !m.Verified && m.Base != "" && !strings.Contains(m.Base, "/") {
				plainBaseReroutes[chain]++
			}
			if was.AssetIdentity == nil && now.AssetIdentity != nil {
				newlyFlagged[chain] = append(newlyFlagged[chain], denom)
				continue
			}
			if now.AssetIdentity != nil && now.AssetIdentity.ExportLabel != "" {
				relabelled[chain] = append(relabelled[chain], denom+"="+now.AssetIdentity.ExportLabel)
				if was.AssetIdentity == nil || !strings.EqualFold(was.AssetIdentity.ReportedLabel, apiSymbolATOM) {
					t.Fatalf("%s %s relabelled without an ATOM flag before", chain, denom)
				}
				continue
			}
			if !reflect.DeepEqual(now, was) {
				t.Fatalf("%s %s row changed:\nnow %+v\nwas %+v", chain, denom, now, was)
			}
			if a, b := renderEverything(t, now), renderEverything(t, was); a != b {
				t.Fatalf("%s %s renders differently", chain, denom)
			}
		}
	}
	t.Logf("plain-base reroutes of a listed symbol, unchanged: %v", plainBaseReroutes)
	t.Logf("relabelled ATOM lookalikes: %v", relabelled)
	if got := newlyFlagged[apiProvider]; len(got) != 0 {
		t.Fatalf("provider gained flags: %v", got)
	}
	got := newlyFlagged[apiMainnet]
	if len(got) != 3 {
		t.Fatalf("want the three factory uusdc vouchers newly flagged, got %v", got)
	}
	for _, denom := range got {
		row := lookalikeRow(s, apiMainnet, fixture[apiMainnet], denom)
		a := row.AssetIdentity
		if row.Symbol != apiSymbolUSDC || a.ReportedLabel != apiSymbolUSDC || a.ListedLookalike != apiSymbolUSDC || a.ExportLabel != denom || !strings.HasPrefix(a.BaseToken, "factory/") || !strings.HasSuffix(a.BaseToken, "/uusdc") {
			t.Fatalf("%s identity %+v", denom, a)
		}
		if !row.PriceMissing || !row.PriceUSD.IsZero() {
			t.Fatalf("%s must stay unpriced: %+v", denom, row)
		}
	}
	if plainBaseReroutes[apiMainnet] < 500 {
		t.Fatalf("fixture should hold the Hub's plain-base reroutes, got %d", plainBaseReroutes[apiMainnet])
	}
	// The Juno factory ATOMREWARDS voucher and the stkATOM vouchers labelled
	// ATOM carry an export label; ATOM over another route does not.
	labels := strings.Join(relabelled[apiMainnet], " ")
	if !strings.Contains(labels, junoFactoryUatomVoucher+"="+junoFactoryUatomSymbol) {
		t.Fatalf("ATOMREWARDS not relabelled: %s", labels)
	}
}

// Real assets render byte-for-byte as before: native ATOM, Noble USDC, and
// milkTIA and LVN arriving over a route other than the listed one.
func TestListedLookalikesLeaveRealAssetsAlone(t *testing.T) {
	fixture := loadOracleDenomsFixture(t)
	full := fixture[apiMainnet]
	before := symbolOnlyMeta(full)
	s := noPriceServer(t)
	cases := []string{apiUatom, cosmosHubNobleUSDCDenom, cosmosHubNobleDirectUSDCDenom}
	for _, symbol := range []string{"milkTIA", "LVN"} {
		for d, m := range full {
			if strings.EqualFold(m.Symbol, symbol) && !m.Verified && strings.HasPrefix(m.Base, "factory/") {
				cases = append(cases, d)
			}
		}
	}
	if len(cases) < 5 {
		t.Fatalf("fixture lacks milkTIA or LVN reroutes: %v", cases)
	}
	for _, denom := range cases {
		now := lookalikeRow(s, apiMainnet, full, denom)
		was := lookalikeRow(s, apiMainnet, before, denom)
		if now.AssetIdentity != nil && now.AssetIdentity.ExportLabel != "" {
			t.Fatalf("%s relabelled: %+v", denom, now.AssetIdentity)
		}
		if !reflect.DeepEqual(now, was) {
			t.Fatalf("%s row changed", denom)
		}
		if a, b := renderEverything(t, now), renderEverything(t, was); a != b {
			t.Fatalf("%s renders differently", denom)
		}
	}
	atom := lookalikeRow(s, apiMainnet, full, apiUatom)
	if atom.Symbol != apiSymbolATOM || atom.AssetIdentity != nil {
		t.Fatalf("native ATOM %+v", atom)
	}
}

func fakeUSDCEvent(denom string) tax.TaxableEvent {
	return tax.TaxableEvent{TxHash: "LOOKALIKETX", MessageID: 7, Denom: denom, Amount: "10000000", Timestamp: lookalikeAt, Category: apiTransfer, FromAddr: "sender", ToAddr: "wallet"}
}

// A fake tokenfactory USDC is held for review like ATOMREWARDS: suspected
// spam, the lookalike and issuer reasons, and WriteCSV refuses every format
// until the user decides it. After a decision the user's choice decides.
func TestFakeFactoryUSDCNeedsReviewBeforeExport(t *testing.T) {
	fixture := loadOracleDenomsFixture(t)
	full := fixture[apiMainnet]
	s := noPriceServer(t)
	var denom string
	for d, m := range full {
		if m.Symbol == apiSymbolUSDC && !m.Verified && strings.HasPrefix(m.Base, "factory/juno") {
			denom = d
			break
		}
	}
	if denom == "" {
		t.Fatal("fixture has no Juno factory uusdc voucher")
	}
	row := lookalikeRow(s, apiMainnet, full, denom)
	attachSpamEvidence(&row, fakeUSDCEvent(denom), apiMainnet, "wallet", "")
	a := row.AssetIdentity
	if a == nil || !a.SuspectedSpam || a.RecordID == "" {
		t.Fatalf("identity %+v", a)
	}
	if !reflect.DeepEqual(a.ReasonCodes, []string{ReasonListedLookalike, ReasonIssuerToken}) {
		t.Fatalf("reason codes %v", a.ReasonCodes)
	}
	for _, format := range lookalikeExportFormats {
		var out bytes.Buffer
		err := WriteCSV(&out, format, []Row{row})
		var pending assetReviewRequired
		if !errors.As(err, &pending) || out.Len() != 0 {
			t.Fatalf("%s exported a pending lookalike: err=%v bytes=%d", format, err, out.Len())
		}
	}

	// The preview transport shows it under its raw denom with the reported
	// label kept in the identity metadata.
	var preview bytes.Buffer
	if err := WritePreviewCSV(&preview, apiCryptotaxcalculator, []Row{row}); err != nil {
		t.Fatal(err)
	}
	records := csvFields(t, preview.String())
	for _, f := range records[1] {
		if f == apiSymbolUSDC {
			t.Fatalf("preview renders the USDC label: %v", records[1])
		}
	}
	if !strings.Contains(preview.String(), `""reported_label"":""USDC""`) || !strings.Contains(preview.String(), denom) {
		t.Fatalf("preview lost the reported label or denom: %s", preview.String())
	}

	excluded, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{a.RecordID: {Mode: AssetDecisionExclude}})
	if err != nil {
		t.Fatal(err)
	}
	var koinly, generic bytes.Buffer
	if err := WriteCSV(&koinly, apiKoinly, excluded); err != nil {
		t.Fatal(err)
	}
	if n := len(csvFields(t, koinly.String())); n != 1 {
		t.Fatalf("excluded lookalike reached Koinly: %s", koinly.String())
	}
	if err := WriteCSV(&generic, apiGeneric, excluded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generic.String(), apiUnverifiedDenomPrefix+denom+")") {
		t.Fatalf("generic %s", generic.String())
	}

	overridden, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{a.RecordID: {Mode: AssetDecisionOverride, Token: "USDC", Quantity: "10", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: "2026-05-21"}})
	if err != nil {
		t.Fatal(err)
	}
	koinly.Reset()
	if err := WriteCSV(&koinly, apiKoinly, overridden); err != nil {
		t.Fatal(err)
	}
	if got := csvFields(t, koinly.String())[1][4]; got != "USDC" {
		t.Fatalf("override token not kept: %q", got)
	}
}

// Each export format, the preview, the journal and positions render a
// lookalike under its own name and never under the listed symbol: the Juno
// ATOMREWARDS voucher, and a made-up factory OSMO with no token name.
func TestLookalikeExportLabelInEveryFormat(t *testing.T) {
	fixture := loadOracleDenomsFixture(t)
	full := map[string]DenomMeta{}
	for d, m := range fixture[apiMainnet] {
		full[d] = m
	}
	fakeOSMO := "ibc/0000000000000000000000000000000000000000000000000000000000000F05"
	full[fakeOSMO] = DenomMeta{Symbol: "OSMO", Decimals: 6, Base: "factory/osmo1lookalike/uosmo"}
	s := noPriceServer(t)
	for _, tc := range []struct{ denom, listed, label string }{
		{junoFactoryUatomVoucher, apiSymbolATOM, junoFactoryUatomSymbol},
		{fakeOSMO, "OSMO", fakeOSMO},
	} {
		row := lookalikeRow(s, apiMainnet, full, tc.denom)
		attachSpamEvidence(&row, fakeUSDCEvent(tc.denom), apiMainnet, "wallet", "")
		if row.Symbol != tc.listed || row.AssetIdentity.ReportedLabel != tc.listed {
			t.Fatalf("%s: API symbol must stay the reported label: %+v", tc.denom, row)
		}
		if got := row.exportSymbol(); got != tc.label {
			t.Fatalf("%s export label %q, want %q", tc.denom, got, tc.label)
		}
		for _, format := range lookalikeExportFormats {
			var out bytes.Buffer
			if err := writeCSV(&out, format, []Row{row}); err != nil {
				t.Fatal(err)
			}
			records := csvFields(t, out.String())
			found := false
			for _, f := range records[1] {
				if f == tc.listed {
					t.Fatalf("%s %s renders the listed symbol: %v", tc.denom, format, records[1])
				}
				found = found || f == tc.label || strings.Contains(f, tc.label)
			}
			if !found {
				t.Fatalf("%s %s lost the export label or raw denom: %v", tc.denom, format, records[1])
			}
		}
		// Pending: the journal and positions leave the receipt out entirely.
		var journal bytes.Buffer
		if err := WriteJournalCSV(&journal, BuildJournal([]Row{row}, func(string) string { return "" })); err != nil {
			t.Fatal(err)
		}
		if n := len(csvFields(t, journal.String())); n != 2 {
			t.Fatalf("%s pending lookalike reached the journal: %s", tc.denom, journal.String())
		}
		noPrice := func(string) (decimal.Decimal, bool) { return decimal.Zero, false }
		if p := BuildPositions([]Row{row}, lookalikeAt.AddDate(0, 0, 1), noPrice); len(p) != 0 {
			t.Fatalf("%s pending lookalike reached positions: %+v", tc.denom, p)
		}
		// Overridden: both book the user's token, never the copied ticker.
		decided, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{row.AssetIdentity.RecordID: {Mode: AssetDecisionOverride, Token: "MYTOKEN", Quantity: "10", ValueUSD: "1", CostBasisUSD: "1", AcquiredDate: "2026-05-21"}})
		if err != nil {
			t.Fatal(err)
		}
		journal.Reset()
		if err := WriteJournalCSV(&journal, BuildJournal(decided, func(string) string { return "" })); err != nil {
			t.Fatal(err)
		}
		booked := 0
		for _, rec := range csvFields(t, journal.String())[1:] {
			if rec[6] == tc.listed {
				t.Fatalf("%s journal books the listed symbol: %v", tc.denom, rec)
			}
			if rec[6] == "MYTOKEN" {
				booked++
			}
		}
		if booked == 0 {
			t.Fatalf("%s overridden receipt not journaled: %s", tc.denom, journal.String())
		}
		positions := BuildPositions(decided, lookalikeAt.AddDate(0, 0, 1), noPrice)
		if len(positions) != 1 || positions[0].Asset != "MYTOKEN" {
			t.Fatalf("%s positions %+v", tc.denom, positions)
		}
	}

	// Real ATOM is the same bytes with and without the listed-asset fields.
	atomNow := lookalikeRow(s, apiMainnet, full, apiUatom)
	atomWas := lookalikeRow(s, apiMainnet, symbolOnlyMeta(full), apiUatom)
	if renderEverything(t, atomNow) != renderEverything(t, atomWas) {
		t.Fatal("real ATOM renders differently")
	}
}

// An oracle that prices every denom at $1 must not put that price on a
// lookalike, while a real reroute of the listed asset keeps it.
func TestLookalikeIsNeverPricedAsTheListedAsset(t *testing.T) {
	fixture := loadOracleDenomsFixture(t)
	full := fixture[apiMainnet]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == "/price" {
			_, _ = w.Write([]byte(`{"usd":1,"found":true,"date":"2026-05-21"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	s := NewServer(nil, NewOracle(srv.URL, ""), DefaultNativeAsset)
	flagged, priced := 0, 0
	for d, m := range full {
		if !strings.EqualFold(m.Symbol, apiSymbolUSDC) || m.Verified {
			continue
		}
		row := lookalikeRow(s, apiMainnet, full, d)
		if row.AssetIdentity != nil {
			flagged++
			if !row.PriceMissing || !row.PriceUSD.IsZero() {
				t.Fatalf("%s lookalike priced: %+v", d, row)
			}
			continue
		}
		priced++
		if row.PriceMissing || !row.PriceUSD.Equal(decimal.NewFromInt(1)) {
			t.Fatalf("%s USDC reroute lost its price: %+v", d, row)
		}
	}
	if flagged != 3 || priced < 50 {
		t.Fatalf("flagged %d, priced %d", flagged, priced)
	}
}

// A failed /denoms refresh keeps the last good snapshot, so the listed-symbol
// index never empties during an oracle outage.
func TestDenomsKeepsLastSnapshotThroughOutage(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"denoms":{"uatom":{"symbol":"ATOM","decimals":6,"verified":true,"coingecko_id":"cosmos"}}}`))
	}))
	t.Cleanup(srv.Close)
	o := NewOracle(srv.URL, "")
	if m := o.Denoms(apiMainnet); m[apiUatom].Symbol != apiSymbolATOM {
		t.Fatalf("first fetch %+v", m)
	}
	up = false
	o.mu.Lock()
	o.at[apiMainnet] = nowUTC().Add(-2 * time.Hour)
	o.mu.Unlock()
	if m := o.Denoms(apiMainnet); m[apiUatom].Symbol != apiSymbolATOM || !m[apiUatom].Verified {
		t.Fatalf("outage emptied the snapshot: %+v", m)
	}
	o.mu.Lock()
	retryIn := o.at[apiMainnet].Add(time.Hour).Sub(nowUTC())
	o.mu.Unlock()
	if retryIn <= 0 || retryIn > denomsRetryAfterFailure {
		t.Fatalf("retry in %s", retryIn)
	}

	// An answer with no denoms is a failed refresh too.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"denoms":{}}`))
	}))
	t.Cleanup(empty.Close)
	o.mu.Lock()
	o.base = empty.URL
	o.at[apiMainnet] = nowUTC().Add(-2 * time.Hour)
	o.mu.Unlock()
	if m := o.Denoms(apiMainnet); m[apiUatom].Symbol != apiSymbolATOM || !m[apiUatom].Verified {
		t.Fatalf("an empty answer replaced the snapshot: %+v", m)
	}
}

func TestContractDenomLookalikeIsJudged(t *testing.T) {
	meta := map[string]DenomMeta{
		cosmosHubNobleUSDCDenom: {Symbol: "USDC", Verified: true, CoingeckoID: "usd-coin", Base: "uusdc"},
		"ibc/CW20USDC":          {Symbol: "USDC", Base: "cw20:juno1fakeusdccontract"},
		"ibc/ERC20USDC":         {Symbol: "USDC", Base: "erc20:0xfakeusdc"},
		"ibc/PLAINUSDC":         {Symbol: "USDC", Base: "uusdc"},
	}
	ix := newOfficialIndex(meta)
	for _, denom := range []string{"ibc/CW20USDC", "ibc/ERC20USDC"} {
		if got := ix.lookalikeOf(meta, denom, denom, "USDC"); got != "USDC" {
			t.Fatalf("%s: a contract-denom USDC was not judged (%q)", denom, got)
		}
	}
	if got := ix.lookalikeOf(meta, "ibc/PLAINUSDC", "ibc/PLAINUSDC", "USDC"); got != "" {
		t.Fatalf("a plain-base USDC reroute was flagged (%q)", got)
	}
}

// fixtureOracle serves the oracle fixture's mainnet /denoms and prices every
// denom at $1, except uatom at atomUSD (no price when atomUSD is 0).
func fixtureOracle(t *testing.T, meta map[string]DenomMeta, atomUSD float64) *Server {
	t.Helper()
	type entry struct {
		Symbol      string `json:"symbol"`
		Decimals    int    `json:"decimals"`
		Verified    bool   `json:"verified"`
		CoingeckoID string `json:"coingecko_id"`
		Base        string `json:"base"`
	}
	denoms := map[string]entry{}
	for d, m := range meta {
		denoms[d] = entry{m.Symbol, m.Decimals, m.Verified, m.CoingeckoID, m.Base}
	}
	body, err := json.Marshal(map[string]any{"denoms": denoms})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		switch {
		case r.URL.Path == "/denoms":
			_, _ = w.Write(body)
		case r.URL.Path == "/price" && r.URL.Query().Get(apiDenom) == apiUatom:
			if atomUSD == 0 {
				_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"usd":%v,"found":true,"date":"2026-05-21"}`, atomUSD)
		case r.URL.Path == "/price":
			_, _ = w.Write([]byte(`{"usd":1,"found":true,"date":"2026-05-21"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return NewServer(nil, NewOracle(srv.URL, ""), DefaultNativeAsset)
}

func fixtureFakeUSDC(t *testing.T, meta map[string]DenomMeta) string {
	t.Helper()
	for d, m := range meta {
		if m.Symbol == apiSymbolUSDC && !m.Verified && strings.HasPrefix(m.Base, "factory/juno") {
			return d
		}
	}
	t.Fatal("fixture has no Juno factory uusdc voucher")
	return ""
}

func fixtureRoundTripATOM(t *testing.T, meta map[string]DenomMeta) string {
	t.Helper()
	for d, m := range meta {
		if d != apiUatom && m.Verified && m.CoingeckoID == "cosmos" && m.Base == apiUatom {
			return d
		}
	}
	t.Fatal("fixture has no priced round-trip ATOM voucher")
	return ""
}

// A flagged denom gets no reference price anywhere a user sees one: not on
// /price-series, and not as a position's market price after the user decides
// the receipt, even when the oracle quotes that denom.
func TestFlaggedDenomsGetNoReferencePrice(t *testing.T) {
	meta := loadOracleDenomsFixture(t)[apiMainnet]
	s := fixtureOracle(t, meta, 10)
	fake, roundTrip := fixtureFakeUSDC(t, meta), fixtureRoundTripATOM(t, meta)

	series := func(denom string) []pricePoint {
		req := httptest.NewRequest(http.MethodGet, "/price-series?denom="+url.QueryEscape(denom)+"&start=2026-05-20&end=2026-05-21", nil)
		rec := httptest.NewRecorder()
		s.handlePriceSeries(rec, req)
		var body struct{ Points []pricePoint }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Points) != 2 {
			t.Fatalf("%s: %d %s", denom, rec.Code, rec.Body.String())
		}
		return body.Points
	}
	for _, denom := range []string{fake, roundTrip} {
		for _, p := range series(denom) {
			if p.Found || p.USD != 0 {
				t.Fatalf("%s: flagged denom published a price %+v", denom, p)
			}
		}
	}
	for denom, want := range map[string]float64{apiUatom: 10, cosmosHubNobleUSDCDenom: 1} {
		for _, p := range series(denom) {
			if !p.Found || p.USD != want {
				t.Fatalf("%s: real asset lost its price %+v", denom, p)
			}
		}
	}

	quoted := s.positionPrice(apiMainnet, "2026-05-21")
	for _, denom := range []string{fake, roundTrip} {
		row := lookalikeRow(s, apiMainnet, meta, denom)
		attachSpamEvidence(&row, fakeUSDCEvent(denom), apiMainnet, "wallet", "")
		if row.AssetIdentity == nil || row.AssetIdentity.RecordID == "" {
			t.Fatalf("%s not flagged: %+v", denom, row.AssetIdentity)
		}
		decided, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{row.AssetIdentity.RecordID: {Mode: AssetDecisionOverride, Token: "MYTOKEN", Quantity: "10", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: "2026-05-21"}})
		if err != nil {
			t.Fatal(err)
		}
		p := BuildPositions(decided, lookalikeAt.AddDate(0, 0, 1), quoted)
		if len(p) != 1 || !p[0].PriceMissing || !p[0].MarketValueUSD.IsZero() || !p[0].UnrealizedGainUSD.IsZero() {
			t.Fatalf("%s: decided flagged receipt took a reference price %+v", denom, p)
		}
	}
	real := lookalikeRow(s, apiMainnet, meta, cosmosHubNobleUSDCDenom)
	if p := BuildPositions([]Row{real}, lookalikeAt.AddDate(0, 0, 1), quoted); len(p) != 1 || p[0].PriceMissing || !p[0].MarketPriceUSD.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("real USDC position lost its price %+v", p)
	}
	// A promotional memo on a real, unflagged OSMO receipt with no price that
	// day is receipt-level spam evidence, not a flagged denom: once the user
	// keeps it as OSMO, the position carries OSMO's price.
	var osmo string
	for d, m := range meta {
		if m.Verified && m.Symbol == "OSMO" && !s.labelDenom(apiMainnet, meta, d).flagged() && (osmo == "" || d < osmo) {
			osmo = d
		}
	}
	if osmo == "" {
		t.Fatal("fixture has no listed OSMO")
	}
	memo := lookalikeRow(noPriceServer(t), apiMainnet, meta, osmo)
	attachSpamEvidence(&memo, fakeUSDCEvent(osmo), apiMainnet, "wallet", "claim your airdrop at https://example.invalid")
	if memo.AssetIdentity == nil || memo.AssetIdentity.RecordID == "" {
		t.Fatalf("spam evidence attached no identity: %+v", memo.AssetIdentity)
	}
	kept, err := ApplyAssetDecisions([]Row{memo}, AssetDecisions{memo.AssetIdentity.RecordID: {Mode: AssetDecisionOverride, Token: "OSMO", Quantity: "10", ValueUSD: "10", CostBasisUSD: "10", AcquiredDate: "2026-05-21"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := BuildPositions(kept, lookalikeAt.AddDate(0, 0, 1), quoted); len(p) != 1 || p[0].PriceMissing || p[0].Denom != osmo || !p[0].MarketPriceUSD.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("kept OSMO lost its price %+v", p)
	}
}

// A fake USDC leg never prices the ATOM it was swapped for: ATOM's value is
// the ATOM oracle price, and with no ATOM price the ATOM leg stays unpriced.
func TestFlaggedSwapLegNeverPricesTheOtherLeg(t *testing.T) {
	meta := loadOracleDenomsFixture(t)[apiMainnet]
	fake := fixtureFakeUSDC(t, meta)
	swap := func(s *Server, atomDir, fakeDir string) (atom, usdc Row) {
		rows := []Row{
			s.buildRow(apiMainnet, meta, lookalikeAt, "SWAPTX", apiSwap, atomDir, apiUatom, "5000000", "a", "b"),
			s.buildRow(apiMainnet, meta, lookalikeAt, "SWAPTX", apiSwap, fakeDir, fake, "100000000", "b", "a"),
		}
		if rows[1].AssetIdentity == nil || !rows[1].PriceMissing {
			t.Fatalf("fake USDC leg not flagged and unpriced: %+v", rows[1])
		}
		implySwapPrices(rows)
		return rows[0], rows[1]
	}

	priced := fixtureOracle(t, meta, 10)
	for _, dirs := range [][2]string{{apiOut, "in"}, {"in", apiOut}} {
		atom, _ := swap(priced, dirs[0], dirs[1])
		if atom.PriceMissing || atom.PriceImplied || !atom.PriceUSD.Equal(decimal.NewFromInt(10)) || !atom.ValueUSD.Equal(decimal.NewFromInt(50)) {
			t.Fatalf("ATOM %s: not valued at the ATOM oracle price: %+v", dirs[0], atom)
		}
	}

	unpriced := fixtureOracle(t, meta, 0)
	for _, dirs := range [][2]string{{apiOut, "in"}, {"in", apiOut}} {
		atom, usdc := swap(unpriced, dirs[0], dirs[1])
		if !atom.PriceMissing || atom.PriceImplied || !atom.ValueUSD.IsZero() {
			t.Fatalf("ATOM %s: priced from the fake USDC leg: %+v", dirs[0], atom)
		}
		if !usdc.PriceMissing || !usdc.ValueUSD.IsZero() {
			t.Fatalf("fake USDC leg priced: %+v", usdc)
		}
	}
}

// /positions carries what the report charges a flagged receipt. Undecided, the
// report holds it for review and positions leave it out. Excluded, the report
// values it at zero and positions leave it out. Overridden, the report and the
// Form 8949 lot use the user's basis, never an oracle quote, and the position
// carries that basis with no reference price, even when the oracle quotes the
// denom.
func TestPositionsMatchReportForFlaggedReceipts(t *testing.T) {
	meta := loadOracleDenomsFixture(t)[apiMainnet]
	s := fixtureOracle(t, meta, 10)
	anyPrice := s.positionPrice(apiMainnet, "2026-05-21")
	asOf := lookalikeAt.AddDate(0, 0, 1)
	for _, denom := range []string{fixtureFakeUSDC(t, meta), fixtureRoundTripATOM(t, meta)} {
		row := lookalikeRow(s, apiMainnet, meta, denom)
		attachSpamEvidence(&row, fakeUSDCEvent(denom), apiMainnet, "wallet", "")
		id := row.AssetIdentity.RecordID

		var out bytes.Buffer
		var pending assetReviewRequired
		if err := WriteCSV(&out, apiKoinly, []Row{row}); !errors.As(err, &pending) {
			t.Fatalf("%s: undecided receipt was not held for review: %v", denom, err)
		}
		if p := BuildPositions([]Row{row}, asOf, anyPrice); len(p) != 0 {
			t.Fatalf("%s: undecided receipt reached positions %+v", denom, p)
		}

		excluded, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{id: {Mode: AssetDecisionExclude}})
		if err != nil {
			t.Fatal(err)
		}
		if !excluded[0].ValueUSD.IsZero() {
			t.Fatalf("%s: excluded receipt valued %s", denom, excluded[0].ValueUSD)
		}
		if p := BuildPositions(excluded, asOf, anyPrice); len(p) != 0 {
			t.Fatalf("%s: excluded receipt reached positions %+v", denom, p)
		}

		decided, err := ApplyAssetDecisions([]Row{row}, AssetDecisions{id: {Mode: AssetDecisionOverride, Token: "MYTOKEN", Quantity: "10", ValueUSD: "25", CostBasisUSD: "20", AcquiredDate: "2026-05-21"}})
		if err != nil {
			t.Fatal(err)
		}
		r := decided[0]
		if !r.ValueUSD.Equal(decimal.NewFromInt(25)) || !r.PriceUSD.Equal(decimal.RequireFromString("2.5")) || r.ManualBasisUSD == nil || !r.ManualBasisUSD.Equal(decimal.NewFromInt(20)) {
			t.Fatalf("%s: report row not at the user's value and basis: %+v", denom, r)
		}
		sale := r
		sale.Direction, sale.Category, sale.Time = apiOut, apiSwap, lookalikeAt.Add(time.Hour)
		sale.ManualBasisUSD, sale.ManualAcquiredDate, sale.AssetDecision = nil, nil, r.AssetDecision
		var lotBasis decimal.Decimal
		for _, f := range Build8949([]Row{r, sale}) {
			lotBasis = lotBasis.Add(f.CostBasis)
		}
		p := BuildPositions(decided, asOf, anyPrice)
		if len(p) != 1 || !p[0].CostBasisUSD.Equal(decimal.NewFromInt(20)) || !p[0].CostBasisUSD.Equal(lotBasis) {
			t.Fatalf("%s: position basis %+v, report lot basis %s", denom, p, lotBasis)
		}
		if !p[0].PriceMissing || !p[0].MarketPriceUSD.IsZero() || !p[0].MarketValueUSD.IsZero() {
			t.Fatalf("%s: decided receipt took a reference price %+v", denom, p[0])
		}
	}
}

// A node that resolves the fake USDC's trace to its unlisted factory base, with
// no bank metadata, still leaves the voucher flagged and unpriced.
func TestResolvedTraceKeepsTheVoucherFlagged(t *testing.T) {
	meta := loadOracleDenomsFixture(t)[apiMainnet]
	fake := fixtureFakeUSDC(t, meta)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ibc/apps/transfer/v1/denom_traces/") {
			w.Header().Set(apiContentType, apiJSONContentType)
			_, _ = w.Write([]byte(`{"denom_trace":{"path":"transfer/channel-207","base_denom":"` + meta[fake].Base + `"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(node.Close)
	priced := fixtureOracle(t, meta, 10)
	s := NewServer(nil, NewOracle(priced.oracle.base, node.URL), DefaultNativeAsset)
	row := lookalikeRow(s, apiMainnet, meta, fake)
	if row.AssetIdentity == nil || !row.PriceMissing {
		t.Fatalf("resolved trace unflagged the voucher: %+v", row)
	}
	if !s.labelDenom(apiMainnet, s.oracle.Denoms(apiMainnet), fake).flagged() {
		t.Fatal("price-series would price the voucher")
	}
}

// A /denoms failure before any good snapshot retries in five minutes, so a
// cold-start outage does not leave every denom unlabelled for an hour.
func TestDenomsColdStartFailureRetriesSoon(t *testing.T) {
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"denoms":{"uatom":{"symbol":"ATOM","decimals":6,"verified":true,"coingecko_id":"cosmos"}}}`))
	}))
	t.Cleanup(srv.Close)
	o := NewOracle(srv.URL, "")
	if m := o.Denoms(apiMainnet); len(m) != 0 {
		t.Fatalf("outage returned %+v", m)
	}
	o.mu.Lock()
	retryIn := o.at[apiMainnet].Add(time.Hour).Sub(nowUTC())
	o.mu.Unlock()
	if retryIn <= 0 || retryIn > denomsRetryAfterFailure {
		t.Fatalf("retry in %s", retryIn)
	}
	up = true
	o.mu.Lock()
	o.at[apiMainnet] = o.at[apiMainnet].Add(-denomsRetryAfterFailure)
	o.mu.Unlock()
	if m := o.Denoms(apiMainnet); m[apiUatom].Symbol != apiSymbolATOM {
		t.Fatalf("retry did not load the snapshot: %+v", m)
	}
}
