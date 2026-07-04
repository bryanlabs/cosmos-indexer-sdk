package tax

import (
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
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

// A reward redirected to a different address (MsgSetWithdrawAddress) must
// still attribute to the delegator, not vanish (INF-213): the delegator has
// dominion and control over the income regardless of where they told it to
// land, so this must not depend on any exact-address match against the event.
func TestClassifyRewardRedirectedToWithdrawAddressStillAttributesToDelegator(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", other}, [2]string{"amount", "500uatom"}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Category != string(CategoryReward) || out[0].Amount != "500" || out[0].ToAddr != del {
		t.Fatalf("redirected reward should attribute to the delegator, got %+v", out)
	}
}

// Real mainnet fixture: cosmoshub-4 height 31821176, tx
// 020F25D22B68506A3953943D441DFDDE2D46A967AE7286C61E3B3BE8977BBB3A. Delegator
// cosmos140kq2fts8ed9m73a6dch7sgdap6hnp2pqqasx9 had set their withdraw address
// to cosmos1tdlzn4kreyrjqg9etg2fvqvxt9vpwhtzp3af80 (confirmed via a separate
// MsgSetWithdrawAddress from the same delegator); withdrawing rewards paid
// 627uatom to that withdraw address, not the delegator. Message events are
// already scoped per msg_index by the chain's own ABCI logs before this
// message's LogMessage is built (core/tx.go), so this is exactly the shape
// classify() sees for message 0 of that tx (the paired MsgWithdrawValidatorCommission
// at msg_index 1, paying 18231286uatom to the same withdraw address, is a
// separate LogMessage classify() never sees here).
func TestClassifyRewardRedirectedToWithdrawAddressMainnetFixture(t *testing.T) {
	delegator := "cosmos140kq2fts8ed9m73a6dch7sgdap6hnp2pqqasx9"
	withdrawAddr := "cosmos1tdlzn4kreyrjqg9etg2fvqvxt9vpwhtzp3af80"
	msg := &disttypes.MsgWithdrawDelegatorReward{
		DelegatorAddress: delegator,
		ValidatorAddress: "cosmosvaloper140kq2fts8ed9m73a6dch7sgdap6hnp2p95f92k",
	}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", withdrawAddr}, [2]string{"amount", "627uatom"}),
		ev("transfer", [2]string{"recipient", withdrawAddr}, [2]string{"sender", "cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl"}, [2]string{"amount", "627uatom"}),
		ev("withdraw_rewards", [2]string{"amount", "627uatom"}, [2]string{"validator", "cosmosvaloper140kq2fts8ed9m73a6dch7sgdap6hnp2p95f92k"}, [2]string{"delegator", delegator}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Category != string(CategoryReward) || out[0].Amount != "627" || out[0].ToAddr != delegator {
		t.Fatalf("mainnet redirected-reward fixture misclassified: %+v", out)
	}
}

// The same auto-withdraw-on-delegate path uses the same withdraw-address
// routing as an explicit MsgWithdrawDelegatorReward, so a redelegate that
// triggers an auto-withdrawn reward to a redirected address must attribute the
// same way.
func TestClassifyRedelegateRewardRedirectedToWithdrawAddressStillAttributesToDelegator(t *testing.T) {
	msg := &stakingtypes.MsgBeginRedelegate{DelegatorAddress: del, ValidatorSrcAddress: val}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", other}, [2]string{"amount", "250uatom"}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Category != string(CategoryReward) || out[0].Amount != "250" || out[0].ToAddr != del {
		t.Fatalf("redirected redelegate reward should attribute to the delegator, got %+v", out)
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

// DEX swap (wasm action=swap) → two legs: dispose offer, acquire return.
func TestClassifySwap(t *testing.T) {
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("wasm",
			[2]string{"action", "swap"},
			[2]string{"receiver", del},
			[2]string{"offer_asset", "uatom"}, [2]string{"offer_amount", "330000"},
			[2]string{"ask_asset", "factory/x/art"}, [2]string{"return_amount", "15362"},
		),
	}}
	out := swapEvents(log)
	if len(out) != 2 {
		t.Fatalf("want 2 swap legs, got %d: %+v", len(out), out)
	}
	if out[0].FromAddr != del || out[0].Denom != "uatom" || out[0].Amount != "330000" {
		t.Fatalf("offer leg wrong: %+v", out[0])
	}
	if out[1].ToAddr != del || out[1].Denom != "factory/x/art" || out[1].Amount != "15362" {
		t.Fatalf("return leg wrong: %+v", out[1])
	}
}

// NFT mint (wasm action=mint) → acquisition for owner with the spent cost.
func TestClassifyNFTMint(t *testing.T) {
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_spent", [2]string{"spender", del}, [2]string{"amount", "5000000uatom"}),
		ev("wasm",
			[2]string{"_contract_address", "coll1"},
			[2]string{"action", "mint"},
			[2]string{"owner", del},
			[2]string{"token_id", "1047"},
		),
	}}
	out := nftMintEvents(log)
	if len(out) != 1 || out[0].Category != string(CategoryNFTMint) || out[0].ToAddr != del ||
		out[0].Asset != "coll1/1047" || out[0].Amount != "5000000" || out[0].Denom != "uatom" {
		t.Fatalf("nft mint classification wrong: %+v", out)
	}
}
