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

// A Stargaze-style NFT marketplace sale emits wasm-finalize-sale; classify it as
// one nft_sale with the asset, price, seller and buyer. (Real event shape from
// cosmoshub-4 height 31761365.)
func TestClassifyNFTSale(t *testing.T) {
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("wasm-finalize-sale",
			[2]string{"collection", "cosmos1coll"},
			[2]string{"token_id", "468"},
			[2]string{"denom", "uatom"},
			[2]string{"price", "30000000"},
			[2]string{"seller_recipient", del},
			[2]string{"nft_recipient", other},
		),
	}}
	out := nftSaleEvents(log)
	if len(out) != 1 {
		t.Fatalf("want 1 nft sale, got %d: %+v", len(out), out)
	}
	e := out[0]
	if e.Category != string(CategoryNFTSale) || e.Amount != "30000000" || e.Denom != "uatom" ||
		e.FromAddr != del || e.ToAddr != other || e.Asset != "cosmos1coll/468" {
		t.Fatalf("nft sale classification wrong: %+v", e)
	}
}

// Both coin_received and transfer to the delegator (same movement) must count once.
func TestClassifyRewardNoDoubleCount(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", del}, [2]string{"amount", "7240259uatom"}),
		ev("transfer", [2]string{"recipient", del}, [2]string{"sender", val}, [2]string{"amount", "7240259uatom"}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Amount != "7240259" {
		t.Fatalf("double-count not prevented: %+v", out)
	}
}
