package observeapp

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"testing"
)

func TestNativeDelegateUsesActualNonUnitShareRate(t *testing.T) {
	app, ctx, del, val, _ := nativeFixture(t)
	msg := staking.NewMsgDelegate(del, val, sdk.NewInt64Coin("stake", 2))
	finish := app.principal(ctx, msg)
	res := staking.NewHandler(app.stakingKeeper)(ctx, msg)
	nativeRequire(t, res.IsOK(), "actual native delegation succeeds")
	finish(true)
	nativeRequire(t, app.RecorderError() == nil, "native delegation evidence")
	found := false
	for _, r := range app.observer.pending {
		if r.Kind == "staking_shares" {
			nativeRequire(t, r.Unit == "delegation_shares" && r.Denom == "" && r.Amount == sdk.NewDec(6).String(), "shares not token input")
			found = true
		}
	}
	nativeRequire(t, found, "actual native added shares recorded")
}
func TestNativeDelegateUnknownSuccessFailsClosed(t *testing.T) {
	app, ctx, del, val, _ := nativeFixture(t)
	finish := app.principal(ctx, staking.NewMsgDelegate(del, val, sdk.NewInt64Coin("stake", 2)))
	finish(true)
	nativeRequire(t, app.RecorderError() != nil, "invented successful delegation refused")
}
