package taxapi

import (
	"testing"
	"time"
)

func TestCanonicalReportKeySameAddressSetAnyOrderCaseWhitespace(t *testing.T) {
	a := canonicalReportKey("mainnet", []string{"cosmos1Abc", " cosmos1Def "}, "2026-01-01", "2026-02-01", "summ")
	b := canonicalReportKey("mainnet", []string{"cosmos1def", "cosmos1abc"}, "2026-01-01", "2026-02-01", "summ")
	if a != b {
		t.Fatalf("expected the same key regardless of order/case/whitespace, got %q vs %q", a, b)
	}
}

func TestCanonicalReportKeyDiffersOnAnyDimension(t *testing.T) {
	base := canonicalReportKey("mainnet", []string{"cosmos1abc"}, "2026-01-01", "2026-02-01", "summ")
	cases := map[string]string{
		"chain":   canonicalReportKey("testnet", []string{"cosmos1abc"}, "2026-01-01", "2026-02-01", "summ"),
		"address": canonicalReportKey("mainnet", []string{"cosmos1xyz"}, "2026-01-01", "2026-02-01", "summ"),
		"start":   canonicalReportKey("mainnet", []string{"cosmos1abc"}, "2026-01-02", "2026-02-01", "summ"),
		"end":     canonicalReportKey("mainnet", []string{"cosmos1abc"}, "2026-01-01", "2026-02-02", "summ"),
		"format":  canonicalReportKey("mainnet", []string{"cosmos1abc"}, "2026-01-01", "2026-02-01", "koinly"),
	}
	for dim, key := range cases {
		if key == base {
			t.Fatalf("changing %s should change the key, both were %q", dim, key)
		}
	}
}

func TestCanonicalReportKeyDropsEmptyAddresses(t *testing.T) {
	a := canonicalReportKey("mainnet", []string{"cosmos1abc", "", "  "}, "", "", "summ")
	b := canonicalReportKey("mainnet", []string{"cosmos1abc"}, "", "", "summ")
	if a != b {
		t.Fatalf("blank entries should be dropped, got %q vs %q", a, b)
	}
}

func TestReportIsStalePastMaxAgeWithoutTouchingDB(t *testing.T) {
	s := &Server{} // no db: this must short-circuit on the max-age check alone
	job := &ReportJob{ComputedThrough: nowUTC().Add(-25 * time.Hour)}
	if !s.reportIsStale(job, []string{"cosmos1abc"}) {
		t.Fatalf("a report older than reportMaxAge must be stale regardless of activity")
	}
}

func TestSplitNonEmpty(t *testing.T) {
	got := splitNonEmpty(" cosmos1abc , ,cosmos1def,  ", ",")
	want := []string{"cosmos1abc", "cosmos1def"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}
