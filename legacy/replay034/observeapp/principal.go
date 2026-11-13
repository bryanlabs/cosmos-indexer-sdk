package observeapp

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
)

// The source validator's actual token debit is the unbonded principal, not
// the rounded-up request in the signed message. Reads use independent gas.
func (app *GaiaApp) principal(ctx sdk.Context, msg sdk.Msg) func(bool) {
	var val sdk.ValAddress
	var del sdk.AccAddress
	var label string
	switch m := msg.(type) {
	case staking.MsgUndelegate:
		val, del, label = m.ValidatorAddress, m.DelegatorAddress, "undelegate"
	case staking.MsgBeginRedelegate:
		val, del, label = m.ValidatorSrcAddress, m.DelegatorAddress, "redelegate"
	default:
		return func(bool) {}
	}
	tokens := func() sdk.Int {
		v, ok := app.stakingKeeper.GetValidator(quiet(ctx), val)
		if !ok {
			return sdk.ZeroInt()
		}
		return v.Tokens
	}
	before := tokens()
	return func(success bool) {
		if !success {
			return
		}
		n := before.Sub(tokens())
		if n.IsNegative() {
			app.fail()
			return
		}
		o := &app.observer
		o.pending = append(o.pending, Record{Kind: "principal", Height: o.height, Tx: o.tx, Message: o.message, Phase: "message", Label: label, Validator: val.String(), Delegator: del.String(), Address: del.String(), Denom: app.stakingKeeper.BondDenom(quiet(ctx)), Amount: n.String()})
	}
}
