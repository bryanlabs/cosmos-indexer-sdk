//go:build ops818harness

package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOPS818ArchiveExactOpenings(t *testing.T) {
	s := connect(t, "jit=off", "statement_timeout=90s", "lock_timeout=2s")
	a, err := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b, _ := json.MarshalIndent(a.evidence(nil), "", "  ")
		_ = os.WriteFile(os.Getenv("H_ARCHIVE_OUTPUT")+".receipts", b, 0600)
	})
	type result struct {
		Address  string
		Day      string
		Holdings flowHoldings
		Receipts []FlowArchiveReceipt
	}
	var results []result
	for _, path := range strings.Split(os.Getenv("H_OPENINGS_FILES"), ",") {
		if path == "" {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if len(r) < 8 || r[7] != "false" {
				continue
			}
			since, err := time.Parse(time.DateOnly, r[6])
			if err != nil {
				t.Fatal(err)
			}
			day := since.AddDate(0, 0, -1)
			h, err := a.holdingsAt(s, apiMainnet, r[0], day)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result{Address: r[0], Day: day.Format(time.DateOnly), Holdings: h, Receipts: a.evidence(map[string]bool{r[0]: true})})
			t.Log("exact archived opening recovered", len(results), "day", day.Format(time.DateOnly))
		}
	}
	for _, v := range strings.Split(os.Getenv("H_ARCHIVE_VALS"), ",") {
		if v == "" {
			continue
		}
		addr := operatorAccount(v)
		day := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
		h, err := a.holdingsAt(s, apiMainnet, addr, day)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result{Address: addr, Day: day.Format(time.DateOnly), Holdings: h, Receipts: a.evidence(map[string]bool{addr: true})})
		t.Log("exact archived operator opening recovered", len(results))
	}
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("H_ARCHIVE_OUTPUT"), b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("exact opening proofs", len(results))
}
