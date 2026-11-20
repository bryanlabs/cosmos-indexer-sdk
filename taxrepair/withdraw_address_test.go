package main

import (
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	waDistribution = "cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl"
	waDelegator    = "cosmos1delegator"
	waWithdraw     = "cosmos1withdraw"
	waValidator    = "cosmosvaloper1validator"
	waClaimURL     = "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward"
	waClaim        = "claim"
	waDenom        = "uatom"
)

func waEvent(typ string, kv ...string) txlog.LogMessageEvent {
	e := txlog.LogMessageEvent{Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Attributes = append(e.Attributes, txlog.Attribute{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

// waClaimLog is a stored claim log whose reward the chain paid to recipient.
func waClaimLog(recipient string) *txlog.LogMessage {
	return &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		waEvent("message", "action", waClaimURL, "sender", waDelegator, "module", "distribution"),
		waEvent("transfer", "recipient", recipient, "sender", waDistribution, "amount", "627uatom"),
		waEvent("withdraw_rewards", "amount", "627uatom", "validator", waValidator, "delegator", waDelegator),
	}}
}

// waSplitLog is a claim log whose reward the module paid in two parts.
func waSplitLog(first, second string) *txlog.LogMessage {
	return &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		waEvent("message", "action", waClaimURL, "sender", waDelegator, "module", "distribution"),
		waEvent("transfer", "recipient", first, "sender", waDistribution, "amount", "300uatom"),
		waEvent("transfer", "recipient", second, "sender", waDistribution, "amount", "327uatom"),
		waEvent("withdraw_rewards", "amount", "627uatom", "validator", waValidator, "delegator", waDelegator),
	}}
}

func waStored(id uint) []tax.TaxableEvent {
	return []tax.TaxableEvent{{ID: 900, MessageID: id, SubIndex: 0, Category: categoryReward, Amount: "627", Denom: waDenom, ToAddr: waDelegator, ValidatorAddress: waValidator, RewardTrigger: waClaim}}
}

func TestWithdrawAddressPlanAppendsParserMoves(t *testing.T) {
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	m := waMessage{ID: 50, Type: waClaimURL, Height: 24000000, Stamp: stamp, Hash: "HASH"}
	class, rows, _ := planWithdrawAddressMessage(codec, m, waClaimLog(waWithdraw), waStored(50))
	if class != waExact || len(rows) != 1 {
		t.Fatalf("class %s rows %d", class, len(rows))
	}
	r := rows[0]
	if r.MessageID != 50 || r.SubIndex != 1 || r.Category != string(tax.CategoryTransfer) || r.FromAddr != waDelegator || r.ToAddr != waWithdraw ||
		r.Amount != "627" || r.Denom != waDenom || r.RewardTrigger != tax.TriggerWithdrawAddress || r.Detail != `{"protocol":"distribution","reward_trigger":"claim"}` ||
		r.BlockHeight != 24000000 || !r.Timestamp.Equal(stamp) || r.TxHash != "HASH" || r.ValidatorAddress != "" {
		t.Fatalf("planned row %+v", r)
	}
	// The plan is exactly what a fresh parse indexes after the stored rows.
	fresh, err := parse(mustMsg(t, m, waClaimLog(waWithdraw)), waClaimLog(waWithdraw))
	if err != nil || len(fresh) != 2 || !sameContent(fresh[1:], []tax.TaxableEvent{r}) {
		t.Fatalf("fresh parse %+v %v", fresh, err)
	}
	if waTrigger(r) != waClaim {
		t.Fatal("trigger from detail")
	}
}

func mustMsg(t *testing.T, m waMessage, log *txlog.LogMessage) sdk.Msg {
	t.Helper()
	msg, err := tax.ReconstructMsg(m.Type, log)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestWithdrawAddressPlanClasses(t *testing.T) {
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	m := waMessage{ID: 51, Type: waClaimURL}
	_, planned, _ := planWithdrawAddressMessage(codec, m, waClaimLog(waWithdraw), waStored(51))
	if len(planned) != 1 {
		t.Fatal("plan")
	}
	moved := append(waStored(51), planned...)
	movedDrift := append(waStored(51), planned...)
	movedDrift[0].Detail = `{"old":"1"}`
	wrongMove := append(waStored(51), tax.TaxableEvent{MessageID: 51, SubIndex: 1, Category: string(tax.CategoryTransfer), RewardTrigger: tax.TriggerWithdrawAddress})
	otherDetail := waStored(51)
	otherDetail[0].Detail = `{"old":"1"}`
	otherAmount := waStored(51)
	otherAmount[0].Amount = "600"
	gap := waStored(51)
	gap[0].SubIndex = 1
	for _, c := range []struct {
		name   string
		m      waMessage
		log    *txlog.LogMessage
		stored []tax.TaxableEvent
		class  string
		rows   int
	}{
		{"unredirected", m, waClaimLog(waDelegator), waStored(51), waNoMoves, 0},
		{"rerun", m, waClaimLog(waWithdraw), moved, waAlreadyExact, 0},
		{"rerun, other rows predate the parser", m, waClaimLog(waWithdraw), movedDrift, waAlreadyMoves, 0},
		{"rerun, stored move differs", m, waClaimLog(waWithdraw), wrongMove, waAlreadyMismatch, 0},
		{"rerun, authz without bytes", waMessage{ID: 51, Type: typeURLAuthzExec}, waClaimLog(waWithdraw), moved, waAlready, 0},
		{"other stored row differs", m, waClaimLog(waWithdraw), otherDetail, waRewardsEqual, 1},
		{"stored reward differs", m, waClaimLog(waWithdraw), otherAmount, waRewardsDrift, 0},
		{"sub_index gap", m, waClaimLog(waWithdraw), gap, waGap, 0},
		{"private profile", waMessage{ID: privateProfileMessageID, Type: waClaimURL}, waClaimLog(waWithdraw), waStored(privateProfileMessageID), waPrivate, 0},
		{"failed tx", waMessage{ID: 51, Type: waClaimURL, Code: 5}, waClaimLog(waWithdraw), waStored(51), waFailedTx, 0},
		{"authz without bytes", waMessage{ID: 51, Type: typeURLAuthzExec}, waClaimLog(waWithdraw), waStored(51), waNoBody, 0},
		{"no stored log", m, nil, waStored(51), waParseError, 0},
		{"paid two other accounts", m, waSplitLog(waWithdraw, "cosmos1second"), waStored(51), waRefusedMany, 0},
		{"paid the delegator and another", m, waSplitLog(waDelegator, waWithdraw), waStored(51), waRefusedSelf, 0},
	} {
		class, rows, _ := planWithdrawAddressMessage(codec, c.m, c.log, c.stored)
		if class != c.class || len(rows) != c.rows {
			t.Errorf("%s: class %s rows %d, want %s %d", c.name, class, len(rows), c.class, c.rows)
		}
	}
}

func TestWithdrawAddressPlanLineEscapes(t *testing.T) {
	r := waPlanRow{TaxableEvent: tax.TaxableEvent{MessageID: 1, SubIndex: 2, Category: "transfer", Detail: "a\tb\\c\nd", Timestamp: time.Unix(0, 0)}, Class: waExact, StoredRows: 2, StoredFinger: "f", MessageTypeID: 7}
	got := waPlanLine(r)
	want := "1\t2\ttransfer\t\t\t\t\t\t0\t1970-01-01T00:00:00Z\t\t\t\ta\\tb\\\\c\\nd\texact\t\t2\tf\t7\n"
	if got != want {
		t.Fatalf("line %q", got)
	}
}
