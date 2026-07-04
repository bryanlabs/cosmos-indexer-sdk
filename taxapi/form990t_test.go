package taxapi

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCompute990T(t *testing.T) {
	// Under the $1,000 threshold: no filing, no tax.
	low := compute990T(decimal.NewFromInt(800), 0, RecognitionPolicyDefault)
	if low.FilingRequired || low.EstimatedTaxUSD != "0.00" || low.TaxableUBTI != "0.00" {
		t.Fatalf("under-threshold wrong: %+v", low)
	}
	// $5,000 UBTI: taxable = 4,000; trust tax = 3100*10% + 900*24% = 310 + 216 = 526.
	hi := compute990T(decimal.NewFromInt(5000), 0, RecognitionPolicyDefault)
	if !hi.FilingRequired || hi.TaxableUBTI != "4000.00" || hi.EstimatedTaxUSD != "526.00" {
		t.Fatalf("over-threshold wrong: %+v", hi)
	}
}

func TestCompute990TFlagsMissingPrices(t *testing.T) {
	got := compute990T(decimal.NewFromInt(800), 3, RecognitionPolicyDefault)
	if got.PriceMissingRows != 3 {
		t.Fatalf("want PriceMissingRows=3, got %+v", got)
	}
	if !strings.Contains(got.Note, "excluded") {
		t.Fatalf("note should mention the gap: %q", got.Note)
	}
}

func TestCompute990TPrintsRecognitionPolicy(t *testing.T) {
	got := compute990T(decimal.NewFromInt(2000), 0, "at-claim")
	if got.RecognitionPolicy != "at-claim" {
		t.Fatalf("want recognition_policy echoed, got %+v", got)
	}
}
