package taxapi

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

const (
	testAxlUSDCDenom  = "ibc/FA33D22EED651DC2D251315AAE2E7C5BA924D308081EE9760AE653AA2F6661CB"
	testStrideDenom   = "ibc/STRIDEHASH"
	testPriceRowHash  = "ABCDEF0123"
	testPriceRowKey   = DecisionKeyPricePrefix + testPriceRowHash + "#0#1"
	testPriceDenomKey = DecisionKeyPricePrefix + testStrideDenom + "@2024-03-01"
)

func unpricedRow(denom, day, hash string, msgIndex, sub int, category, dir, amount string) Row {
	ts, _ := time.Parse(apiDateLayout, day)
	return Row{
		Time: ts.Add(13 * time.Hour), Category: category, Direction: dir, Symbol: "STRD", Denom: denom,
		Amount: decimal.RequireFromString(amount), TxHash: hash, MessageID: 7, SubIndex: sub,
		MessageIndex: msgIndex, hasMessageIndex: true, PriceMissing: true,
	}
}

func TestParseUserPriceDecisions(t *testing.T) {
	d, err := ParseAssetDecisions(`{"price:ibc/STRIDEHASH@2024-03-01":{"mode":"override","unit_value_usd":"1.25"},"price:factory/osmo1x/uphmn@2024-03-02":{"mode":"override","unit_value_usd":"0"},"price:ABCDEF0123#0#1":{"mode":"override","value_usd":"0.5"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := d["price:factory/osmo1x/uphmn@2024-03-02"].UnitValueUSD; got != "0" {
		t.Fatalf("explicit zero day price = %q", got)
	}
	if got := d[testPriceRowKey].ValueUSD; got != "0.5" {
		t.Fatalf("row value = %q", got)
	}
	for name, input := range map[string]string{
		"price mode":         `{"price:uatom@2024-03-01":{"mode":"price","unit_value_usd":"1"}}`,
		"exclude":            `{"price:uatom@2024-03-01":{"mode":"exclude"}}`,
		"no day":             `{"price:uatom":{"mode":"override","unit_value_usd":"1"}}`,
		"bad day":            `{"price:uatom@2024-02-30":{"mode":"override","unit_value_usd":"1"}}`,
		"total on denom":     `{"price:uatom@2024-03-01":{"mode":"override","value_usd":"1"}}`,
		"unit on row":        `{"price:ABCDEF0123#0#1":{"mode":"override","unit_value_usd":"1"}}`,
		"negative":           `{"price:uatom@2024-03-01":{"mode":"override","unit_value_usd":"-1"}}`,
		"exponent":           `{"price:ABCDEF0123#0#1":{"mode":"override","value_usd":"1e3"}}`,
		"lowercase row hash": `{"price:abcdef0123#0#1":{"mode":"override","value_usd":"1"}}`,
		"basis":              `{"price:ABCDEF0123#0#1":{"mode":"override","value_usd":"1","cost_basis_usd":"1"}}`,
		"token":              `{"price:uatom@2024-03-01":{"mode":"override","token":"X","unit_value_usd":"1"}}`,
		"acquired":           `{"price:uatom@2024-03-01":{"mode":"override","unit_value_usd":"1","acquired_date":"2024-03-01"}}`,
		"empty target":       `{"price:":{"mode":"override","value_usd":"1"}}`,
		"applied_from":       `{"price:uatom@2024-03-01":{"mode":"override","unit_value_usd":"1","applied_from":"x"}}`,
		"unknown field":      `{"price:uatom@2024-03-01":{"mode":"override","daily_usd":{"2024-03-01":"1"}}}`,
	} {
		if _, err := ParseAssetDecisions(input); err == nil {
			t.Errorf("%s: accepted %s", name, input)
		}
	}
}

func TestUserPricesHaveTheirOwnBound(t *testing.T) {
	day := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	b.WriteString(`{`)
	for i := 0; i < 600; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"price:uatom@` + day.AddDate(0, 0, i).Format(apiDateLayout) + `":{"mode":"override","unit_value_usd":"1.5"}`)
	}
	b.WriteString(`}`)
	if _, err := ParseAssetDecisions(b.String()); err != nil {
		t.Fatalf("600 user prices should not count toward the %d decision cap: %v", MaxAssetDecisions, err)
	}
	many := AssetDecisions{}
	for i := 0; i <= MaxUserPrices; i++ {
		many["price:uatom@"+day.AddDate(0, 0, i).Format(apiDateLayout)] = AssetDecision{Mode: AssetDecisionOverride, UnitValueUSD: "1"}
	}
	if err := ValidateAssetDecisions(many); err == nil {
		t.Fatal("user prices over the cap accepted")
	}
}

func TestUserPricesFillOnlyMissingPrices(t *testing.T) {
	priced := unpricedRow(testStrideDenom, "2024-03-01", "AAAA", 0, 0, apiReward, "in", "2")
	priced.PriceMissing, priced.PriceUSD, priced.ValueUSD = false, decimal.RequireFromString("3"), decimal.RequireFromString("6")
	implied := unpricedRow(testStrideDenom, "2024-03-01", "BBBB", 0, 0, "swap", "in", "1")
	implied.PriceMissing, implied.PriceImplied, implied.PriceUSD = false, true, decimal.RequireFromString("2.5")
	flagged := unpricedRow(testStrideDenom, "2024-03-01", "CCCC", 0, 0, apiTransfer, "in", "9")
	flagged.AssetIdentity = &AssetIdentity{RecordID: "mainnet|w|1|0:aa", RawDenom: testStrideDenom}
	rows := []Row{
		unpricedRow(testStrideDenom, "2024-03-01", "DDDD", 0, 0, apiReward, "in", "4"),
		unpricedRow(testStrideDenom, "2024-03-01", testPriceRowHash, 0, 1, apiReward, "in", "10"),
		unpricedRow(testStrideDenom, "2024-03-03", "EEEE", 0, 0, apiReward, "in", "1"),
		unpricedRow(testStrideDenom, "2024-03-01", "FFFF", 0, 0, apiFee, "out", "0.5"),
		priced, implied, flagged,
	}
	decisions := AssetDecisions{
		testPriceDenomKey: {Mode: AssetDecisionOverride, UnitValueUSD: "1.25"},
		DecisionKeyPricePrefix + testStrideDenom + "@2024-03-02": {Mode: AssetDecisionOverride, UnitValueUSD: "9"},
		testPriceRowKey:          {Mode: AssetDecisionOverride, ValueUSD: "7.5"},
		"price:uosmo@2024-03-01": {Mode: AssetDecisionOverride, UnitValueUSD: "1"},
	}
	out, err := ApplyAssetDecisions(rows, decisions)
	if err != nil {
		t.Fatal(err)
	}
	check := func(i int, price, value, key string) {
		t.Helper()
		r := out[i]
		if r.PriceMissing || !r.PriceUserSupplied || r.PriceUSD.String() != price || r.ValueUSD.String() != value || r.UserPriceKey != key {
			t.Fatalf("row %d = missing %v user %v price %s value %s key %q", i, r.PriceMissing, r.PriceUserSupplied, r.PriceUSD, r.ValueUSD, r.UserPriceKey)
		}
	}
	check(0, "1.25", "5", testPriceDenomKey)
	check(1, "0.75", "7.5", testPriceRowKey)
	check(3, "1.25", "0.625", testPriceDenomKey)
	if !out[2].PriceMissing || out[2].PriceUserSupplied {
		t.Fatal("a day the user did not price must stay missing")
	}
	if out[4].PriceUserSupplied || out[4].PriceUSD.String() != "3" {
		t.Fatal("a reference price must never be replaced by a user price")
	}
	if out[5].PriceUserSupplied || !out[5].PriceImplied {
		t.Fatal("an implied price must never be replaced by a user price")
	}
	if out[6].PriceUserSupplied || !out[6].PriceMissing {
		t.Fatal("a flagged receipt is valued only through its identity decision")
	}
	if got := UnmatchedDecisionKeys(out, decisions); strings.Join(got, ",") != DecisionKeyPricePrefix+testStrideDenom+"@2024-03-02,price:uosmo@2024-03-01" {
		t.Fatalf("unmatched = %v", got)
	}
	if PendingAssetReviews(out) != 1 {
		t.Fatal("a user price must not clear an identity review")
	}
}

func TestUserPriceRowKeyNeedsItsMessageIndex(t *testing.T) {
	r := unpricedRow(testStrideDenom, "2024-03-01", testPriceRowHash, 0, 1, apiReward, "in", "1")
	r.hasMessageIndex = false
	out, err := ApplyAssetDecisions([]Row{r}, AssetDecisions{testPriceRowKey: {Mode: AssetDecisionOverride, ValueUSD: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].PriceUserSupplied {
		t.Fatal("a row without its message index has no row key to match")
	}
	if !hasUserPriceRowKeys(AssetDecisions{testPriceRowKey: {}}) || hasUserPriceRowKeys(AssetDecisions{testPriceDenomKey: {}}) {
		t.Fatal("row key detection")
	}
}

func TestUserPricesAreLabelledInEveryExport(t *testing.T) {
	rows, err := ApplyAssetDecisions([]Row{
		unpricedRow(testStrideDenom, "2024-03-01", "AAAA", 0, 0, apiReward, "in", "4"),
		unpricedRow(testStrideDenom, "2024-03-05", "BBBB", 1, 0, apiTransfer, "out", "4"),
	}, AssetDecisions{
		testPriceDenomKey: {Mode: AssetDecisionOverride, UnitValueUSD: "1.25"},
		DecisionKeyPricePrefix + testStrideDenom + "@2024-03-05": {Mode: AssetDecisionOverride, UnitValueUSD: "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := rows[0].description(); !strings.Contains(d, userPriceNote) || strings.Contains(d, "price missing") {
		t.Fatalf("description = %q", d)
	}

	var preview bytes.Buffer
	if err := WritePreviewCSV(&preview, apiCryptotaxcalculator, rows); err != nil {
		t.Fatal(err)
	}
	records, _ := csv.NewReader(&preview).ReadAll()
	col := func(name string) int {
		for i, h := range records[0] {
			if h == name {
				return i
			}
		}
		t.Fatalf("preview has no %q column", name)
		return -1
	}
	if records[1][col("Price Source")] != "user" || records[1][col("User Price Key")] != testPriceDenomKey || records[1][col("Denom")] != testStrideDenom || records[1][col("Row Key")] != "AAAA#0#0" {
		t.Fatalf("preview row = %v", records[1])
	}
	if records[1][col("Reference Price Per Unit (Optional)")] != "1.25" {
		t.Fatalf("preview price = %v", records[1])
	}

	var generic bytes.Buffer
	if err := WriteCSV(&generic, apiGeneric, rows); err != nil {
		t.Fatal(err)
	}
	g, _ := csv.NewReader(&generic).ReadAll()
	if h := g[0]; h[len(h)-3] != "price_source" || h[len(h)-2] != "user_price_key" || h[len(h)-1] != "row_key" {
		t.Fatalf("generic header = %v", h)
	}
	if r := g[1]; r[len(r)-3] != "user" || r[len(r)-2] != testPriceDenomKey || r[len(r)-1] != "AAAA#0#0" || r[8] != "5" {
		t.Fatalf("generic row = %v", r)
	}

	lines := Build8949(rows)
	if len(lines) != 1 {
		t.Fatalf("8949 lines = %+v", lines)
	}
	l := lines[0]
	if !strings.Contains(l.Description, userSalePriceNote) || !strings.Contains(l.Description, userReceiptPriceNote) || l.BasisUnknown || l.ProceedsUnknown {
		t.Fatalf("8949 line = %+v", l)
	}
	if l.Proceeds.String() != "8" || l.CostBasis.String() != "5" {
		t.Fatalf("8949 proceeds/basis = %s/%s", l.Proceeds, l.CostBasis)
	}
}

func TestPriceSourceNames(t *testing.T) {
	for want, r := range map[string]Row{
		"reference":    {},
		"implied":      {PriceImplied: true},
		"missing":      {PriceMissing: true},
		"user":         {PriceUserSupplied: true},
		"asset_review": {AssetDecision: &AssetDecision{Mode: AssetDecisionExclude}},
	} {
		if got := priceSource(r); got != want {
			t.Errorf("priceSource = %q, want %q", got, want)
		}
	}
}

func TestNobleUSDCPegAllowlistIsExact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer srv.Close()
	o := NewOracle(srv.URL, "")
	for _, denom := range []string{cosmosHubNobleUSDCDenom, cosmosHubNobleDirectUSDCDenom} {
		if usd, found := o.PriceAt(apiMainnet, denom, apiTestReceiptDate); !found || usd != 1 {
			t.Fatalf("%s: want the $1 Noble USDC peg, got %v/%v", denom, usd, found)
		}
	}
	if _, found := o.PriceAt(apiMainnet, testAxlUSDCDenom, apiTestReceiptDate); found {
		t.Fatal("Axelar USDC through Neutron is a different asset and must not be pegged")
	}
	if _, found := o.PriceAt("cosmoshub-testnet", cosmosHubNobleDirectUSDCDenom, apiTestReceiptDate); found {
		t.Fatal("the peg is for the mainnet voucher only")
	}
}
