package main

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func TestCanonicalComparisonAndRewardTotals(t *testing.T) {
	now := time.Now().UTC()
	expected := []tax.TaxableEvent{{MessageID: 7, SubIndex: 0, Category: "reward", ToAddr: "wallet", Amount: "2", Denom: "uatom", RewardTrigger: "claim", Timestamp: now}}
	// Database IDs are deliberately irrelevant, making a second repair a no-op.
	got := append([]tax.TaxableEvent(nil), expected...)
	got[0].ID = 99
	if !same(got, expected) {
		t.Fatal("canonical rows with a different primary key must compare equal")
	}
	stale := append([]tax.TaxableEvent(nil), expected...)
	stale = append(stale, tax.TaxableEvent{MessageID: 7, SubIndex: 1, Category: "reward"})
	if same(stale, expected) {
		t.Fatal("a stale sub-index must require deletion")
	}
	totals := totals{}
	add(totals, append(expected, tax.TaxableEvent{Category: "reward", ToAddr: "wallet", Amount: "3", Denom: "uatom", RewardTrigger: "claim"}))
	if totals["wallet"]["uatom"]["claim"] != "5" {
		t.Fatalf("aggregate = %q", totals["wallet"]["uatom"]["claim"])
	}
}
