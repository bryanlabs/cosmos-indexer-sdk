package tax

import (
	"bytes"
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	dist "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// Validator operators run MsgEditValidator and the provider's MsgOptIn and
// MsgOptOut through an authz grantee. None of them moves coins, so the exec is
// a clean no-op unless the inner message's own events show a coin movement.
func validatorOpsInners(t *testing.T) map[string]*codectypes.Any {
	t.Helper()
	operator := sdk.ValAddress(bytes.Repeat([]byte{9}, 20)).String()
	signer := sdk.AccAddress(bytes.Repeat([]byte{9}, 20)).String()
	optIn := append(fieldBytes(5, "7"), fieldBytes(2, operator)...)
	optIn = append(optIn, fieldBytes(4, signer)...)
	optOut := append(fieldBytes(4, "7"), fieldBytes(2, operator)...)
	optOut = append(optOut, fieldBytes(3, signer)...)
	return map[string]*codectypes.Any{
		"/cosmos.staking.v1beta1.MsgEditValidator": safetyUncached(t, &stakingtypes.MsgEditValidator{
			Description:      stakingtypes.Description{Moniker: "v", Details: "edited"},
			ValidatorAddress: operator,
		}),
		TypeURLCCVOptIn:  {TypeUrl: TypeURLCCVOptIn, Value: optIn},
		TypeURLCCVOptOut: {TypeUrl: TypeURLCCVOptOut, Value: optOut},
	}
}

func TestAuthzValidatorOpsHaveNoTaxEffect(t *testing.T) {
	for url, inner := range validatorOpsInners(t) {
		t.Run(url, func(t *testing.T) {
			msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{inner}}
			own := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev("message", [2]string{"action", url}, [2]string{attributeAuthzMsgIndex, "0"}),
			}}
			for name, log := range map[string]*txlog.LogMessage{"empty": {}, "own events": own} {
				data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
				if err != nil {
					t.Fatalf("%s log: %v", name, err)
				}
				if got := (*data).([]TaxableEvent); len(got) != 0 {
					t.Fatalf("%s log: delegated %s produced %+v", name, url, got)
				}
			}
		})
	}
}

func TestAuthzValidatorOpsWithCoinMovementStayUnresolved(t *testing.T) {
	for url, inner := range validatorOpsInners(t) {
		t.Run(url, func(t *testing.T) {
			msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{inner}}
			moved := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "0"}),
			}}
			if _, err := (&Parser{}).ParseMessage(msg, moved, cfgForTests()); err == nil || !strings.Contains(err.Error(), "unsupported inner "+url) {
				t.Fatalf("%s with a coin movement must stay unresolved: %v", url, err)
			}
		})
	}
}

// A no-op inner beside an economic one must not hide or shift the economic
// sibling's events.
func TestAuthzValidatorOpsBesideRewardKeepReward(t *testing.T) {
	inners := validatorOpsInners(t)
	reward := safetyUncached(t, &dist.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{inners[TypeURLCCVOptIn], reward, inners["/cosmos.staking.v1beta1.MsgEditValidator"]}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", TypeURLCCVOptIn}, [2]string{attributeAuthzMsgIndex, "0"}),
		ev("withdraw_rewards", [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeValidator, val}, [2]string{attributeDelegator, del}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev("message", [2]string{"action", "/cosmos.staking.v1beta1.MsgEditValidator"}, [2]string{attributeAuthzMsgIndex, "2"}),
	}}
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	got := (*data).([]TaxableEvent)
	if len(got) != 1 || got[0].Category != string(CategoryReward) || got[0].Amount != "9" || got[0].ToAddr != del {
		t.Fatalf("reward beside no-op inners changed: %+v", got)
	}
}

// Provider messages that are no-op inners must also be no-effect at the top
// level, so both paths agree.
func TestAuthzNoEffectProviderTypesAreTopLevelNoEffect(t *testing.T) {
	topLevel := map[string]bool{}
	for _, url := range NoTaxEffectTypeURLs {
		topLevel[url] = true
	}
	for url := range authzNoTaxEffect {
		if _, provider := ProviderMsgTypes()[url]; provider && !topLevel[url] {
			t.Fatalf("%s is an authz no-op but not a top-level no-effect type", url)
		}
	}
	for _, url := range []string{TypeURLCCVOptIn, TypeURLCCVOptOut, "/cosmos.staking.v1beta1.MsgEditValidator"} {
		if !authzNoTaxEffect[url] || !authzBodyOnly(url) {
			t.Fatalf("%s is not an authz no-op", url)
		}
	}
}
