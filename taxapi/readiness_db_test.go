package taxapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// Uses the existing disposable Docker Postgres fixture, never a live tax DB.
func TestReportReadinessSemanticAndParserGates(t *testing.T) {
	s, db, wallet, _ := assetReviewFixture(t)
	decisions := AssetDecisions{DecisionKeySpamPolicy: {Mode: AssetDecisionExclude}}
	preview := waitPreview(t, s, wallet, decisions)
	checkReady := func(preview map[string]any, ready bool) {
		t.Helper()
		r, ok := preview[apiReadiness].(map[string]any)
		if !ok || r["export_ready"] != ready || preview["preview_only"] != true {
			t.Fatalf("bad preview readiness: %v", preview)
		}
	}
	checkReady(preview, true)
	claim := semanticClaimRow()
	if err := db.Model(&tax.TaxableEvent{}).Where("message_id = 1").Updates(map[string]any{apiRewardTrigger: claim.RewardTrigger, apiDetail: claim.Detail}).Error; err != nil {
		t.Fatal(err)
	}
	preview = waitPreview(t, s, wallet, decisions)
	checkReady(preview, false)
	for _, path := range []string{apiAssetReviewPath, apiEvents, apiIncome, api8949Path, apiScheduleD, api990TPath, apiJournalPath, apiPositionsPath} {
		rr := callAsset(t, s, path, wallet, decisions)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "semantic") {
			t.Fatalf("semantic gate %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	// Successful reclassification clears semantic review, but a retained parser
	// error must immediately suppress the prior cached CSV, even if stale rows
	// are still present. No value/identity decision can suppress the parser error.
	if err := db.Model(&tax.TaxableEvent{}).Where("message_id = 1").Updates(map[string]any{apiRewardTrigger: "", apiDetail: ""}).Error; err != nil {
		t.Fatal(err)
	}
	checkReady(waitPreview(t, s, wallet, decisions), true)
	parser := models.MessageParser{Identifier: "tax:/cosmos.authz.v1beta1.MsgExec"}
	if err := db.Create(&parser).Error; err != nil {
		t.Fatal(err)
	}
	parserError := models.MessageParserError{MessageParserID: parser.ID, MessageID: 1, Error: "classification incomplete: unknown authz inner"}
	if err := db.Omit(apiMessage, "MessageParser").Create(&parserError).Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{apiReportJob, apiAssetReviewPath, apiEvents, api8949Path} {
		rr := callAsset(t, s, path, wallet, decisions)
		var body map[string]any
		if rr.Code != http.StatusConflict || json.Unmarshal(rr.Body.Bytes(), &body) != nil || body[apiStatus] != "blocked" || body[apiCSV] != nil {
			t.Fatalf("incomplete prior CSV leaked %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	for _, scope := range []struct {
		chain, address string
		start, end     time.Time
	}{
		{apiTestnet, wallet, start, end},
		{apiMainnet, "cosmos1unrelated", start, end},
		{apiMainnet, wallet, start.AddDate(-1, 0, 0), start},
	} {
		r, err := s.classificationReadiness(scope.chain, []string{scope.address}, scope.start, scope.end)
		if err != nil || !r.ParserDecodeComplete {
			t.Fatalf("irrelevant error blocked scope %+v: %+v %v", scope, r, err)
		}
	}
	if err := db.Delete(&parserError).Error; err != nil {
		t.Fatal(err)
	}
	// Decode errors have the same retained-evidence wallet and period scope.
	if err := db.Create(&models.FailedMessage{TxID: 1, MessageIndex: 0}).Error; err != nil {
		t.Fatal(err)
	}
	rr := callAsset(t, s, apiReportJob, wallet, decisions)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"decode_errors":1`) {
		t.Fatalf("decode gate: %d %s", rr.Code, rr.Body.String())
	}
	if err := db.Where("tx_id = 1").Delete(&models.FailedMessage{}).Error; err != nil {
		t.Fatal(err)
	}
	checkReady(waitPreview(t, s, wallet, decisions), true)
	if err := db.Model(&models.Message{}).Where("id = 1").Update("opaque", true).Error; err != nil {
		t.Fatal(err)
	}
	rr = callAsset(t, s, apiReportJob, wallet, decisions)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"opaque_messages":1`) {
		t.Fatalf("opaque payload must block cached reports: %d %s", rr.Code, rr.Body.String())
	}
	if err := db.Model(&models.Message{}).Where("id = 1").Update("opaque", false).Error; err != nil {
		t.Fatal(err)
	}
	checkReady(waitPreview(t, s, wallet, decisions), true)
}
