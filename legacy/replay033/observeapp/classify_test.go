package observeapp

import (
	"bytes"
	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
	"testing"
)

func TestMovementClassificationCausalAndFailClosed(t *testing.T) {
	for label, want := range map[string]string{
		"delegator_reward/bank.add":                          "delegator_reward_payout",
		"distribution/delegation_reward/bank.add":            "delegator_reward_payout",
		"validator_commission/bank.add":                      "validator_commission_payout",
		"distribution/removed_validator_commission/bank.add": "validator_commission_payout",
		"staking/unbonding_complete/bank.undelegate":         "unbonding_principal_return",
		"staking/delegate/bank.delegate":                     "staking_principal_debit",
		"gov/deposit_refund/bank.send":                       "governance_deposit_refund",
		"gov/deposit_burn/bank.send":                         "governance_deposit_burn",
		"gov/gov/submit_proposal/bank.send":                  "governance_deposit",
		"bank/send/bank.send":                                "bank_transfer",
	} {
		got, ok := movementClass(label)
		must(t, ok && got == want, "known native cause classification")
	}
	for _, label := range []string{"", "begin_block/bank.add", "distribution/unknown/bank.add", "staking/unknown/bank.undelegate", "delegator_reward/bank.send"} {
		_, ok := movementClass(label)
		must(t, !ok, "unknown cause must not become payout")
	}
	must(t, !supportedMessage(nil), "unknown message type refused")
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	a := sdk.AccAddress(key.PubKey().Address()).String()
	app.recordDelta(balances{{a, "stake"}: sdk.NewInt(1)}, balances{{a, "stake"}: sdk.NewInt(2)}, "begin_block/bank.add")
	must(t, app.RecorderError() != nil, "unexplained residual is not income")
	app.Commit()
	must(t, out.Len() == 0 && app.LastBlockHeight() == 0, "unknown financial effect cannot commit or emit completion")
}
func TestNativeTransactionEvidenceIncludesSignedBytesAndFailure(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	raw := []byte("not a native signed transaction")
	response := app.DeliverTx(raw)
	must(t, response.Code != 0, "invalid native tx fails")
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	must(t, app.RecorderError() == nil, "failed tx is valid evidence")
	found := false
	for _, r := range rows(t, &out) {
		if r.Kind == "native_execution" && r.Phase == "transaction" {
			found = r.Code == response.Code && len(r.Before) > 0 && len(r.After) > 0
		}
	}
	_ = key
	must(t, found, "complete native failed response and input retained")
}
