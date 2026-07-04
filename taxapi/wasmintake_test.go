package taxapi

import (
	"testing"
	"time"
)

func TestSubmissionLimiterAllowsUpToLimit(t *testing.T) {
	l := newLimiter(submissionRateLimit, submissionRateWindow)
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
	l := newLimiter(submissionRateLimit, submissionRateWindow)
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

// Two limiter instances with different configured limits must not share a
// quota (regression: an earlier version hardcoded the wasm-submission limit
// into the shared allow() method, so a differently-configured limiter like
// delegatorReportLimiter silently used the wrong limit).
func TestLimitersWithDifferentConfigsAreIndependent(t *testing.T) {
	strict := newLimiter(2, time.Hour)
	generous := newLimiter(30, time.Minute)

	for i := 0; i < 2; i++ {
		if !strict.allow("client") {
			t.Fatalf("strict limiter call %d should be allowed", i+1)
		}
	}
	if strict.allow("client") {
		t.Fatal("strict limiter should reject the 3rd call (limit is 2)")
	}
	for i := 0; i < 30; i++ {
		if !generous.allow("client") {
			t.Fatalf("generous limiter call %d should be allowed (limit is 30)", i+1)
		}
	}
	if generous.allow("client") {
		t.Fatal("generous limiter should reject the 31st call")
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
