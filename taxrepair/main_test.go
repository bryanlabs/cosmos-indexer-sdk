package main

import (
	"bytes"
	"slices"
	"testing"
	"time"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authz "github.com/cosmos/cosmos-sdk/x/authz"
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
	old := []tax.TaxableEvent{{Category: string(tax.CategoryTransfer), Amount: "1", Denom: "uatom", FromAddr: wallet, ToAddr: contract}}
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

// The repair writes exactly the rows the indexer writes for contract
// messages inside a MsgExec, though the stored log does not name the
// granter: the indexer runs only the signer-free supplement there too.
func TestAuthzWasmSupplementMatchesIndexer(t *testing.T) {
	stream := sdk.AccAddress(bytes.Repeat([]byte{7}, 32)).String()
	market := sdk.AccAddress(bytes.Repeat([]byte{8}, 32)).String()
	granter := sdk.AccAddress(bytes.Repeat([]byte{9}, 20)).String()
	seller := sdk.AccAddress(bytes.Repeat([]byte{5}, 20)).String()
	grantee := sdk.AccAddress(bytes.Repeat([]byte{4}, 20))
	sub0 := []txlog.LogMessageEvent{
		authzEvent("transfer", "recipient", stream, "sender", granter, "amount", "3000000uatom"),
		authzEvent("execute", "_contract_address", stream),
		authzEvent("wasm", "_contract_address", stream, "action", "subscribe", "stream_id", "4"),
	}
	sub1 := []txlog.LogMessageEvent{
		authzEvent("transfer", "recipient", market, "sender", granter, "amount", "2000000uatom"),
		authzEvent("execute", "_contract_address", market),
		authzEvent("wasm-finalize-sale", "_contract_address", market, "collection", market, "token_id", "1", "price", "2000000", "denom", "uatom", "seller_recipient", seller, "nft_recipient", granter),
		authzEvent("transfer", "recipient", seller, "sender", market, "amount", "2000000uatom"),
	}
	indexed := func(i string, evs []txlog.LogMessageEvent) []txlog.LogMessageEvent {
		var out []txlog.LogMessageEvent
		for _, e := range evs {
			e.Attributes = append(append([]txlog.Attribute(nil), e.Attributes...), txlog.Attribute{Key: "authz_msg_index", Value: i}, txlog.Attribute{Key: "msg_index", Value: "0"})
			out = append(out, e)
		}
		return out
	}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{authzEvent("message", "action", "/cosmos.authz.v1beta1.MsgExec", "sender", grantee.String(), "msg_index", "0")}}
	log.Events = append(log.Events, indexed("0", sub0)...)
	log.Events = append(log.Events, indexed("1", sub1)...)
	exec := authz.NewMsgExec(grantee, []sdk.Msg{
		&wasmtypes.MsgExecuteContract{Sender: granter, Contract: stream},
		&wasmtypes.MsgExecuteContract{Sender: granter, Contract: market},
	})
	live, err := parse(&exec, log)
	require.NoError(t, err)
	sup := tax.AuthzWasmSupplementForLog(log)
	require.NotEmpty(t, sup)
	require.GreaterOrEqual(t, len(live), len(sup))
	known := live[:len(live)-len(sup)]
	require.NotEmpty(t, known)
	for i := range sup {
		require.Equal(t, rowKey(sup[i]), rowKey(live[len(known)+i]))
	}
	for _, e := range live {
		require.NotEqual(t, tax.TriggerStreamSubscribe, e.RewardTrigger)
	}

	// The same subscription signed directly gets its signer rule.
	direct, err := parse(&wasmtypes.MsgExecuteContract{Sender: granter, Contract: stream}, &txlog.LogMessage{Events: sub0})
	require.NoError(t, err)
	require.Len(t, direct, 1)
	require.Equal(t, tax.TriggerStreamSubscribe, direct[0].RewardTrigger)

	old := append([]tax.TaxableEvent(nil), known...)
	got, err := authzFromStoredLog(log, old)
	require.NoError(t, err)
	require.Len(t, got, len(live))
	for i := range live {
		require.Equal(t, rowKey(live[i]), rowKey(got[i]))
	}
}

// A MsgExec an indexer with the supplement already wrote keeps its rows, and
// a bank fallback row of an earlier repair is replaced by its marker, not
// kept beside the new one.
func TestAuthzWasmSupplementNoDoubleCount(t *testing.T) {
	contract := sdk.AccAddress(bytes.Repeat([]byte{7}, 32)).String()
	wallet := sdk.AccAddress(bytes.Repeat([]byte{9}, 20)).String()
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		authzEvent("message", "action", "/cosmos.authz.v1beta1.MsgExec", "msg_index", "0"),
		authzEvent("transfer", "recipient", contract, "sender", wallet, "amount", "3000000uatom", "authz_msg_index", "0", "msg_index", "0"),
		authzEvent("execute", "_contract_address", contract, "authz_msg_index", "0", "msg_index", "0"),
		authzEvent("wasm", "_contract_address", contract, "action", "subscribe", "stream_id", "4", "authz_msg_index", "0", "msg_index", "0"),
	}}
	live := []tax.TaxableEvent{{Category: "protocol_deposit", Amount: "3000000", Denom: "uatom", FromAddr: wallet, ToAddr: contract, RewardTrigger: tax.TriggerStreamSubscribe, Detail: `{"contract":"` + contract + `","stream_id":"4"}`}}
	got, err := authzFromStoredLog(log, live)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, rowKey(live[0]), rowKey(got[0]))

	moved := tax.TaxableEvent{Category: string(tax.CategoryTransfer), Amount: "5", Denom: "uatom", FromAddr: wallet, ToAddr: contract, RewardTrigger: "withdraw_address", Detail: `{"protocol":"distribution","reward_trigger":"claim"}`}
	stale := []tax.TaxableEvent{moved, {Category: string(tax.CategoryTransfer), Amount: "2999999", Denom: "uatom", FromAddr: wallet, ToAddr: contract, Detail: `{"contract":"` + contract + `","fallback":"bank"}`}}
	got, err = authzFromStoredLog(log, stale)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, rowKey(moved), rowKey(got[0]), "a withdraw-address move is kept")
	require.Equal(t, "3000000", got[1].Amount)
	require.Contains(t, got[1].Detail, `"fallback":"bank"`)
}

func TestReplayUpperStopsBeforeBoundary(t *testing.T) {
	got, err := replayUpper(0, defaultSupplementBoundary)
	require.NoError(t, err)
	require.Equal(t, uint(138666818), got)
	got, err = replayUpper(100, defaultSupplementBoundary)
	require.NoError(t, err)
	require.Equal(t, uint(100), got)
	_, err = replayUpper(defaultSupplementBoundary, defaultSupplementBoundary)
	require.Error(t, err)
	_, err = replayUpper(0, 0)
	require.Error(t, err)
}
