package taxapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const testUnpricedDenom = "ibc/UNPRICEDSTRD"

// userPriceFixture is a wallet with one receipt of a token the oracle never
// prices (in message 2 of its transaction, so its row key is PRICETX#2#0),
// one Noble USDC receipt the oracle also misses, and one Axelar USDC receipt.
func userPriceFixture(t *testing.T) (*Server, string) {
	t.Helper()
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}); err != nil {
		t.Fatal(err)
	}
	wallet := "cosmos1pricewallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'" + wallet + "')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,31197048,'2026-05-20T08:15:44Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(1,1,'PRICETX',0,''),(2,1,'USDCTX',0,''),(3,1,'AXLTX',0,'')",
		"INSERT INTO message_types(id,message_type) VALUES(1,'/cosmos.bank.v1beta1.MsgSend')",
		"INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(1,1,1,2),(2,2,1,0),(3,3,1,0)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 5, 20, 8, 15, 44, 0, time.UTC)
	for _, e := range []tax.TaxableEvent{
		{MessageID: 1, SubIndex: 0, Category: apiTransfer, FromAddr: "sender", ToAddr: wallet, Amount: "4000000", Denom: testUnpricedDenom, BlockHeight: 31197048, Timestamp: at, TxHash: "PRICETX"},
		{MessageID: 2, SubIndex: 0, Category: apiTransfer, FromAddr: "sender", ToAddr: wallet, Amount: "2500000", Denom: cosmosHubNobleDirectUSDCDenom, BlockHeight: 31197048, Timestamp: at, TxHash: "USDCTX"},
		{MessageID: 3, SubIndex: 0, Category: apiTransfer, FromAddr: "sender", ToAddr: wallet, Amount: "3000000", Denom: testAxlUSDCDenom, BlockHeight: 31197048, Timestamp: at, TxHash: "AXLTX"},
	} {
		if err := db.Omit(apiMessage).Create(&e).Error; err != nil {
			t.Fatal(err)
		}
	}
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == "/denoms" {
			fmt.Fprintf(w, `{"denoms":{"%s":{"symbol":"STRD","decimals":6},"%s":{"symbol":"USDC.n","decimals":6},"%s":{"symbol":"USDC","decimals":6}}}`, testUnpricedDenom, cosmosHubNobleDirectUSDCDenom, testAxlUSDCDenom)
			return
		}
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	t.Cleanup(oracle.Close)
	return NewServer(db, NewOracle(oracle.URL, ""), DefaultNativeAsset), wallet
}

func previewByDenom(t *testing.T, data map[string]any) map[string]map[string]string {
	t.Helper()
	raw, _ := data["csv"].(string)
	records := csvRecords(t, raw)
	out := map[string]map[string]string{}
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, h := range records[0] {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		out[row["Denom"]] = row
	}
	return out
}

func TestUserPricesThroughReportJobAndExports(t *testing.T) {
	s, wallet := userPriceFixture(t)

	rows := previewByDenom(t, waitPreview(t, s, wallet, nil))
	strd := rows[testUnpricedDenom]
	if strd["Price Source"] != "missing" || strd["Row Key"] != "PRICETX#2#0" || !strings.HasSuffix(strd["Description (Optional)"], " (price missing)") {
		t.Fatalf("unpriced preview row = %v", strd)
	}
	if usdc := rows[cosmosHubNobleDirectUSDCDenom]; usdc["Price Source"] != "reference" || usdc["Reference Price Per Unit (Optional)"] != "1" {
		t.Fatalf("Noble USDC must take the $1 peg: %v", usdc)
	}
	if axl := rows[testAxlUSDCDenom]; axl["Price Source"] != "missing" {
		t.Fatalf("Axelar USDC must stay unpriced: %v", axl)
	}

	// One row priced by its row key.
	byRow := AssetDecisions{"price:PRICETX#2#0": {Mode: AssetDecisionOverride, ValueUSD: "3.2"}}
	strd = previewByDenom(t, waitPreview(t, s, wallet, byRow))[testUnpricedDenom]
	if strd["Price Source"] != "user" || strd["User Price Key"] != "price:PRICETX#2#0" || strd["Reference Price Per Unit (Optional)"] != "0.8" || !strings.Contains(strd["Description (Optional)"], userPriceNote) {
		t.Fatalf("row-priced preview row = %v", strd)
	}

	// The same through a denom and day price, in the Generic CSV, with an
	// idle key and a user price that must not replace the USDC peg.
	byDay := AssetDecisions{
		DecisionKeyPricePrefix + testUnpricedDenom + "@2026-05-20":             {Mode: AssetDecisionOverride, UnitValueUSD: "1.1"},
		DecisionKeyPricePrefix + cosmosHubNobleDirectUSDCDenom + "@2026-05-20": {Mode: AssetDecisionOverride, UnitValueUSD: "5"},
		"price:uosmo@2026-05-20": {Mode: AssetDecisionOverride, UnitValueUSD: "1"},
	}
	rr := callAsset(t, s, apiEvents, wallet, byDay)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	generic := csvRecords(t, rr.Body.String())
	col := map[string]int{}
	for i, h := range generic[0] {
		col[h] = i
	}
	seen := 0
	for _, rec := range generic[1:] {
		switch rec[col[apiDenom]] {
		case testUnpricedDenom:
			seen++
			if rec[col["row_key"]] != "PRICETX#2#0" {
				t.Fatalf("generic row key = %q", rec[col["row_key"]])
			}
			if rec[col["price_source"]] != "user" || rec[col[apiUnitPriceUSD]] != "1.1" || rec[col[apiValueUSD]] != "4.4" || rec[col[apiPriceMissing]] != "false" {
				t.Fatalf("generic user-priced row = %v", rec)
			}
		case cosmosHubNobleDirectUSDCDenom:
			seen++
			if rec[col["price_source"]] != "reference" || rec[col["user_price_key"]] != "" || rec[col[apiValueUSD]] != "2.50" {
				t.Fatalf("a user price replaced the peg: %v", rec)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("generic rows seen = %d", seen)
	}

	rr = callAsset(t, s, apiAssetReviewPath, wallet, byDay)
	var review struct {
		Unmatched []string `json:"unmatched_decision_keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if strings.Join(review.Unmatched, ",") != DecisionKeyPricePrefix+cosmosHubNobleDirectUSDCDenom+"@2026-05-20,price:uosmo@2026-05-20" {
		t.Fatalf("unmatched = %v", review.Unmatched)
	}

	// A malformed price key is a 400, not a silent skip.
	rr = callAsset(t, s, apiEvents, wallet, AssetDecisions{"price:pricetx#2#0": {Mode: AssetDecisionOverride, ValueUSD: "1"}})
	if rr.Code != 400 {
		t.Fatalf("lowercase row key accepted: %d %s", rr.Code, rr.Body.String())
	}
}
