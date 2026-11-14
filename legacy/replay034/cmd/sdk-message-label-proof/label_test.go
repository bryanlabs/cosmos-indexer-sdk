package main

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"testing"
)

// This module pins the actual recorder SDK v0.34.10. The observer emits exactly
// msg.Route()+"/"+msg.Type(); modern SDK module names are not evidence for it.
func TestSDK034TypedWithdrawAddressRoute(t *testing.T) {
	var msg sdk.Msg = distribution.MsgSetWithdrawAddress{}
	if got := msg.Route() + "/" + msg.Type(); got != "distr/set_withdraw_address" {
		t.Fatal("pinned SDK034 typed route/type differs from consumer literal")
	}
}
