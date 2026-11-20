package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
)

// A MsgExec stored with its bytes that holds a reward withdrawal paid to a
// withdraw address and a contract execute replays, through planBatch and
// applyPlans, to the reward, then the contract's supplement rows, then the
// move to the withdraw address last, the order the parser writes so neither
// addition shifts an existing row. A second replay keeps
// all of them, and the withdraw-address planner finds the move already there.
func TestRepairReplayMsgExecKeepsMovesAndSupplement(t *testing.T) {
	db := waLoadScratch(t)
	contract := sdk.AccAddress(bytes.Repeat([]byte{7}, 32)).String()
	payee := sdk.AccAddress(bytes.Repeat([]byte{6}, 20)).String()
	claim, err := codectypes.NewAnyWithValue(&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: waLoadDelegator, ValidatorAddress: waValidator})
	if err != nil {
		t.Fatal(err)
	}
	execute, err := codectypes.NewAnyWithValue(&wasmtypes.MsgExecuteContract{Sender: waLoadDelegator, Contract: contract})
	if err != nil {
		t.Fatal(err)
	}
	body, err := (&authz.MsgExec{Grantee: waLoadWithdraw, Msgs: []*codectypes.Any{claim, execute}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const id = 90
	stamp := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{sqlSeedBlock, []any{id, 24000000 + id, stamp}},
		{sqlSeedTx, []any{id, id, "HASHEXEC"}},
		{"INSERT INTO message_types VALUES(2, ?)", []any{typeURLAuthzExec}},
		{"INSERT INTO messages VALUES(?,?,2,0,?)", []any{id, id, body}},
		{
			"INSERT INTO taxable_events(message_id,sub_index,category,amount,denom,asset,from_addr,to_addr,block_height,timestamp,tx_hash,validator_address,reward_trigger,detail) VALUES(?,0,'reward','627','uatom','','',?,?,?,'HASHEXEC',?,'authz','{\"stale\":true}')",
			[]any{id, waLoadDelegator, 24000000 + id, stamp, waValidator},
		},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	in := func(i string, e txlog.LogMessageEvent) txlog.LogMessageEvent {
		e.Attributes = append(e.Attributes, txlog.Attribute{Key: "authz_msg_index", Value: i}, txlog.Attribute{Key: attrMsgIndex, Value: "0"})
		return e
	}
	seedWaEvents(t, db, id, []txlog.LogMessageEvent{
		waEvent("message", "action", typeURLAuthzExec, "sender", waLoadWithdraw, "msg_index", "0"),
		in("0", waEvent("transfer", "recipient", waLoadWithdraw, "sender", waDistribution, "amount", "627uatom")),
		in("0", waEvent("withdraw_rewards", "amount", "627uatom", "validator", waValidator, "delegator", waLoadDelegator)),
		in("1", waEvent("execute", "_contract_address", contract)),
		in("1", waEvent("wasm", "_contract_address", contract, "action", "payout")),
		in("1", waEvent("transfer", "recipient", payee, "sender", contract, "amount", "5000000uatom")),
	})
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	replay := func() []backup {
		t.Helper()
		rows, err := load(db, 0, 100, 100, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("load rows %d %v", len(rows), err)
		}
		plans, skips, err := planBatch(db, codec, rows)
		if err != nil || len(skips) != 0 {
			t.Fatalf("plan %v %v", skips, err)
		}
		if err := applyPlans(db, plans); err != nil {
			t.Fatal(err)
		}
		return plans
	}
	plans := replay()
	if len(plans) != 1 {
		t.Fatalf("%d plans", len(plans))
	}
	var stored []tax.TaxableEvent
	if err := db.Where("message_id = ?", id).Order("sub_index").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range stored {
		sup, _ := tax.SupplementRow(e)
		switch {
		case sup && e.Amount == "5000000" && e.FromAddr == contract && e.ToAddr == payee:
			kinds = append(kinds, "supplement")
		case e.RewardTrigger == tax.TriggerWithdrawAddress && e.FromAddr == waLoadDelegator && e.ToAddr == waLoadWithdraw && e.Amount == "627":
			kinds = append(kinds, "move")
		case e.Category == "reward" && e.ToAddr == waLoadDelegator && e.Amount == "627":
			kinds = append(kinds, "reward")
		default:
			kinds = append(kinds, "other")
		}
		if strings.Contains(e.Detail, "stale") {
			t.Fatal("the replay kept the stale row")
		}
	}
	if strings.Join(kinds, ",") != "reward,supplement,move" {
		t.Fatalf("after the replay the rows are %v", kinds)
	}
	if again := replay(); len(again) != 0 {
		t.Fatalf("a second replay planned %d changes", len(again))
	}
	if n := waRowCount(t, db, "message_id = 90"); n != len(stored) {
		t.Fatalf("%d rows after the second replay, %d before", n, len(stored))
	}
	planned, counts, err := planWithdrawAddressBatch(db, codec, []uint{id}, nil)
	if err != nil || len(planned) != 0 || counts[waAlreadyExact] != 1 {
		t.Fatalf("withdraw-address plan after the replay: %d rows %v %v", len(planned), counts, err)
	}
}
