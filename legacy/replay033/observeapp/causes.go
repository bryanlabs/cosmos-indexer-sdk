package observeapp

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"github.com/cosmos/cosmos-sdk/x/staking/tags"
	abci "github.com/tendermint/tendermint/abci/types"
)

// causal scopes wrap the actual pinned keeper call, never an inferred residual.
func (app *GaiaApp) cause(label, validator, delegator string) func() {
	o := &app.observer
	l, v, d := o.label, o.validator, o.delegator
	o.label, o.validator, o.delegator = label, validator, delegator
	return func() { o.label, o.validator, o.delegator = l, v, d }
}

type distributionHooks struct {
	sdk.StakingHooks
	app *GaiaApp
}

func (h distributionHooks) BeforeDelegationSharesModified(ctx sdk.Context, d sdk.AccAddress, v sdk.ValAddress) {
	defer h.app.cause("delegation_reward", v.String(), d.String())()
	h.StakingHooks.BeforeDelegationSharesModified(ctx, d, v)
}
func (h distributionHooks) AfterValidatorRemoved(ctx sdk.Context, c sdk.ConsAddress, v sdk.ValAddress) {
	defer h.app.cause("removed_validator_commission", v.String(), sdk.AccAddress(v).String())()
	h.StakingHooks.AfterValidatorRemoved(ctx, c, v)
}
func (app *GaiaApp) messageCause(msg sdk.Msg) func() {
	switch m := msg.(type) {
	case distribution.MsgWithdrawDelegatorReward:
		return app.cause("delegation_reward", m.ValidatorAddress.String(), m.DelegatorAddress.String())
	case distribution.MsgWithdrawValidatorCommission:
		return app.cause("validator_commission", m.ValidatorAddress.String(), sdk.AccAddress(m.ValidatorAddress).String())
	case staking.MsgDelegate:
		return app.cause("delegate", m.ValidatorAddress.String(), m.DelegatorAddress.String())
	case staking.MsgCreateValidator:
		return app.cause("create_validator", m.ValidatorAddress.String(), m.DelegatorAddress.String())
	case staking.MsgUndelegate:
		return app.cause("undelegate", m.ValidatorAddress.String(), m.DelegatorAddress.String())
	case staking.MsgBeginRedelegate:
		return app.cause("redelegate", m.ValidatorSrcAddress.String(), m.DelegatorAddress.String())
	}
	return func() {}
}

// stakingEndBlock is v0.33.2 x/staking/handler.go EndBlocker, with a
// causal scope around CompleteUnbonding only. Calls, ordering, tags and gas
// are unchanged. The copied upstream license is observeapp/LICENSE.
func (app *GaiaApp) stakingEndBlock(ctx sdk.Context) ([]abci.ValidatorUpdate, sdk.Tags) {
	k := app.stakingKeeper
	if !app.observer.active {
		return staking.EndBlocker(ctx, k)
	}
	resTags := sdk.NewTags()
	updates := k.ApplyAndReturnValidatorSetUpdates(ctx)
	k.UnbondAllMatureValidatorQueue(ctx)
	mature := k.DequeueAllMatureUBDQueue(ctx, ctx.BlockHeader().Time)
	for _, pair := range mature {
		restore := app.cause("unbonding_complete", pair.ValidatorAddress.String(), pair.DelegatorAddress.String())
		err := k.CompleteUnbonding(ctx, pair.DelegatorAddress, pair.ValidatorAddress)
		app.keeperResult("end_block", "native_complete_unbonding", pair, err)
		restore()
		if err != nil {
			continue
		}
		resTags.AppendTags(sdk.NewTags(tags.Action, staking.ActionCompleteUnbonding, tags.Delegator, pair.DelegatorAddress.String(), tags.SrcValidator, pair.ValidatorAddress.String()))
	}
	reds := k.DequeueAllMatureRedelegationQueue(ctx, ctx.BlockHeader().Time)
	for _, r := range reds {
		restore := app.cause("redelegation_complete", r.ValidatorSrcAddress.String(), r.DelegatorAddress.String())
		err := k.CompleteRedelegation(ctx, r.DelegatorAddress, r.ValidatorSrcAddress, r.ValidatorDstAddress)
		app.keeperResult("end_block", "native_complete_redelegation", r, err)
		restore()
		if err != nil {
			continue
		}
		resTags.AppendTags(sdk.NewTags(tags.Action, tags.ActionCompleteRedelegation, tags.Delegator, r.DelegatorAddress.String(), tags.SrcValidator, r.ValidatorSrcAddress.String(), tags.DstValidator, r.ValidatorDstAddress.String()))
	}
	return updates, resTags
}
