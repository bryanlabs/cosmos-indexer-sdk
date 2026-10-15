package main

import (
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
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
	if totals["reward"]["uatom"]["claim"] != "5" {
		t.Fatalf("aggregate = %q", totals["reward"]["uatom"]["claim"])
	}
	changed := append([]tax.TaxableEvent(nil), expected...)
	changed[0].Detail = `{"x":"1"}`
	if same(changed, expected) {
		t.Fatal("a changed detail must count as a difference")
	}
}

func TestTrivialLog(t *testing.T) {
	noop := &txlog.LogMessage{Events: []txlog.LogMessageEvent{{Type: "message"}, {Type: "tx"}}}
	if !trivialLog(noop) {
		t.Fatal("message-only log must be trivial")
	}
	moved := &txlog.LogMessage{Events: []txlog.LogMessageEvent{{Type: "message"}, {Type: "coin_received"}}}
	if trivialLog(moved) {
		t.Fatal("coin movement is not trivial")
	}
}
