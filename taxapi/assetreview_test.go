package taxapi

import (
	"strings"
	"testing"
)

func TestDuplicateDecisionFieldsRejected(t *testing.T) {
	for _, input := range []string{`{"id":{"mode":"exclude","mode":"override"}}`, `{"id":{"mode":"exclude"},"id":{"mode":"exclude"}}`} {
		if _, err := ParseAssetDecisions(input); err == nil {
			t.Fatal("ambiguous duplicate accepted")
		}
	}
}

func TestParseAssetDecisionsAcceptsArbitraryTickerAndZeroTotals(t *testing.T) {
	decisions, err := ParseAssetDecisions(`{"chain|wallet|message|0":{"mode":"override","token":"  mytoken  ","quantity":"1.25","value_usd":"0","cost_basis_usd":"0.00","acquired_date":"2026-02-03"}}`)
	if err != nil {
		t.Fatal(err)
	}
	got := decisions["chain|wallet|message|0"]
	if got.Token != "MYTOKEN" || got.Quantity != "1.25" || got.ValueUSD != "0" || got.CostBasisUSD != "0.00" {
		t.Fatalf("override was changed: %+v", got)
	}
}

func TestParseAssetDecisionsRejectsUnsafeNumbers(t *testing.T) {
	for _, value := range []string{"-1", "NaN", "Infinity", "1e10", "1E10", ".1", "1.", strings.Repeat("9", 39), "0." + strings.Repeat("1", 19)} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseAssetDecisions(`{"id":{"mode":"override","token":"MYTOKEN","quantity":"` + value + `","value_usd":"0","cost_basis_usd":"0","acquired_date":"2026-02-03"}}`)
			if err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
}

func TestParseAssetDecisionsRejectsMixedExclude(t *testing.T) {
	_, err := ParseAssetDecisions(`{"id":{"mode":"exclude","token":"MYTOKEN"}}`)
	if err == nil {
		t.Fatal("exclude with override data accepted")
	}
}

func TestAbsenceMeansUnreviewedNotExcluded(t *testing.T) {
	decisions, err := ParseAssetDecisions(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("unexpected decisions: %+v", decisions)
	}
	if _, found := decisions["unreviewed-record"]; found {
		t.Fatal("absence was treated as a decision")
	}
}

func TestAssetDecisionsAreCanonicalAndSorted(t *testing.T) {
	a := AssetDecisions{
		"z": {Mode: AssetDecisionOverride, Token: "mytoken", Quantity: "1", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: "2026-02-03"},
		"a": {Mode: AssetDecisionExclude},
	}
	b := AssetDecisions{
		"a": {Mode: AssetDecisionExclude},
		"z": {Mode: AssetDecisionOverride, Token: "MYTOKEN", Quantity: "1", ValueUSD: "0", CostBasisUSD: "0", AcquiredDate: "2026-02-03"},
	}
	left, err := CanonicalAssetDecisionsJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalAssetDecisionsJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if left != right || !strings.HasPrefix(left, `{"a":`) || !strings.Contains(left, `"token":"MYTOKEN"`) {
		t.Fatalf("noncanonical JSON: %s / %s", left, right)
	}
}

func TestParseAssetDecisionsRejectsUnknownFieldsAndBounds(t *testing.T) {
	for _, input := range []string{
		`{"id":{"mode":"exclude","unknown":true}}`,
		`{"` + strings.Repeat("x", MaxAssetDecisionRecordID+1) + `":{"mode":"exclude"}}`,
	} {
		if _, err := ParseAssetDecisions(input); err == nil {
			t.Fatalf("accepted invalid input %q", input)
		}
	}
}
