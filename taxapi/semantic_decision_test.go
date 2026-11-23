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
	"github.com/shopspring/decimal"
)

const (
	testSemanticAcquired = "2024-12-01"
	testSemanticDetail   = `{"contract":"cosmos1contract","fallback":"bank","review_required":"true","review_reason":"unclassified_contract_movement"}`
	testSemanticKeyOut   = "mainnet|w|7|0:semantic:aa"
	testSemanticKeyIn    = "mainnet|w|7|1:semantic:bb"
)

func semanticRow(key, dir, amount string, priced bool) Row {
	r := Row{
		Time: time.Date(2025, 3, 4, 10, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: dir,
		Symbol: apiSymbolATOM, Denom: apiUatom, Amount: decimal.RequireFromString(amount), TxHash: "SEMTX",
		MessageID: 7, Detail: testSemanticDetail, SemanticRecordID: key, PriceMissing: !priced,
	}
	if priced {
		r.PriceUSD = decimal.NewFromInt(5)
		r.ValueUSD = r.Amount.Mul(r.PriceUSD)
	}
	return r
}

func TestParseSemanticDecisions(t *testing.T) {
	d, err := ParseAssetDecisions(`{"mainnet|w|7|0:semantic:aa":{"classification":"protocol_deposit"},"mainnet|w|7|1:semantic:bb":{"classification":"transfer_in","value_usd":"3","cost_basis_usd":"1.5","acquired_date":"2024-01-02"},"mainnet|w|7|2:ab":{"mode":"exclude","classification":"exclude"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := d[testSemanticKeyIn]; got.Classification != ClassTransferIn || got.CostBasisUSD != "1.5" || got.Mode != "" {
		t.Fatalf("semantic decision = %+v", got)
	}
	if got := d["mainnet|w|7|2:ab"]; got.Mode != AssetDecisionExclude || got.Classification != ClassExclude {
		t.Fatalf("record decision lost its classification: %+v", got)
	}
	canonical, err := CanonicalAssetDecisionsJSON(d)
	if err != nil || !strings.Contains(canonical, `"classification":"protocol_deposit"`) {
		t.Fatalf("canonical JSON must carry the classification (cache key): %s %v", canonical, err)
	}
	for name, input := range map[string]string{ // #nosec G101 -- decision fixtures, no credentials
		"mode on semantic":        `{"mainnet|w|7|0:semantic:aa":{"mode":"exclude","classification":"exclude"}}`,
		"no classification":       `{"mainnet|w|7|0:semantic:aa":{"value_usd":"1"}}`,
		"unknown classification":  `{"mainnet|w|7|0:semantic:aa":{"classification":"airdrop_maybe"}}`,
		"token":                   `{"mainnet|w|7|0:semantic:aa":{"classification":"income","token":"X"}}`,
		"quantity":                `{"mainnet|w|7|0:semantic:aa":{"classification":"income","quantity":"1"}}`,
		"unit price":              `{"mainnet|w|7|0:semantic:aa":{"classification":"income","unit_value_usd":"1"}}`,
		"exclude with value":      `{"mainnet|w|7|0:semantic:aa":{"classification":"exclude","value_usd":"1"}}`,
		"basis on income":         `{"mainnet|w|7|0:semantic:aa":{"classification":"income","cost_basis_usd":"1"}}`,
		"date without basis":      `{"mainnet|w|7|0:semantic:aa":{"classification":"transfer_in","acquired_date":"2024-01-02"}}`,
		"negative value":          `{"mainnet|w|7|0:semantic:aa":{"classification":"income","value_usd":"-1"}}`,
		"applied_from":            `{"mainnet|w|7|0:semantic:aa":{"classification":"income","applied_from":"x"}}`,
		"denom rule":              `{"denom:uatom":{"mode":"exclude","classification":"exclude"}}`,
		"spam policy":             `{"policy:spam":{"mode":"exclude","classification":"exclude"}}`,
		"price row key":           `{"price:ABCDEF0123#0#1":{"mode":"override","value_usd":"1","classification":"income"}}`,
		"price day key":           `{"price:uatom@2024-03-01":{"mode":"override","unit_value_usd":"1","classification":"income"}}`,
		"record exclude override": `{"mainnet|w|7|2:ab":{"mode":"override","token":"X","quantity":"1","value_usd":"1","cost_basis_usd":"1","acquired_date":"2024-01-02","classification":"exclude"}}`,
		"record bad class":        `{"mainnet|w|7|2:ab":{"mode":"exclude","classification":"gift"}}`,
	} {
		if _, err := ParseAssetDecisions(input); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestSemanticDecisionCountsApart(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < MaxAssetDecisions+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"mainnet|w|%d|0:semantic:aa":{"classification":"swap"}`, i)
	}
	b.WriteString("}")
	if _, err := ParseAssetDecisions(b.String()); err != nil {
		t.Fatalf("semantic keys must not count toward the identity bound: %v", err)
	}
}

func TestApplySemanticClassifications(t *testing.T) {
	rows := []Row{
		semanticRow(testSemanticKeyOut, apiOut, "2", true),
		semanticRow(testSemanticKeyIn, "in", "4", false),
	}
	original := reviewFingerprint(rows)
	if PendingSemanticReviews(rows) != 2 {
		t.Fatal("rows must start pending")
	}
	out, err := ApplyAssetDecisions(rows, AssetDecisions{
		testSemanticKeyOut: {Classification: ClassProtocolDeposit, ValueUSD: "99"},
		testSemanticKeyIn:  {Classification: ClassIncome, ValueUSD: "8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if PendingSemanticReviews(out) != 0 || !readinessForRows(out).ExportReady {
		t.Fatal("classified rows still pending")
	}
	if reviewFingerprint(out) != original {
		t.Fatal("a decision must not change the evidence fingerprint")
	}
	dep, inc := out[0], out[1]
	if dep.Category != string(tax.CategoryProtocolDeposit) || dep.ClassifiedFrom != apiTransfer || dep.isTaxable() {
		t.Fatalf("protocol deposit = %+v", dep)
	}
	if !dep.ValueUSD.Equal(decimal.NewFromInt(10)) || dep.PriceUserSupplied || dep.SemanticDecision.ValueUSD != "" {
		t.Fatalf("value_usd replaced a reference price or stayed on the decision: %+v %+v", dep, dep.SemanticDecision)
	}
	if inc.Category != string(tax.CategoryIncome) || !inc.ValueUSD.Equal(decimal.NewFromInt(8)) || !inc.PriceUSD.Equal(decimal.NewFromInt(2)) || inc.PriceMissing || !inc.PriceUserSupplied || inc.UserPriceKey != testSemanticKeyIn {
		t.Fatalf("income row = %+v", inc)
	}
	if !isIncomeRow(inc) || !strings.Contains(inc.description(), "user_classification=income parser_category=transfer") || reviewMode(inc) != "classified:income" {
		t.Fatalf("income row audit trail: %s / %s", inc.description(), reviewMode(inc))
	}
	if rows[0].Category != apiTransfer || rows[1].SemanticDecision != nil {
		t.Fatal("ApplyAssetDecisions mutated its input")
	}
	if got := UnmatchedDecisionKeys(out, AssetDecisions{testSemanticKeyOut: {Classification: ClassProtocolDeposit}, testSemanticKeyIn: {Classification: ClassIncome}}); len(got) != 0 {
		t.Fatalf("applied semantic keys listed unmatched: %v", got)
	}
}

func TestApplySemanticClassificationShapes(t *testing.T) {
	in := semanticRow(testSemanticKeyIn, "in", "4", false)
	out, err := ApplyAssetDecisions([]Row{in}, AssetDecisions{testSemanticKeyIn: {Classification: ClassTransferIn, CostBasisUSD: "6", AcquiredDate: testSemanticAcquired}})
	if err != nil {
		t.Fatal(err)
	}
	r := out[0]
	if r.Category != apiTransfer || r.ManualBasisUSD == nil || !r.ManualBasisUSD.Equal(decimal.NewFromInt(6)) || r.ManualAcquiredDate.Format(apiDateLayout) != testSemanticAcquired || manualField(r, apiBasis) != "6" || manualField(r, apiDate) != testSemanticAcquired {
		t.Fatalf("transfer in with basis = %+v", r)
	}
	if !r.PriceMissing || PendingSemanticReviews(out) != 0 {
		t.Fatal("a classification without value_usd keeps the missing price and still settles the review")
	}

	out, err = ApplyAssetDecisions([]Row{semanticRow(testSemanticKeyOut, apiOut, "2", true)}, AssetDecisions{testSemanticKeyOut: {Classification: ClassExclude}})
	if err != nil {
		t.Fatal(err)
	}
	if r := out[0]; !r.Excluded || !r.ValueUSD.IsZero() || priceSource(r) != "asset_review" || rowValueText(r) != "0" || len(Build8949(out)) != 0 {
		t.Fatalf("excluded row = %+v", r)
	}

	out, err = ApplyAssetDecisions([]Row{semanticRow(testSemanticKeyOut, apiOut, "2", true)}, AssetDecisions{testSemanticKeyOut: {Classification: ClassPayment}})
	if err != nil {
		t.Fatal(err)
	}
	if r := out[0]; r.Category != apiTransfer || r.UserTag != TagPayment || !r.isTaxable() || len(Build8949(out)) != 1 {
		t.Fatalf("payment out = %+v", r)
	}

	for name, d := range map[string]AssetDecisions{
		"income on an out row":   {testSemanticKeyOut: {Classification: ClassIncome}},
		"deposit on an in row":   {testSemanticKeyIn: {Classification: ClassProtocolDeposit}},
		"stale key":              {"mainnet|w|7|0:semantic:ff": {Classification: ClassSwap}},
		"acquired after receipt": {testSemanticKeyIn: {Classification: ClassTransferIn, CostBasisUSD: "1", AcquiredDate: "2025-03-05"}},
	} {
		if _, err := ApplyAssetDecisions([]Row{semanticRow(testSemanticKeyOut, apiOut, "2", true), semanticRow(testSemanticKeyIn, "in", "4", false)}, d); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestSemanticDecisionNeverSettlesBasisOrUnkeyedMoves(t *testing.T) {
	refund := semanticRow("", "in", "1", true)
	refund.RewardTrigger = ibcTimeoutRefund
	move := semanticRow("", apiOut, "1", true)
	move.RewardTrigger, move.MessageID = tax.TriggerWithdrawAddress, 0
	keyedMove := semanticRow("", apiOut, "1", true)
	keyedMove.RewardTrigger = tax.TriggerWithdrawAddress
	seen := map[string]int{}
	for _, r := range []*Row{&refund, &move, &keyedMove} {
		assignSemanticRecordID(r, tax.TaxableEvent{TxHash: r.TxHash, MessageID: r.MessageID}, apiMainnet, "w", seen)
		if r.SemanticRecordID != "" {
			t.Fatalf("row %s got a classification key", r.RewardTrigger)
		}
	}
	// A key forged onto the refund row still does not settle it.
	refund.SemanticRecordID = testSemanticKeyIn
	out, err := ApplyAssetDecisions([]Row{refund}, AssetDecisions{testSemanticKeyIn: {Classification: ClassTransferIn}})
	if err == nil {
		t.Fatalf("a decision settled an IBC refund: %+v", out)
	}
}

func TestAssignSemanticRecordIDs(t *testing.T) {
	at := time.Date(2025, 3, 4, 10, 0, 0, 0, time.UTC)
	e := tax.TaxableEvent{TxHash: "SEMTX", MessageID: 7, SubIndex: 0, Category: apiTransfer, Denom: apiUatom, Amount: "1", Timestamp: at, FromAddr: "w", ToAddr: "c", Detail: testSemanticDetail}
	seen := map[string]int{}
	a, b := semanticRow("", apiOut, "1", true), semanticRow("", apiOut, "1", true)
	assignSemanticRecordID(&a, e, apiMainnet, "w", seen)
	assignSemanticRecordID(&b, e, apiMainnet, "w", seen)
	if a.SemanticRecordID == "" || b.SemanticRecordID != a.SemanticRecordID+"#2" || decisionKeyKind(a.SemanticRecordID) != decisionKindSemantic || decisionKeyKind(b.SemanticRecordID) != decisionKindSemantic {
		t.Fatalf("identical rows: %q %q", a.SemanticRecordID, b.SemanticRecordID)
	}
	if len(a.SemanticRecordID) > MaxAssetDecisionRecordID {
		t.Fatal("semantic record ID exceeds the record ID bound")
	}
	c := semanticRow("", apiOut, "1", true)
	e2 := e
	e2.Detail = strings.Replace(testSemanticDetail, "cosmos1contract", "cosmos1other", 1)
	c.Detail = e2.Detail
	assignSemanticRecordID(&c, e2, apiMainnet, "w", map[string]int{})
	if c.SemanticRecordID == a.SemanticRecordID {
		t.Fatal("changed detail must change the key")
	}
	flagged := semanticRow("", "in", "1", false)
	flagged.AssetIdentity = &AssetIdentity{RecordID: "mainnet|w|7|0:cc"}
	assignSemanticRecordID(&flagged, e, apiMainnet, "w", seen)
	if flagged.SemanticRecordID != "mainnet|w|7|0:cc" {
		t.Fatalf("flagged receipt must be classified on its record ID: %q", flagged.SemanticRecordID)
	}
	plain := Row{Category: apiTransfer, Direction: "in"}
	assignSemanticRecordID(&plain, e, apiMainnet, "w", seen)
	if plain.SemanticRecordID != "" {
		t.Fatal("a row with no review got a key")
	}
}

func TestClassificationOnFlaggedReceipt(t *testing.T) {
	id := "mainnet|w|7|1:cc"
	r := semanticRow(id, "in", "4", false)
	r.AssetIdentity = &AssetIdentity{RecordID: id, RawDenom: apiUatom}
	out, err := ApplyAssetDecisions([]Row{r}, AssetDecisions{id: {Mode: AssetDecisionOverride, Token: "X", Quantity: "4", ValueUSD: "2", CostBasisUSD: "2", AcquiredDate: "2025-03-04", Classification: ClassIncome}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0]; got.Category != string(tax.CategoryIncome) || !got.ValueUSD.Equal(decimal.NewFromInt(2)) || PendingAssetReviews(out) != 0 || PendingSemanticReviews(out) != 0 || reviewMode(got) != "override;classified:income" {
		t.Fatalf("flagged receipt classified = %+v", got)
	}
	// Mode alone settles the identity, not the classification.
	out, err = ApplyAssetDecisions([]Row{r}, AssetDecisions{id: {Mode: AssetDecisionExclude}})
	if err != nil || PendingSemanticReviews(out) != 1 {
		t.Fatalf("exclude alone settled classification: %v", err)
	}
	// A classification on a receipt with no classification review is stale.
	plain := r
	plain.Detail = ""
	if _, err := ApplyAssetDecisions([]Row{plain}, AssetDecisions{id: {Mode: AssetDecisionExclude, Classification: ClassExclude}}); err == nil {
		t.Fatal("classification on a receipt without review accepted")
	}
}

// semanticFixture is a wallet that sent ATOM into an unnamed contract and got
// an unpriced token back in the same message: both rows wait on
// classification review.
func semanticFixture(t *testing.T) (*Server, string) {
	t.Helper()
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}); err != nil {
		t.Fatal(err)
	}
	wallet := "cosmos1semanticwallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'" + wallet + "')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,31197048,'2026-05-20T08:15:44Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(1,1,'SEMTX',0,'')",
		"INSERT INTO message_types(id,message_type) VALUES(1,'/cosmwasm.wasm.v1.MsgExecuteContract')",
		"INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(1,1,1,0)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 5, 20, 8, 15, 44, 0, time.UTC)
	for _, e := range []tax.TaxableEvent{
		{MessageID: 1, SubIndex: 0, Category: apiTransfer, FromAddr: wallet, ToAddr: "cosmos1contract", Amount: "1000000", Denom: apiUatom, BlockHeight: 31197048, Timestamp: at, TxHash: "SEMTX", Detail: testSemanticDetail},
		{MessageID: 1, SubIndex: 1, Category: apiTransfer, FromAddr: "cosmos1contract", ToAddr: wallet, Amount: "4000000", Denom: testUnpricedDenom, BlockHeight: 31197048, Timestamp: at, TxHash: "SEMTX", Detail: testSemanticDetail},
	} {
		if err := db.Omit(apiMessage).Create(&e).Error; err != nil {
			t.Fatal(err)
		}
	}
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == statementPGDenomsPath {
			fmt.Fprintf(w, `{"denoms":{"%s":{"symbol":"STRD","decimals":6}}}`, testUnpricedDenom)
			return
		}
		if r.URL.Query().Get(apiDenom) == apiUatom {
			_, _ = w.Write([]byte(`{"usd":5,"found":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	t.Cleanup(oracle.Close)
	return NewServer(db, NewOracle(oracle.URL, ""), DefaultNativeAsset), wallet
}

func TestSemanticDecisionsThroughHTTP(t *testing.T) {
	s, wallet := semanticFixture(t)
	if rr := callAsset(t, s, apiEvents, wallet, nil); rr.Code != http.StatusConflict {
		t.Fatalf("unclassified rows must hold the export: %d %s", rr.Code, rr.Body.String())
	}
	rr := callAsset(t, s, apiAssetReviewPath, wallet, nil)
	var review struct {
		Pending         int              `json:"pending"`
		Semantic        []Row            `json:"semantic_reviews"`
		Classifications []map[string]any `json:"classifications"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if len(review.Semantic) != 2 || len(review.Classifications) != len(classDirections) {
		t.Fatalf("asset review = %s", rr.Body.String())
	}
	keys := map[string]string{}
	for _, r := range review.Semantic {
		if decisionKeyKind(r.SemanticRecordID) != decisionKindSemantic {
			t.Fatalf("semantic review row without a key: %+v", r)
		}
		keys[r.Direction] = r.SemanticRecordID
	}
	decisions := AssetDecisions{
		keys[apiOut]: {Classification: ClassSwap},
		keys["in"]:   {Classification: ClassSwap, ValueUSD: "5"},
	}
	rr = callAsset(t, s, apiEvents, wallet, decisions)
	if rr.Code != 200 {
		t.Fatalf("classified export %d %s", rr.Code, rr.Body.String())
	}
	generic := csvRecords(t, rr.Body.String())
	col := map[string]int{}
	for i, h := range generic[0] {
		col[h] = i
	}
	seen := 0
	for _, rec := range generic[1:] {
		if rec[col[apiTxHash]] != "SEMTX" {
			continue
		}
		seen++
		if rec[col[apiCategory]] != apiSwap || rec[col["taxable"]] != apiTrue || !strings.HasPrefix(rec[col["user_treatment"]], "classified:swap") || rec[col[apiDetail]] != testSemanticDetail {
			t.Fatalf("classified generic row = %v", rec)
		}
		if rec[col[apiDenom]] == testUnpricedDenom && (rec[col["price_source"]] != "user" || rec[col[apiValueUSD]] != "5" || rec[col["manual_value_usd"]] != "5") {
			t.Fatalf("user-valued swap leg = %v", rec)
		}
	}
	if seen != 2 {
		t.Fatalf("generic rows seen = %d", seen)
	}
	rr = callAsset(t, s, apiAssetReviewPath, wallet, decisions)
	if rr.Code != 200 {
		t.Fatalf("classified asset review %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil || review.Pending != 0 || len(review.Semantic) != 0 {
		t.Fatalf("classified review still pending: %s", rr.Body.String())
	}
	for name, d := range map[string]AssetDecisions{
		"wrong direction": {keys[apiOut]: {Classification: ClassIncome}},
		"stale key":       {keys[apiOut][:len(keys[apiOut])-1] + "0": {Classification: ClassSwap}},
		"price key class": {"price:SEMTX#0#1": {Mode: AssetDecisionOverride, ValueUSD: "1", Classification: ClassIncome}},
	} {
		if rr := callAsset(t, s, apiEvents, wallet, d); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}
