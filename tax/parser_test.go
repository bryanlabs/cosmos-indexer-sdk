package tax

import (
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
)

const (
	del   = "cosmos1delegatoraddrxxxxxxxxxxxxxxxxxxxxxx"
	val   = "cosmosvaloper1validatorxxxxxxxxxxxxxxxxxxxx"
	other = "cosmos1otheraddrxxxxxxxxxxxxxxxxxxxxxxxxxx"
)

func ev(t string, attrs ...[2]string) indexerTxTypes.LogMessageEvent {
	e := indexerTxTypes.LogMessageEvent{Type: t}
	for _, a := range attrs {
		e.Attributes = append(e.Attributes, indexerTxTypes.Attribute{Key: a[0], Value: a[1]})
	}
	return e
}

func TestClassifyBankSend(t *testing.T) {
	msg := &banktypes.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 100))}
	out := classify(msg, &indexerTxTypes.LogMessage{})
	if len(out) != 1 || out[0].Category != string(CategoryTransfer) || out[0].Amount != "100" || out[0].Denom != "uatom" || out[0].FromAddr != del || out[0].ToAddr != other {
		t.Fatalf("bank send classification wrong: %+v", out)
	}
}

func TestClassifyDelegatorRewardFromEvents(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", del}, [2]string{"amount", "500uatom"}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Category != string(CategoryReward) || out[0].Amount != "500" || out[0].ToAddr != del {
		t.Fatalf("reward classification wrong: %+v", out)
	}
}

// A reward to a different address in the same log must NOT be attributed to del.
func TestClassifyRewardIgnoresOtherReceiver(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", other}, [2]string{"amount", "500uatom"}),
	}}
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("expected no events for non-matching receiver, got %+v", out)
	}
}

// REStake: MsgExec wrapping a delegator-reward withdraw, events tagged with
// authz_msg_index, must unwrap to one reward event.
func TestClassifyAuthzExecRestake(t *testing.T) {
	inner := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	exec := authz.NewMsgExec(sdk.AccAddress("grantee-bot-addr"), []sdk.Msg{inner})
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", del}, [2]string{"amount", "750uatom"}, [2]string{"authz_msg_index", "0"}),
	}}
	out := classify(&exec, log)
	if len(out) != 1 || out[0].Category != string(CategoryReward) || out[0].Amount != "750" || out[0].ToAddr != del {
		t.Fatalf("authz exec restake classification wrong: %+v", out)
	}
}
