package observeapp

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/bank"
)

// Embed the untouched keeper for read-only and parameter methods. All balance
// mutators delegate exactly once, with the original context and gas meters.
// Observations measure only the addresses explicitly touched by that bank call;
// phase reconciliation catches writes bypassing these calls, not residuals.
type observedBank struct {
	bank.Keeper
	app    *GaiaApp
	module string
}

func (k observedBank) watch(ctx sdk.Context, method string, addresses ...sdk.AccAddress) func() {
	o := &k.app.observer
	if !o.active || ctx.MultiStore().GetKVStore(k.app.keyAccount) != o.store {
		return func() {}
	}
	snapshot := func() balances {
		out := balances{}
		for _, a := range addresses {
			for _, c := range k.Keeper.GetCoins(quiet(ctx), a) {
				out[balanceKey{a.String(), c.Denom}] = c.Amount
			}
		}
		return out
	}
	before := snapshot()
	label := o.label
	if label == "" {
		label = o.phase
	}
	if k.module != "" && o.phase != "zero_height_export" {
		label = k.module + "/" + label
	}
	return func() { k.app.recordDelta(before, snapshot(), label+"/bank."+method) }
}
func (k observedBank) SetCoins(ctx sdk.Context, a sdk.AccAddress, c sdk.Coins) sdk.Error {
	defer k.watch(ctx, "set", a)()
	return k.Keeper.SetCoins(ctx, a, c)
}
func (k observedBank) AddCoins(ctx sdk.Context, a sdk.AccAddress, c sdk.Coins) (sdk.Coins, sdk.Tags, sdk.Error) {
	defer k.watch(ctx, "add", a)()
	return k.Keeper.AddCoins(ctx, a, c)
}
func (k observedBank) SubtractCoins(ctx sdk.Context, a sdk.AccAddress, c sdk.Coins) (sdk.Coins, sdk.Tags, sdk.Error) {
	defer k.watch(ctx, "subtract", a)()
	return k.Keeper.SubtractCoins(ctx, a, c)
}
func (k observedBank) SendCoins(ctx sdk.Context, a, b sdk.AccAddress, c sdk.Coins) (sdk.Tags, sdk.Error) {
	if k.module == "gov" {
		defer k.app.governanceSend(a, b, c)()
	}
	defer k.watch(ctx, "send", a, b)()
	return k.Keeper.SendCoins(ctx, a, b, c)
}
func (k observedBank) InputOutputCoins(ctx sdk.Context, in []bank.Input, out []bank.Output) (sdk.Tags, sdk.Error) {
	var addresses []sdk.AccAddress
	for _, v := range in {
		addresses = append(addresses, v.Address)
	}
	for _, v := range out {
		addresses = append(addresses, v.Address)
	}
	defer k.watch(ctx, "input_output", addresses...)()
	return k.Keeper.InputOutputCoins(ctx, in, out)
}
func (k observedBank) DelegateCoins(ctx sdk.Context, a sdk.AccAddress, c sdk.Coins) (sdk.Tags, sdk.Error) {
	defer k.watch(ctx, "delegate", a)()
	return k.Keeper.DelegateCoins(ctx, a, c)
}
func (k observedBank) UndelegateCoins(ctx sdk.Context, a sdk.AccAddress, c sdk.Coins) (sdk.Tags, sdk.Error) {
	defer k.watch(ctx, "undelegate", a)()
	return k.Keeper.UndelegateCoins(ctx, a, c)
}
