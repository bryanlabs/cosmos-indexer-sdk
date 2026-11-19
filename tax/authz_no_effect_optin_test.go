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
)

// Validator operators opt in to consumer chains through an authz grantee.
// MsgOptIn moves no coins, so the exec is a clean no-op unless the inner
// message's own events show a coin movement.
func optInInner(t *testing.T) *codectypes.Any {
	t.Helper()
	operator := sdk.ValAddress(bytes.Repeat([]byte{9}, 20)).String()
	var b []byte
	for _, f := range []struct {
		num byte
		v   string
	}{{2, operator}, {5, "21"}} {
		b = append(b, f.num<<3|2, byte(len(f.v)))
		b = append(b, f.v...)
	}
	a := &codectypes.Any{TypeUrl: TypeURLCCVOptIn, Value: b}
	m, err := unpackAuthzInner(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(*MsgOptIn); got.ProviderAddr != operator || got.ConsumerID != "21" {
		t.Fatalf("decoded opt-in %+v", got)
	}
	return a
}

func TestAuthzOptInHasNoTaxEffect(t *testing.T) {
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{optInInner(t)}}
	exec := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", "/cosmos.authz.v1beta1.MsgExec"}, [2]string{"module", "authz"}, [2]string{"sender", other}),
	}}
	own := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", TypeURLCCVOptIn}, [2]string{attributeAuthzMsgIndex, "0"}),
	}}
	for name, log := range map[string]*txlog.LogMessage{"empty": {}, "exec only": exec, "own events": own} {
		data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
		if err != nil {
			t.Fatalf("%s log: %v", name, err)
		}
		if got := (*data).([]TaxableEvent); len(got) != 0 {
			t.Fatalf("%s log: delegated opt-in produced %+v", name, got)
		}
	}
}

func TestAuthzOptInWithCoinMovementStaysUnresolved(t *testing.T) {
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{optInInner(t)}}
	for name, log := range map[string]*txlog.LogMessage{
		"indexed": {Events: []txlog.LogMessageEvent{
			ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "0"}),
		}},
		"untagged": {Events: []txlog.LogMessageEvent{
			ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}),
		}},
	} {
		if _, err := (&Parser{}).ParseMessage(msg, log, cfgForTests()); err == nil || !strings.Contains(err.Error(), "unsupported inner "+TypeURLCCVOptIn) {
			t.Fatalf("%s: opt-in with a coin movement must stay unresolved: %v", name, err)
		}
	}
}

// An opt-in beside an economic inner must not hide or shift the economic
// sibling's events.
func TestAuthzOptInBesideRewardKeepsReward(t *testing.T) {
	reward := safetyUncached(t, &dist.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{optInInner(t), reward}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", TypeURLCCVOptIn}, [2]string{attributeAuthzMsgIndex, "0"}),
		ev("withdraw_rewards", [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeValidator, val}, [2]string{attributeDelegator, del}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "9" + fixtureNativeDenom}, [2]string{attributeAuthzMsgIndex, "1"}),
	}}
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	got := (*data).([]TaxableEvent)
	if len(got) != 1 || got[0].Category != string(CategoryReward) || got[0].Amount != "9" || got[0].ToAddr != del {
		t.Fatalf("reward beside an opt-in changed: %+v", got)
	}
}

// An authz no-op must also be a top-level no-effect type, so both paths agree.
func TestAuthzOptInIsTopLevelNoEffect(t *testing.T) {
	topLevel := false
	for _, url := range NoTaxEffectTypeURLs {
		if url == TypeURLCCVOptIn {
			topLevel = true
		}
	}
	if !topLevel || !authzNoTaxEffect[TypeURLCCVOptIn] || !authzBodyOnly(TypeURLCCVOptIn) {
		t.Fatalf("%s: top level %v, authz no-op %v", TypeURLCCVOptIn, topLevel, authzNoTaxEffect[TypeURLCCVOptIn])
	}
}
