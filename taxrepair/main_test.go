package main

import (
	"bytes"
	"slices"
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

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

func authzEvent(typ string, kv ...string) txlog.LogMessageEvent {
	e := txlog.LogMessageEvent{Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Attributes = append(e.Attributes, txlog.Attribute{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

// A contract message inside a MsgExec gets the supplement rows the indexer
// writes for it, after every other row, and a second replay is a no-op.
func TestAuthzWasmSupplementAppendedOnce(t *testing.T) {
	contract := sdk.AccAddress(bytes.Repeat([]byte{7}, 32)).String()
	wallet := sdk.AccAddress(bytes.Repeat([]byte{9}, 20)).String()
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		authzEvent("message", "action", "/cosmos.authz.v1beta1.MsgExec", "msg_index", "0"),
		authzEvent("execute", "_contract_address", contract, "authz_msg_index", "0", "msg_index", "0"),
		authzEvent("wasm", "_contract_address", contract, "action", "payout", "authz_msg_index", "0", "msg_index", "0"),
		authzEvent("transfer", "recipient", wallet, "sender", contract, "amount", "5000000uatom", "authz_msg_index", "0", "msg_index", "0"),
	}}
	old := []tax.TaxableEvent{{Category: "transfer", Amount: "1", Denom: "uatom", FromAddr: wallet, ToAddr: contract}}
	got, err := authzFromStoredLog(log, old)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, old[0].Amount, got[0].Amount)
	require.Equal(t, "5000000", got[1].Amount)
	require.Equal(t, contract, got[1].FromAddr)
	require.Equal(t, wallet, got[1].ToAddr)
	require.Contains(t, got[1].Detail, `"fallback":"bank"`)

	again, err := authzFromStoredLog(log, got)
	require.NoError(t, err)
	require.Equal(t, len(got), len(again))
	for i := range got {
		require.Equal(t, rowKey(got[i]), rowKey(again[i]))
	}

	// Events of the MsgExec itself, with no inner index, add nothing.
	plain := &txlog.LogMessage{Events: log.Events[:1]}
	got, err = authzFromStoredLog(plain, old)
	require.NoError(t, err)
	require.Len(t, got, 1)
}
