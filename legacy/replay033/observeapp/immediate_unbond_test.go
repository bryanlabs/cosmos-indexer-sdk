package observeapp

import (
	"bytes"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"testing"
)

func TestRecordedNativeUnbondedUndelegateImmediatePrincipalReturn(t *testing.T) {
	app, ctx, del, val, _ := nativeFixture(t)
	validator, found := app.stakingKeeper.GetValidator(quiet(ctx), val)
	nativeRequire(t, found, "native fixture validator exists")
	pool := app.stakingKeeper.GetPool(quiet(ctx))
	pool.BondedTokens = pool.BondedTokens.Sub(validator.Tokens)
	pool.NotBondedTokens = pool.NotBondedTokens.Add(validator.Tokens)
	app.stakingKeeper.SetPool(quiet(ctx), pool)
	validator.Status = sdk.Unbonded
	app.stakingKeeper.SetValidator(quiet(ctx), validator)
	var output bytes.Buffer
	app.SetRecorder(&output)
	app.observer.active = true
	app.observer.tx = 0
	app.observer.message = -1
	msg := staking.NewMsgUndelegate(del, val, validator.DelegatorShares)
	response := app.observeHandler(staking.NewHandler(app.stakingKeeper))(ctx, msg)
	nativeRequire(t, response.IsOK() && app.RecorderError() == nil, "actual recorded immediate native unbond must succeed")
	payout, principal, shares := false, false, false
	for _, r := range app.observer.pending {
		if r.Kind == "movement" && r.Label == "staking/undelegate/bank.undelegate" {
			nativeRequire(t, r.Classification == "immediate_unbond_principal_return" && r.Amount == validator.Tokens.String() && r.Address == del.String() && r.Validator == val.String(), "native immediate principal must not be reward income")
			payout = true
		}
		principal = principal || r.Kind == "principal"
		shares = shares || r.Kind == "staking_shares"
	}
	nativeRequire(t, payout && principal && shares, "actual payout, principal and separate share evidence required")
}
