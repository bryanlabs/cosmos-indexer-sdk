package observeapp

import (
	"bytes"
	"testing"
	"time"

	upstream "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

func TestCausalStakingHashGasEquivalentAndActualPrincipal(t *testing.T) {
	app, _, key, raw := fixture(t, true)
	original := upstream.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0)
	original.InitChain(abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"})
	var out bytes.Buffer
	app.SetRecorder(&out)
	a := sdk.AccAddress(key.PubKey().Address())
	v := sdk.ValAddress(a)
	for h := int64(1); h <= 2; h++ {
		req := abci.RequestBeginBlock{Header: abci.Header{Height: h, ChainID: "observe-test", Time: time.Unix(1500000000+h, 0)}}
		app.BeginBlock(req)
		original.BeginBlock(req)
		var msg sdk.Msg = staking.NewMsgDelegate(a, v, coins(10)[0])
		if h == 2 {
			msg = staking.NewMsgUndelegate(a, v, coins(4)[0])
		}
		tx := signed(t, app, key, uint64(h-1), msg)
		x, y := app.DeliverTx(tx), original.DeliverTx(tx)
		must(t, x.Code == 0 && x.Code == y.Code && x.GasUsed == y.GasUsed && x.GasWanted == y.GasWanted, "staking code and original gas")
		app.EndBlock(abci.RequestEndBlock{Height: h})
		original.EndBlock(abci.RequestEndBlock{Height: h})
		must(t, app.RecorderError() == nil, "causal staking reconciliation")
		must(t, bytes.Equal(app.Commit().Data, original.Commit().Data), "causal staking original state hash")
	}
	found := false
	for _, r := range rows(t, &out) {
		if r.Kind == "principal" {
			must(t, r.Label == "undelegate" && r.Validator == v.String() && r.Delegator == a.String() && r.Amount == "4" && r.Status == "committed", "actual keeper principal identity")
			found = true
		}
	}
	must(t, found, "principal operation recorded")
}
func TestCausalAutoRewardAndMatureUnbonding(t *testing.T) {
	app, _, key, _ := fixture(t, true)
	begin(app, 1)
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	ctx := app.NewContext(false, abci.Header{Height: 1})
	a := sdk.AccAddress(key.PubKey().Address())
	v := sdk.ValAddress(a)
	dest := sdk.AccAddress(bytes.Repeat([]byte{8}, 20))
	app.distrKeeper.SetDelegatorWithdrawAddr(ctx, a, dest)
	current := app.distrKeeper.GetValidatorCurrentRewards(ctx, v)
	current.Rewards = sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(5))}
	app.distrKeeper.SetValidatorCurrentRewards(ctx, v, current)
	app.distrKeeper.SetValidatorOutstandingRewards(ctx, v, sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(5))})
	p := app.stakingKeeper.GetParams(ctx)
	p.UnbondingTime = 0
	app.stakingKeeper.SetParams(ctx, p)
	pool := app.stakingKeeper.GetPool(ctx)
	pool.NotBondedTokens = pool.NotBondedTokens.AddRaw(5)
	app.stakingKeeper.SetPool(ctx, pool)
	app.Commit()
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 2)
	for i, msg := range []sdk.Msg{staking.NewMsgDelegate(a, v, coins(10)[0]), staking.NewMsgUndelegate(a, v, coins(4)[0])} {
		r := app.DeliverTx(signed(t, app, key, uint64(i), msg))
		must(t, r.Code == 0, "actual staking operation succeeds")
	}
	app.EndBlock(abci.RequestEndBlock{Height: 2})
	app.Commit()
	must(t, app.RecorderError() == nil, "reward and completion reconcile")
	reward, complete := false, false
	for _, r := range rows(t, &out) {
		if r.Kind != "movement" {
			continue
		}
		switch r.Label {
		case "distribution/delegation_reward/bank.add":
			must(t, r.Validator == v.String() && r.Delegator == a.String() && r.Address == dest.String(), "auto reward keeper identities and destination")
			reward = true
		case "staking/unbonding_complete/bank.undelegate":
			must(t, r.Validator == v.String() && r.Delegator == a.String() && r.Address == a.String() && r.Amount == "4", "actual mature completion identity and amount")
			complete = true
		}
	}
	must(t, reward && complete, "actual auto reward and mature payout labels")
}
func TestGovernanceBurnActualDepositorScope(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	begin(app, 1)
	ctx := app.NewContext(false, abci.Header{Height: 1})
	a := sdk.AccAddress(key.PubKey().Address())
	// Use the actual keeper to create an inactive proposal and deposit.
	ctx = ctx.WithBlockHeader(abci.Header{Height: 1, Time: time.Unix(1500000001, 0)})
	proposal, err := app.govKeeper.SubmitProposal(ctx, gov.NewTextProposal("synthetic", "synthetic"))
	must(t, err == nil, "create synthetic proposal")
	err, _ = app.govKeeper.AddDeposit(ctx, proposal.ProposalID, a, coins(12))
	must(t, err == nil, "actual deposit")
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	var out bytes.Buffer
	app.SetRecorder(&out)
	req := abci.RequestBeginBlock{Header: abci.Header{Height: 2, ChainID: "observe-test", Time: proposal.DepositEndTime.Add(time.Second)}}
	app.BeginBlock(req)
	app.EndBlock(abci.RequestEndBlock{Height: 2})
	app.Commit()
	must(t, app.RecorderError() == nil, "actual governance burn reconciliation")
	count := 0
	for _, r := range rows(t, &out) {
		if r.Kind == "movement" && r.Label == "gov/deposit_burn/bank.send" {
			must(t, r.Delegator == a.String() && r.Status == "committed", "burn original depositor")
			count++
		}
	}
	must(t, count == 2, "escrow debit and burned credit retained")
}
