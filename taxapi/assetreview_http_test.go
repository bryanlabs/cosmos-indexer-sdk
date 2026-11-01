package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/ory/dockertest/v3"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func assetReviewFixture(t *testing.T) (*Server, *gorm.DB, string, string) {
	t.Helper()
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := pool.Run("postgres", "15-alpine", []string{"POSTGRES_USER=test", "POSTGRES_PASSWORD=test", "POSTGRES_DB=test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Purge(resource); err != nil {
			t.Error(err)
		}
	})
	var db *gorm.DB
	if err := pool.Retry(func() error {
		var e error
		db, e = dbpkg.PostgresDbConnect(resource.GetBoundIP(apiPostgresPort), resource.GetPort(apiPostgresPort), apiTest, apiTest, apiTest, "silent")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}); err != nil {
		t.Fatal(err)
	}
	wallet := "cosmos1testwallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'" + wallet + "')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,31197048,'2026-05-20T08:15:44Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(1,1,'SCAMTX',0,'Claim 10,000 ATOM at https://claim.invalid')",
		"INSERT INTO message_types(id,message_type) VALUES(1,'/cosmos.bank.v1beta1.MsgSend')",
		"INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(1,1,1,0)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	event := tax.TaxableEvent{MessageID: 1, SubIndex: 0, Category: apiTransfer, FromAddr: "sender", ToAddr: wallet, Amount: "10000000000", Denom: junoFactoryUatomVoucher, BlockHeight: 31197048, Timestamp: time.Date(2026, 5, 20, 8, 15, 44, 0, time.UTC), TxHash: "SCAMTX"}
	if err := db.Omit(apiMessage).Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == "/denoms" {
			fmt.Fprintf(w, `{"denoms":{"%s":{"symbol":"ATOM","decimals":6},"uatom":{"symbol":"ATOM","decimals":6}}}`, junoFactoryUatomVoucher)
			return
		}
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	t.Cleanup(oracle.Close)
	s := NewServer(db, NewOracle(oracle.URL, ""), DefaultNativeAsset)
	preview, err := s.rowsFor(apiMainnet, wallet, time.Time{}, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return s, db, wallet, preview[0].AssetIdentity.RecordID
}

func callAsset(t *testing.T, s *Server, path, wallet string, decisions AssetDecisions) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{apiAddress: {wallet}, apiAddresses: {wallet}, apiChain: {apiMainnet}, apiStart: {apiTestPeriodStart}, apiEnd: {apiTestPeriodEnd}, apiFormat: {apiGeneric}}
	if decisions != nil {
		b, err := json.Marshal(decisions)
		if err != nil {
			t.Fatal(err)
		}
		q.Set(apiAssetDecisions, string(b))
	}
	if path == apiReportJob {
		q.Set(apiFormat, apiCryptotaxcalculator)
	}
	req := httptest.NewRequest(apiMethodGET, path+"?"+q.Encode(), nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func waitPreview(t *testing.T, s *Server, wallet string, decisions AssetDecisions) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		rr := callAsset(t, s, apiReportJob, wallet, decisions)
		if rr.Code != 200 {
			t.Fatalf("preview %d %s", rr.Code, rr.Body.String())
		}
		var data map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if data[apiStatus] == ReportJobReady {
			return data
		}
		if data[apiStatus] == ReportJobFailed {
			t.Fatalf("preview failed: %v", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("preview did not complete")
	return nil
}

func csvRecords(t *testing.T, raw string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestAssetReviewHTTPGateAndExplicitDecisions(t *testing.T) {
	s, db, wallet, id := assetReviewFixture(t)
	for _, path := range []string{apiEvents, apiIncome, api8949Path, apiScheduleD, api990TPath, apiAssetReviewPath} {
		rr := callAsset(t, s, path, wallet, nil)
		if rr.Code != 409 {
			t.Fatalf("%s must block unreviewed exports, got %d: %s", path, rr.Code, rr.Body.String())
		}
	}
	pending := waitPreview(t, s, wallet, nil)
	rows := csvRecords(t, pending[apiCSV].(string))
	if len(rows) != 2 || rows[1][2] != apiSymbolATOM {
		t.Fatalf("pending receipt was dropped/relabeled: %v", rows)
	}
	var evidence AssetIdentity
	if err := json.Unmarshal([]byte(rows[1][15]), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.RecordID != id || !evidence.SuspectedSpam || evidence.Decision != nil || evidence.Excluded || evidence.Memo == "" || len(evidence.Reasons) == 0 {
		t.Fatalf("pending evidence wrong: %+v", evidence)
	}
	exclusion := AssetDecisions{id: {Mode: AssetDecisionExclude}}
	rr := callAsset(t, s, apiEvents, wallet, exclusion)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if rows := csvRecords(t, rr.Body.String()); len(rows) != 2 || rows[1][4] != junoFactoryUatomSymbol || rows[1][19] != AssetDecisionExclude || rows[1][20] != "0" || rows[1][21] != "0" {
		t.Fatalf("generic CSV did not retain explicit zero exclusion: %v", rows)
	}
	excludedPreview := waitPreview(t, s, wallet, exclusion)
	erows := csvRecords(t, excludedPreview[apiCSV].(string))
	if len(erows) != 2 {
		t.Fatal("excluded source vanished from preview")
	}
	if err := json.Unmarshal([]byte(erows[1][15]), &evidence); err != nil {
		t.Fatal(err)
	}
	if !evidence.Excluded || evidence.Decision == nil || evidence.Decision.Mode != AssetDecisionExclude {
		t.Fatal("explicit exclusion not auditable")
	}
	override := AssetDecisions{id: {Mode: AssetDecisionOverride, Token: apiTestOverrideToken, Quantity: "2", ValueUSD: "20", CostBasisUSD: "6", AcquiredDate: apiTestMayStart}}
	rr = callAsset(t, s, apiEvents, wallet, override)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	records := csvRecords(t, rr.Body.String())
	if len(records) != 2 || records[1][4] != apiTestOverrideToken || records[1][6] != "2" || records[1][8] != "20" || records[1][20] != "20" || records[1][21] != "6" {
		t.Fatalf("manual values not reflected: %v", records)
	}
	reviewed, err := s.reviewedRows(apiMainnet, []string{wallet}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), override, false)
	if err != nil {
		t.Fatal(err)
	}
	disposal := Row{Time: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: apiOut, Symbol: apiTestOverrideToken, Amount: d(1), PriceUSD: d(12), ValueUSD: d(12)}
	form := Build8949(append(reviewed, disposal))
	if len(form) != 1 || !form[0].CostBasis.Equal(d(3)) || !form[0].GainLoss.Equal(d(9)) || form[0].DateAcquired != "05/01/2026" || form[0].BasisUnknown || !strings.Contains(form[0].Description, "user-supplied") {
		t.Fatalf("manual basis/date lost: %+v", form)
	}
	waitPreview(t, s, wallet, override)
	var jobCount int64
	db.Model(&ReportJob{}).Count(&jobCount)
	if jobCount != 3 {
		t.Fatalf("decision-specific cache keys missing: %d", jobCount)
	}
	again := waitPreview(t, s, wallet, nil)
	if again[apiCSV] != pending[apiCSV] {
		t.Fatal("override contaminated default preview")
	}
	var original tax.TaxableEvent
	if err := db.First(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.Denom != junoFactoryUatomVoucher || original.Amount != "10000000000" || original.Category != apiTransfer {
		t.Fatal("original record changed")
	}
	for name, bad := range map[string]AssetDecisions{"missing fields": {id: {Mode: AssetDecisionOverride, Token: apiSymbolUSDC}}, "unknown record": {"mainnet|" + wallet + "|999|0": {Mode: AssetDecisionExclude}}, "future basis date": {id: {Mode: AssetDecisionOverride, Token: apiSymbolUSDC, Quantity: "1", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: apiTestPeriodEnd}}} {
		rr := callAsset(t, s, apiEvents, wallet, bad)
		if rr.Code != 400 {
			t.Fatalf("%s accepted: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestMemoCorrectionInvalidatesCachedPreviewAndDecision(t *testing.T) {
	s, db, wallet, id := assetReviewFixture(t)
	before := waitPreview(t, s, wallet, nil)
	decision := AssetDecisions{id: {Mode: AssetDecisionExclude}}
	waitPreview(t, s, wallet, decision)
	if err := db.Exec("UPDATE txes SET memo='Corrected source memo' WHERE hash='SCAMTX'").Error; err != nil {
		t.Fatal(err)
	}
	after := waitPreview(t, s, wallet, nil)
	if before[apiCSV] == after[apiCSV] || !strings.Contains(after[apiCSV].(string), "Corrected source memo") {
		t.Fatal("cached preview kept stale source evidence")
	}
	failed := false
	for i := 0; i < 100; i++ {
		rr := callAsset(t, s, apiReportJob, wallet, decision)
		var result map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result[apiStatus] == ReportJobReady {
			t.Fatal("old decision reused after evidence changed")
		}
		if result[apiStatus] == ReportJobFailed {
			failed = true
			if _, exists := result[apiCSV]; exists {
				t.Fatal("failed job exposed stale CSV")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !failed {
		t.Fatal("invalid decision job restarted endlessly instead of reporting failure")
	}
	if rr := callAsset(t, s, apiEvents, wallet, decision); rr.Code != 400 {
		t.Fatalf("stale approval accepted: %d %s", rr.Code, rr.Body.String())
	}
}

func TestReviewIDChangesWhenOriginalReceiptChanges(t *testing.T) {
	e := tax.TaxableEvent{MessageID: 1, SubIndex: 0, TxHash: "tx", Denom: "ibc/hash", Amount: "100", Timestamp: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)}
	identity := &AssetIdentity{Memo: "original memo", SourceChain: apiJunoChainID, ChannelPath: "transfer/channel-207", BaseToken: "factory/issuer/token"}
	before := assetRecordID(apiMainnet, apiWallet, e, identity)
	identity.Memo = "corrected memo"
	if assetRecordID(apiMainnet, apiWallet, e, identity) == before {
		t.Fatal("memo correction did not invalidate approval")
	}
	identity.Memo = "original memo"
	identity.SourceChain = "other-chain"
	if assetRecordID(apiMainnet, apiWallet, e, identity) == before {
		t.Fatal("trace correction did not invalidate approval")
	}
	identity.SourceChain = apiJunoChainID
	e.Amount = "200"
	if assetRecordID(apiMainnet, apiWallet, e, identity) == before {
		t.Fatal("stale decision could apply to changed amount")
	}
}

func TestReviewPureRulesDoNotInferOrMutate(t *testing.T) {
	when := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	original := Row{Time: when, Direction: "in", Symbol: apiSymbolATOM, Amount: d(10000), PriceMissing: true, AssetIdentity: &AssetIdentity{RecordID: "id", RawAmount: "10000000000", RawDenom: junoFactoryUatomVoucher}}
	if n := PendingAssetReviews([]Row{original}); n != 1 {
		t.Fatal(n)
	}
	var buf strings.Builder
	if err := WriteCSV(&buf, apiGeneric, []Row{original}); err == nil || buf.Len() != 0 {
		t.Fatal("direct CSV bypassed review")
	}
	supplied := AssetDecisions{"id": {Mode: AssetDecisionOverride, Token: apiSymbolUSDC, Quantity: "10000000000000000000000000000000000000", ValueUSD: "0.000000000000000001", CostBasisUSD: "0", AcquiredDate: "2026-05-19"}}
	reviewed, err := ApplyAssetDecisions([]Row{original}, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed[0].PriceUSD.IsZero() || !reviewed[0].ValueUSD.Equal(decimal.RequireFromString("0.000000000000000001")) {
		t.Fatal("small manual total lost")
	}
	if original.Symbol != apiSymbolATOM || original.AssetDecision != nil || original.AssetIdentity.Decision != nil {
		t.Fatal("original mutated")
	}
	if reviewed[0].Symbol != apiSymbolUSDC || reviewed[0].ManualBasisUSD == nil || !reviewed[0].ManualBasisUSD.IsZero() {
		t.Fatal("arbitrary token or explicit zero basis lost")
	}
}

func TestExplicitZeroOverrideExportsZeroNotMissingPrice(t *testing.T) {
	original := Row{Time: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: "in", Symbol: apiSymbolATOM, Amount: d(10000), PriceMissing: true, AssetIdentity: &AssetIdentity{RecordID: "id", RawAmount: "10000000000", RawDenom: junoFactoryUatomVoucher}}
	rows, err := ApplyAssetDecisions([]Row{original}, AssetDecisions{"id": {Mode: AssetDecisionOverride, Token: apiSymbolUSDC, Quantity: "10000", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: apiTestMayStart}})
	if err != nil {
		t.Fatal(err)
	}
	for format, columns := range map[string][]int{apiGeneric: {7, 8}, apiCryptotaxcalculator: {13}, apiKoinly: {7}, apiCryptio: {5, 6}} {
		var b strings.Builder
		if err := WriteCSV(&b, format, rows); err != nil {
			t.Fatal(err)
		}
		records := csvRecords(t, b.String())
		for _, col := range columns {
			if records[1][col] == "" {
				t.Fatalf("%s dropped explicit zero in column %d", format, col)
			}
		}
	}
}

func TestExcludedReceiptHasExplicitZeroTreatmentOnlyInGenericAndPreview(t *testing.T) {
	original := Row{
		Time: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: "in",
		Symbol: apiSymbolATOM, Denom: junoFactoryUatomVoucher, Amount: d(10000), PriceMissing: true,
		AssetIdentity: &AssetIdentity{RecordID: "id", ReportedLabel: apiSymbolATOM, TokenName: junoFactoryUatomSymbol, RawAmount: "10000000000", RawDenom: junoFactoryUatomVoucher, Memo: "source evidence"},
	}
	excluded, err := ApplyAssetDecisions([]Row{original}, AssetDecisions{"id": {Mode: AssetDecisionExclude}})
	if err != nil {
		t.Fatal(err)
	}
	row := excluded[0]
	if !row.Excluded || !row.PriceUSD.IsZero() || !row.ValueUSD.IsZero() || row.ManualBasisUSD == nil || !row.ManualBasisUSD.IsZero() || row.PriceMissing || !row.Amount.Equal(original.Amount) || row.AssetIdentity != original.AssetIdentity {
		t.Fatalf("exclusion did not preserve source or set explicit zero treatment: %+v", row)
	}

	var preview strings.Builder
	if err := WritePreviewCSV(&preview, apiCryptotaxcalculator, excluded); err != nil {
		t.Fatal(err)
	}
	previewRows := csvRecords(t, preview.String())
	if previewRows[1][13] != "0" || !strings.Contains(previewRows[1][15], `"excluded":true`) {
		t.Fatalf("preview did not retain explicit zero exclusion: %v", previewRows[1])
	}

	generic := csvRecords(t, mustWriteCSV(t, apiGeneric, excluded))
	if len(generic) != 2 || generic[1][4] != junoFactoryUatomSymbol || generic[1][6] != "10000" || generic[1][7] != "0" || generic[1][8] != "0" || generic[1][19] != AssetDecisionExclude || generic[1][20] != "0" || generic[1][21] != "0" || !strings.Contains(generic[1][18], "manual zero value and basis") {
		t.Fatalf("generic CSV lost excluded metadata or explicit zero treatment: %v", generic)
	}
	for _, format := range []string{apiKoinly, apiCointracker, apiCoinledger, apiCryptotaxcalculator, apiSumm, apiCryptio, apiBitwave} {
		records := csvRecords(t, mustWriteCSV(t, format, excluded))
		if len(records) != 1 {
			t.Fatalf("%s retained excluded receipt: %v", format, records)
		}
	}

	reset, err := ApplyAssetDecisions([]Row{original}, AssetDecisions{})
	if err != nil {
		t.Fatal(err)
	}
	if reset[0].Excluded || reset[0].AssetDecision != nil || !reset[0].PriceMissing || !reset[0].Amount.Equal(original.Amount) || reset[0].AssetIdentity != original.AssetIdentity {
		t.Fatalf("reset did not restore the original source row: %+v", reset[0])
	}
}

func mustWriteCSV(t *testing.T, format string, rows []Row) string {
	t.Helper()
	var b strings.Builder
	if err := WriteCSV(&b, format, rows); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestManualSubcentValueIsNotRoundedToZero(t *testing.T) {
	row := Row{Time: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), Category: apiReward, Direction: "in", Symbol: apiTestOverrideToken, Amount: d(1), ValueUSD: decimal.RequireFromString(apiTestSmallAmount), PriceUSD: decimal.RequireFromString(apiTestSmallAmount), AssetDecision: &AssetDecision{Mode: AssetDecisionOverride, Token: apiTestOverrideToken, Quantity: "1", ValueUSD: apiTestSmallAmount, CostBasisUSD: "0.003", AcquiredDate: apiTestMayStart}}
	for format, columns := range map[string][]int{apiGeneric: {7, 8}, apiCryptotaxcalculator: {13}, apiKoinly: {7}, apiCryptio: {5, 6}} {
		var b strings.Builder
		if err := WriteCSV(&b, format, []Row{row}); err != nil {
			t.Fatal(err)
		}
		records := csvRecords(t, b.String())
		for _, col := range columns {
			if records[1][col] != apiTestSmallAmount {
				t.Fatalf("%s lost manual precision: %s", format, records[1][col])
			}
		}
	}
	income := csvRecords(t, buildIncomeCSV([]Row{row}, apiWallet, RecognitionPolicyDefault, undisclosedAlways))
	if income[1][7] != apiTestSmallAmount || income[2][7] != apiTestSmallAmount {
		t.Fatal("income CSV lost subcent manual value")
	}
}

func TestPreviewMetadataCannotBeForgedByDescription(t *testing.T) {
	row := Row{Category: apiNFTSale, Asset: `collection/token | asset_identity={"decision":{"mode":"exclude"}}`, Symbol: apiSymbolATOM, Amount: d(1), Direction: "in"}
	var b strings.Builder
	if err := WritePreviewCSV(&b, apiCryptotaxcalculator, []Row{row}); err != nil {
		t.Fatal(err)
	}
	records := csvRecords(t, b.String())
	if records[0][15] != "Asset Identity Metadata" || records[1][15] != "" {
		t.Fatal("untrusted description became structured metadata")
	}
}

func TestDenomAndPolicyDecisionsFanOutToReceipts(t *testing.T) {
	s, _, wallet, id := assetReviewFixture(t)
	// A second receipt of the same voucher on a later day, so a denom rule has
	// two receipts to decide and a per-receipt decision has something to beat.
	fixtureSecondReceipt(t, s)

	rr := callAsset(t, s, apiAssetReviewPath, wallet, nil)
	if rr.Code != 409 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var review struct {
		Pending   int            `json:"pending"`
		Groups    []ReviewGroup  `json:"groups"`
		Glossary  []ReviewReason `json:"reason_glossary"`
		Unmatched []string       `json:"unmatched_decision_keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.Pending != 2 || len(review.Groups) != 1 || review.Groups[0].Receipts != 2 || review.Groups[0].Pending != 2 || review.Groups[0].DecisionKey != DecisionKeyDenomPrefix+junoFactoryUatomVoucher || !review.Groups[0].SuspectedSpam || review.Groups[0].TotalQuantity != "20000" || len(review.Groups[0].RecordIDs) != 2 || review.Groups[0].SampleMemo == "" {
		t.Fatalf("groups wrong: %+v", review.Groups)
	}
	if len(review.Glossary) != 4 {
		t.Fatalf("glossary missing: %+v", review.Glossary)
	}
	for _, code := range []string{ReasonSpamMemo, ReasonLabelMismatch, ReasonUnverifiedFactory} {
		found := false
		for _, c := range review.Groups[0].ReasonCodes {
			found = found || c == code
		}
		if !found {
			t.Fatalf("reason code %s missing from %v", code, review.Groups[0].ReasonCodes)
		}
	}

	// policy:spam excludes every suspected-spam receipt and stamps applied_from.
	policy := AssetDecisions{DecisionKeySpamPolicy: {Mode: AssetDecisionExclude}}
	rr = callAsset(t, s, apiEvents, wallet, policy)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rows := csvRecords(t, rr.Body.String())
	if len(rows) != 3 || rows[1][19] != AssetDecisionExclude || rows[2][19] != AssetDecisionExclude {
		t.Fatalf("policy did not exclude both receipts: %v", rows)
	}
	rr = callAsset(t, s, apiAssetReviewPath, wallet, policy)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var decided struct {
		Pending int   `json:"pending"`
		Reviews []Row `json:"reviews"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decided); err != nil {
		t.Fatal(err)
	}
	if decided.Pending != 0 || len(decided.Reviews) != 2 || decided.Reviews[0].AssetDecision == nil || decided.Reviews[0].AssetDecision.AppliedFrom != DecisionKeySpamPolicy || !decided.Reviews[0].AssetIdentity.Excluded {
		t.Fatalf("policy decision not materialized: %+v", decided.Reviews)
	}

	// spam_policy=valueless is the same thing as the policy key.
	q := url.Values{apiAddress: {wallet}, apiAddresses: {wallet}, apiChain: {apiMainnet}, apiStart: {apiTestPeriodStart}, apiEnd: {apiTestPeriodEnd}, apiFormat: {apiGeneric}, apiSpamPolicy: {"valueless"}}
	req := httptest.NewRequest(apiMethodGET, "/events?"+q.Encode(), nil)
	shorthand := httptest.NewRecorder()
	s.Handler().ServeHTTP(shorthand, req)
	if shorthand.Code != 200 || shorthand.Body.String() != rr.Body.String() && len(csvRecords(t, shorthand.Body.String())) != 3 {
		t.Fatal(shorthand.Code, shorthand.Body.String())
	}
	q.Set(apiSpamPolicy, "sometimes")
	bad := httptest.NewRecorder()
	s.Handler().ServeHTTP(bad, httptest.NewRequest(apiMethodGET, "/events?"+q.Encode(), nil))
	if bad.Code != 400 {
		t.Fatal("bad spam_policy accepted", bad.Code)
	}

	// A denom override prices each receipt by its own quantity, and a record
	// decision for one receipt beats the denom rule. Idle denom keys are fine;
	// idle record keys are not.
	denomKey := DecisionKeyDenomPrefix + junoFactoryUatomVoucher
	rules := AssetDecisions{
		denomKey:         {Mode: AssetDecisionOverride, Token: apiTestToken, UnitValueUSD: "0.5"},
		"denom:ibc/NOPE": {Mode: AssetDecisionExclude},
		id:               {Mode: AssetDecisionExclude},
	}
	rr = callAsset(t, s, apiEvents, wallet, rules)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rows = csvRecords(t, rr.Body.String())
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	var excluded, priced int
	for _, r := range rows[1:] {
		switch r[19] {
		case AssetDecisionExclude:
			excluded++
		case AssetDecisionOverride:
			priced++
			if r[4] != apiTestToken || r[6] != "10000" || r[8] != "5000" || r[20] != "5000" || r[21] != "5000" {
				t.Fatalf("denom override not priced per receipt: %v", r)
			}
		}
	}
	if excluded != 1 || priced != 1 {
		t.Fatalf("precedence wrong: %v", rows)
	}
	rr = callAsset(t, s, apiAssetReviewPath, wallet, rules)
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if len(review.Unmatched) != 1 || review.Unmatched[0] != "denom:ibc/NOPE" || review.Groups[0].Decision == nil || review.Groups[0].Decision.Token != apiTestToken {
		t.Fatalf("unmatched/group decision wrong: %+v %+v", review.Unmatched, review.Groups)
	}
	for name, bad := range map[string]AssetDecisions{
		"policy override":      {DecisionKeySpamPolicy: {Mode: AssetDecisionOverride, Token: "X", UnitValueUSD: "1"}},
		"denom without unit":   {denomKey: {Mode: AssetDecisionOverride, Token: "X"}},
		"denom with totals":    {denomKey: {Mode: AssetDecisionOverride, Token: "X", UnitValueUSD: "1", Quantity: "1"}},
		"record with unit":     {id: {Mode: AssetDecisionOverride, Token: "X", UnitValueUSD: "1", Quantity: "1", ValueUSD: "1", CostBasisUSD: "1", AcquiredDate: apiTestMayStart}},
		"applied_from forged":  {id: {Mode: AssetDecisionExclude, AppliedFrom: DecisionKeySpamPolicy}},
		"unknown record still": {"mainnet|" + wallet + "|999|0": {Mode: AssetDecisionExclude}},
	} {
		rr := callAsset(t, s, apiEvents, wallet, bad)
		if rr.Code != 400 {
			t.Fatalf("%s accepted: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

func fixtureSecondReceipt(t *testing.T, s *Server) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(2,1,1,31300000,'2026-05-27T08:15:44Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(2,2,'SCAMTX2',0,'Claim 10,000 ATOM at https://claim.invalid')",
		"INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(2,2,1,0)",
	} {
		if err := s.db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	event := tax.TaxableEvent{MessageID: 2, SubIndex: 0, Category: apiTransfer, FromAddr: "sender", ToAddr: "cosmos1testwallet", Amount: "10000000000", Denom: junoFactoryUatomVoucher, BlockHeight: 31300000, Timestamp: time.Date(2026, 5, 27, 8, 15, 44, 0, time.UTC), TxHash: "SCAMTX2"}
	if err := s.db.Omit(apiMessage).Create(&event).Error; err != nil {
		t.Fatal(err)
	}
}
