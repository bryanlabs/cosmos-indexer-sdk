package taxapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func semanticClaimRow() Row {
	return Row{
		Time: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), Category: apiTransfer, Direction: "in", Denom: apiClaimToken, Symbol: "CLAIM", Amount: decimal.NewFromInt(10), TxHash: "CLAIMTX", RewardTrigger: apiUnknownClaim, Detail: `{"review_required":"true","review_reason":"unknown_claim_semantics","action":"claim","contract":"unreviewed-contract"}`,
		AssetIdentity: &AssetIdentity{RecordID: "claim-row", RawDenom: apiClaimToken, SuspectedSpam: true},
	}
}

func TestSemanticReviewIndependentOfAssetDecisionsAndTags(t *testing.T) {
	for _, decisions := range []AssetDecisions{
		nil,
		{"claim-row": {Mode: AssetDecisionExclude}},
		{DecisionKeyDenomPrefix + apiClaimToken: {Mode: AssetDecisionOverride, Token: "CLAIM", UnitValueUSD: "2"}},
		{DecisionKeySpamPolicy: {Mode: AssetDecisionExclude}},
	} {
		rows, err := ApplyAssetDecisions([]Row{semanticClaimRow()}, decisions)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{"", TagIncome, TagSpam, TagProtocolDeposit} {
			rows[0].UserTag = tag
			if PendingSemanticReviews(rows) != 1 || readinessForRows(rows).ExportReady {
				t.Fatalf("decision/tag cleared semantic review: %+v %s", decisions, tag)
			}
			for _, format := range []string{apiGeneric, apiSumm, apiCryptotaxcalculator, apiBitwave} {
				var out strings.Builder
				var blocked reportReadinessRequired
				if err := WriteCSV(&out, format, rows); !errors.As(err, &blocked) || out.Len() != 0 {
					t.Fatalf("export leaked %s: %v %s", format, err, out.String())
				}
			}
		}
		var out strings.Builder
		if err := WritePreviewCSV(&out, apiSumm, rows); err != nil {
			t.Fatal(err)
		}
		records := csvRecords(t, out.String())
		columns := map[string]int{}
		for i, header := range records[0] {
			columns[header] = i
		}
		if records[1][columns[apiTaxable]] != apiUnknown || records[1][columns[apiPreviewDetail]] != rows[0].Detail {
			t.Fatal("preview certified claim semantics or lost evidence")
		}
	}
}

func TestSemanticReviewFallbackAndFingerprint(t *testing.T) {
	row := semanticClaimRow()
	row.AssetIdentity = nil
	original := reviewFingerprint([]Row{row})
	row.Detail = "malformed detail"
	if PendingSemanticReviews([]Row{row}) != 1 {
		t.Fatal("trigger must retain blocker when detail is malformed")
	}
	row.RewardTrigger = ""
	row.Detail = `{"review_required":"true","review_reason":"other_unresolved_semantics"}`
	if PendingSemanticReviews([]Row{row}) != 1 {
		t.Fatal("explicit semantic marker ignored")
	}
	row.Detail = ""
	if PendingSemanticReviews([]Row{row}) != 0 || reviewFingerprint([]Row{row}) == original {
		t.Fatal("review change must invalidate cache")
	}
}

func TestReadinessErrorIsExplicitConflict(t *testing.T) {
	readiness := readinessForRows([]Row{semanticClaimRow()})
	rr := httptest.NewRecorder()
	writeTaxError(rr, reportReadinessRequired{readiness})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"export_ready":false`) || !strings.Contains(rr.Body.String(), `"status":"blocked"`) {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}
