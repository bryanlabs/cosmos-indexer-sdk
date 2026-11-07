package taxapi

import (
	"strings"
	"testing"
	"time"
)

func TestReportCacheIncludesCorrectedRewardVersion(t *testing.T) {
	key := canonicalReportKey(apiMainnet, []string{apiTestGenericWallet}, apiTestPeriodStart, apiTestPeriodEnd, apiSumm)
	if !strings.HasPrefix(key, MethodologyVersion+"|") || strings.HasPrefix(key, "v2|") {
		t.Fatalf("old inflated reward reports must not be reused: %s", key)
	}
}

func TestCanonicalReportKeySameAddressSetAnyOrderCaseWhitespace(t *testing.T) {
	a := canonicalReportKey(apiMainnet, []string{"cosmos1Abc", " cosmos1Def "}, apiTestPeriodStart, apiTestFebruaryStart, apiSumm)
	b := canonicalReportKey(apiMainnet, []string{"cosmos1def", apiTestWallet}, apiTestPeriodStart, apiTestFebruaryStart, apiSumm)
	if a != b {
		t.Fatalf("expected the same key regardless of order/case/whitespace, got %q vs %q", a, b)
	}
}

func TestCanonicalReportKeyDiffersOnAnyDimension(t *testing.T) {
	base := canonicalReportKey(apiMainnet, []string{apiTestWallet}, apiTestPeriodStart, apiTestFebruaryStart, apiSumm)
	cases := map[string]string{
		apiChain:   canonicalReportKey(apiTestnet, []string{apiTestWallet}, apiTestPeriodStart, apiTestFebruaryStart, apiSumm),
		apiAddress: canonicalReportKey(apiMainnet, []string{"cosmos1xyz"}, apiTestPeriodStart, apiTestFebruaryStart, apiSumm),
		apiStart:   canonicalReportKey(apiMainnet, []string{apiTestWallet}, "2026-01-02", apiTestFebruaryStart, apiSumm),
		apiEnd:     canonicalReportKey(apiMainnet, []string{apiTestWallet}, apiTestPeriodStart, "2026-02-02", apiSumm),
		apiFormat:  canonicalReportKey(apiMainnet, []string{apiTestWallet}, apiTestPeriodStart, apiTestFebruaryStart, apiKoinly),
	}
	for dim, key := range cases {
		if key == base {
			t.Fatalf("changing %s should change the key, both were %q", dim, key)
		}
	}
}

func TestCanonicalReportKeyDropsEmptyAddresses(t *testing.T) {
	a := canonicalReportKey(apiMainnet, []string{apiTestWallet, "", "  "}, "", "", apiSumm)
	b := canonicalReportKey(apiMainnet, []string{apiTestWallet}, "", "", apiSumm)
	if a != b {
		t.Fatalf("blank entries should be dropped, got %q vs %q", a, b)
	}
}

func TestReportIsStalePastMaxAgeWithoutTouchingDB(t *testing.T) {
	s := &Server{} // no db: this must short-circuit on the max-age check alone
	job := &ReportJob{ComputedThrough: nowUTC().Add(-25 * time.Hour)}
	if !s.reportIsStale(job, []string{apiTestWallet}) {
		t.Fatalf("a report older than reportMaxAge must be stale regardless of activity")
	}
}

func TestSplitNonEmpty(t *testing.T) {
	got := splitNonEmpty(" cosmos1abc , ,cosmos1def,  ", ",")
	want := []string{apiTestWallet, "cosmos1def"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}
