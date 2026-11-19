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
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
)

const typeURLUnjail = "/cosmos.slashing.v1beta1.MsgUnjail"

// Validator operators unjail through an authz grantee. MsgUnjail moves no
// coins, so the exec is a clean no-op unless the inner message's own events
// show a coin movement.
func unjailInner(t *testing.T) *codectypes.Any {
	t.Helper()
	operator := sdk.ValAddress(bytes.Repeat([]byte{9}, 20)).String()
	a := safetyUncached(t, &slashingtypes.MsgUnjail{ValidatorAddr: operator})
	if a.TypeUrl != typeURLUnjail {
		t.Fatalf("unexpected type url %s", a.TypeUrl)
	}
	return a
}

func TestAuthzUnjailHasNoTaxEffect(t *testing.T) {
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{unjailInner(t)}}
	// The Hub stores an authz unjail with only the exec's own message event:
	// the inner handler emits none and carries no authz_msg_index.
	exec := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", "/cosmos.authz.v1beta1.MsgExec"}, [2]string{"module", "authz"}, [2]string{"sender", other}),
	}}
	own := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", typeURLUnjail}, [2]string{attributeAuthzMsgIndex, "0"}),
	}}
	for name, log := range map[string]*txlog.LogMessage{"empty": {}, "exec only": exec, "own events": own} {
		data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
		if err != nil {
			t.Fatalf("%s log: %v", name, err)
		}
		if got := (*data).([]TaxableEvent); len(got) != 0 {
			t.Fatalf("%s log: delegated unjail produced %+v", name, got)
		}
	}
}

func TestAuthzUnjailWithCoinMovementStaysUnresolved(t *testing.T) {
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{unjailInner(t)}}
	for name, log := range map[string]*txlog.LogMessage{
		"indexed": {Events: []txlog.LogMessageEvent{
			ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "0"}),
		}},
		"untagged": {Events: []txlog.LogMessageEvent{
			ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}),
		}},
	} {
		if _, err := (&Parser{}).ParseMessage(msg, log, cfgForTests()); err == nil || !strings.Contains(err.Error(), "unsupported inner "+typeURLUnjail) {
			t.Fatalf("%s: unjail with a coin movement must stay unresolved: %v", name, err)
		}
	}
}

// An unjail beside an economic inner must not hide or shift the economic
// sibling's events.
func TestAuthzUnjailBesideRewardKeepsReward(t *testing.T) {
	reward := safetyUncached(t, &dist.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{unjailInner(t), reward}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", typeURLUnjail}, [2]string{attributeAuthzMsgIndex, "0"}),
		ev("withdraw_rewards", [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeValidator, val}, [2]string{attributeDelegator, del}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "1"}),
	}}
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	got := (*data).([]TaxableEvent)
	if len(got) != 1 || got[0].Category != string(CategoryReward) || got[0].Amount != "9" || got[0].ToAddr != del {
		t.Fatalf("reward beside an unjail changed: %+v", got)
	}
}

// An authz no-op must also be a top-level no-effect type, so both paths agree.
func TestAuthzUnjailIsTopLevelNoEffect(t *testing.T) {
	topLevel := false
	for _, url := range NoTaxEffectTypeURLs {
		if url == typeURLUnjail {
			topLevel = true
		}
	}
	if !topLevel || !authzNoTaxEffect[typeURLUnjail] || !authzBodyOnly(typeURLUnjail) {
		t.Fatalf("%s: top level %v, authz no-op %v", typeURLUnjail, topLevel, authzNoTaxEffect[typeURLUnjail])
	}
}
