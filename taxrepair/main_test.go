package main

import (
	"slices"
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func TestCanonicalComparisonAndRewardTotals(t *testing.T) {
	now := time.Now().UTC()
	expected := []tax.TaxableEvent{{MessageID: 7, SubIndex: 0, Category: categoryReward, ToAddr: fixtureWallet, Amount: "2", Denom: "uatom", RewardTrigger: "claim", Timestamp: now}}
	// Database IDs are deliberately irrelevant, making a second repair a no-op.
	got := append([]tax.TaxableEvent(nil), expected...)
	got[0].ID = 99
	if !same(got, expected) {
		t.Fatal("canonical rows with a different primary key must compare equal")
	}
	stale := append([]tax.TaxableEvent(nil), expected...)
	stale = append(stale, tax.TaxableEvent{MessageID: 7, SubIndex: 1, Category: categoryReward})
	if same(stale, expected) {
		t.Fatal("a stale sub-index must require deletion")
	}
	totals := totals{}
	add(totals, append(expected, tax.TaxableEvent{Category: categoryReward, ToAddr: fixtureWallet, Amount: "3", Denom: "uatom", RewardTrigger: "claim"}))
	if totals[categoryReward]["uatom"]["claim"] != "5" {
		t.Fatalf("aggregate = %q", totals[categoryReward]["uatom"]["claim"])
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

func TestRepairCodecDecodesLegacyTokenizeShareRewards(t *testing.T) {
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{0x0a, byte(len(fixtureWallet))}, fixtureWallet...)
	var msg sdk.Msg
	if err := codec.UnpackAny(&codectypes.Any{TypeUrl: tax.LegacyTypeURLWithdrawAllTokenizeShareReward, Value: wire}, &msg); err != nil {
		t.Fatal(err)
	}
	if m, ok := msg.(*tax.LegacyMsgWithdrawAllTokenizeShareRecordReward); !ok || m.OwnerAddress != fixtureWallet {
		t.Fatalf("decoded %T %+v", msg, msg)
	}
	for _, u := range []string{tax.LegacyTypeURLRedeemTokensForShares, tax.LegacyTypeURLWithdrawTokenizeShareReward, tax.LegacyTypeURLWithdrawAllTokenizeShareReward} {
		if !slices.Contains(urls, u) {
			t.Fatalf("%s is not replayable", u)
		}
	}
}
