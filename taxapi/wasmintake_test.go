package taxapi

import (
	"testing"
	"time"
)

func TestSubmissionLimiterAllowsUpToLimit(t *testing.T) {
	l := &submissionLimiter{hits: map[string][]time.Time{}}
	key := "1.2.3.4|cosmos1contract"
	for i := 0; i < submissionRateLimit; i++ {
		if !l.allow(key) {
			t.Fatalf("submission %d should be allowed (limit is %d)", i+1, submissionRateLimit)
		}
	}
	if l.allow(key) {
		t.Fatal("submission beyond the limit should be rejected")
	}
}

func TestSubmissionLimiterIsPerKey(t *testing.T) {
	l := &submissionLimiter{hits: map[string][]time.Time{}}
	for i := 0; i < submissionRateLimit; i++ {
		if !l.allow("1.2.3.4|contractA") {
			t.Fatalf("contractA submission %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4|contractA") {
		t.Fatal("contractA should now be rate-limited")
	}
	if !l.allow("1.2.3.4|contractB") {
		t.Fatal("a different contract from the same client should have its own quota")
	}
}

func TestValidWasmStatusRejectsUnknownValues(t *testing.T) {
	for _, s := range []string{WasmSubmissionSubmitted, WasmSubmissionUnderReview, WasmSubmissionApproved, WasmSubmissionDeclined} {
		if !validWasmStatus[s] {
			t.Fatalf("status %q should be valid", s)
		}
	}
	if validWasmStatus["deleted"] || validWasmStatus[""] {
		t.Fatal("unknown statuses should not validate")
	}
}
