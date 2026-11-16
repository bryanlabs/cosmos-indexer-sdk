package observeapp

import (
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
)

// Native execution evidence is separate from financial classification. It retains
// every native response field and signed input byte without a schema projection.
func (app *GaiaApp) execution(phase, label string, before, after interface{}, code uint32) {
	o := &app.observer
	if !o.active {
		return
	}
	b, e1 := json.Marshal(before)
	a, e2 := json.Marshal(after)
	if e1 != nil || e2 != nil {
		app.fail()
		return
	}
	o.pending = append(o.pending, Record{Kind: "native_execution", Validator: o.validator, Delegator: o.delegator, Height: o.height, Tx: o.tx, Message: o.message, Phase: phase, Label: label, Before: b, After: a, Code: code, Unit: "native_execution_evidence"})
}
func (app *GaiaApp) keeperResult(phase, label string, input interface{}, err sdk.Error) {
	result := sdk.Result{}
	if err != nil {
		result = err.Result()
	}
	app.execution(phase, label, input, result, uint32(result.Code))
}
func (app *GaiaApp) withdrawalResult(err sdk.Error) {
	app.keeperResult("zero_height_export", "native_withdrawal_result", nil, err)
}
func supportedMessage(msg sdk.Msg) bool {
	switch msg.(type) {
	case bank.MsgSend, bank.MsgMultiSend, staking.MsgCreateValidator, staking.MsgEditValidator, staking.MsgDelegate, staking.MsgUndelegate, staking.MsgBeginRedelegate, distribution.MsgSetWithdrawAddress, distribution.MsgWithdrawDelegatorReward, distribution.MsgWithdrawValidatorCommission, gov.MsgSubmitProposal, gov.MsgDeposit, gov.MsgVote, slashing.MsgUnjail:
		return true
	}
	return false
}
