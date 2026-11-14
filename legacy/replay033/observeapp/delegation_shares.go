package observeapp

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
)

// Delegate/CreateValidator request native tokens but create decimal shares at
// the validator's actual rate. Neither number substitutes for the other.
func (app *GaiaApp) addedShares(ctx sdk.Context, msg sdk.Msg) func(bool) {
	var del sdk.AccAddress
	var val sdk.ValAddress
	var value sdk.Coin
	var label string
	switch m := msg.(type) {
	case staking.MsgDelegate:
		del, val, value, label = m.DelegatorAddress, m.ValidatorAddress, m.Value, "delegate"
	case staking.MsgCreateValidator:
		del, val, value, label = m.DelegatorAddress, m.ValidatorAddress, m.Value, "create_validator"
	default:
		return func(bool) {}
	}
	before := app.nativeStake(ctx, del, val)
	base := app.nativeRecord("principal_request", label, "native_bond_tokens")
	base.Phase = "message"
	base.Validator = val.String()
	base.Delegator = del.String()
	base.Address = del.String()
	base.Denom = value.Denom
	base.Amount = value.Amount.String()
	base.Before = app.nativeJSON(value)
	app.observer.pending = append(app.observer.pending, base)
	return func(success bool) {
		if !success {
			return
		}
		after := app.nativeStake(ctx, del, val)
		if after.Validator == nil || after.Delegation == nil || value.Denom != app.stakingKeeper.BondDenom(quiet(ctx)) || !value.Amount.IsPositive() {
			app.fail()
			return
		}
		beforeTokens, beforeShares := sdk.ZeroInt(), sdk.ZeroDec()
		if before.Validator != nil {
			beforeTokens, beforeShares = before.Validator.Tokens, before.Validator.DelegatorShares
		}
		change := nativeDelShares(after).Sub(nativeDelShares(before))
		if !change.IsPositive() || !after.Validator.Tokens.Sub(beforeTokens).Equal(value.Amount) || !after.Validator.DelegatorShares.Sub(beforeShares).Equal(change) {
			app.fail()
			return
		}
		r := base
		r.Kind = "staking_shares"
		r.Label = label + ":destination"
		r.Unit = "delegation_shares"
		r.Denom = ""
		r.Amount = change.String()
		r.Before, r.After = app.nativeJSON(before), app.nativeJSON(after)
		app.observer.pending = append(app.observer.pending, r)
	}
}
