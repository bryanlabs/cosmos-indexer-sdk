package tax

import (
	"bytes"
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestProviderMessagesDecodeSigner(t *testing.T) {
	signer := sdk.AccAddress(bytes.Repeat([]byte{7}, 20))
	operator := sdk.ValAddress(bytes.Repeat([]byte{9}, 20)).String()
	cases := []struct {
		url            string
		signer, other  int
		otherIsAddress bool
	}{
		{TypeURLCCVAssignConsumerKey, 4, 2, true},
		{TypeURLCCVOptIn, 4, 2, true},
		{TypeURLCCVOptOut, 3, 2, true},
		{TypeURLCCVSetConsumerCommissionRate, 4, 1, true},
		{TypeURLCCVCreateConsumer, 1, 2, false},
		{TypeURLCCVUpdateConsumer, 1, 2, false},
		{TypeURLCCVRemoveConsumer, 2, 1, false},
		{TypeURLCCVSubmitConsumerMisbehaviour, 1, 3, false},
		{TypeURLCCVSubmitConsumerDoubleVoting, 1, 4, false},
		{TypeURLCCVRegisterConsumerRewardDenom, 2, 1, false},
	}
	types := ProviderMsgTypes()
	if len(types) != len(cases) {
		t.Fatalf("registered %d provider types, tested %d", len(types), len(cases))
	}
	for _, c := range cases {
		other := "0"
		if c.otherIsAddress {
			other = operator
		}
		// The other field comes first so a signer field mixed up with it would win.
		b := append(fieldBytes(c.other, other), fieldBytes(c.signer, signer.String())...)
		b = append(b, nested(99, []byte{1})...)
		msg := types[c.url]
		if err := msg.(interface{ Unmarshal([]byte) error }).Unmarshal(b); err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		got := msg.GetSigners()
		if len(got) != 1 || !got[0].Equals(signer) {
			t.Fatalf("%s: signers = %v, want %s", c.url, got, signer)
		}
		if raw, _ := msg.(interface{ Marshal() ([]byte, error) }).Marshal(); !bytes.Equal(raw, b) {
			t.Fatalf("%s: raw bytes not preserved", c.url)
		}
	}
}

// Before ICS v4 MsgAssignConsumerKey had no signer field; the operator's own
// account signed it, so the provider address stands in for the signer.
func TestLegacyAssignConsumerKeySignedByOperator(t *testing.T) {
	account := bytes.Repeat([]byte{9}, 20)
	b := append(fieldBytes(1, "neutron-1"), fieldBytes(2, sdk.ValAddress(account).String())...)
	b = append(b, fieldBytes(3, `{"@type":"/cosmos.crypto.ed25519.PubKey"}`)...)
	var m MsgAssignConsumerKey
	if err := m.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	got := m.GetSigners()
	if len(got) != 1 || !bytes.Equal(got[0], account) {
		t.Fatalf("signers = %v", got)
	}
}

func TestNewlyDecodedNoEffectTypesAreReviewed(t *testing.T) {
	urls := []string{
		"/ibc.applications.interchain_accounts.controller.v1.MsgRegisterInterchainAccount",
		"/ibc.applications.interchain_accounts.controller.v1.MsgSendTx",
	}
	for url := range ProviderMsgTypes() {
		urls = append(urls, url)
	}
	for url := range IBCClientV2MsgTypes() {
		urls = append(urls, url)
	}
	noEffect := map[string]bool{}
	for _, url := range NoTaxEffectTypeURLs {
		noEffect[url] = true
	}
	for _, url := range MessageTypeURLs {
		noEffect[url] = true
	}
	for _, url := range urls {
		if !noEffect[url] {
			t.Fatalf("%s decodes but has no tax decision, so it would hold reports back", url)
		}
	}
}

// Mainnet registration at height 15598226: the depositor paid 10 ATOM to the
// community pool for the consumer reward denom.
func TestRegisterConsumerRewardDenomFeeIsExpense(t *testing.T) {
	const rcrdFee = "10000000uatom"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_spent", [2]string{attributeSpender, del}, [2]string{attributeAmount, rcrdFee}),
		ev(eventCoinReceived, [2]string{attributeReceiver, distributionModuleAddress}, [2]string{attributeAmount, rcrdFee}),
		ev("transfer", [2]string{attributeRecipient, distributionModuleAddress}, [2]string{attributeSender, del}, [2]string{attributeAmount, rcrdFee}),
		ev("message", [2]string{attributeSender, del}),
		ev("register_consumer_reward_denom", [2]string{attributeDenom, "ibc/3C3D7B3BE4ECC85A0E5B52A3AEC3B7DFC2AA9CA47C37821E57020D6807043BE9"}, [2]string{"depositor", del}),
	}}
	msg := &MsgRegisterConsumerRewardDenom{Denom: "ibc/3C3D7B3BE4ECC85A0E5B52A3AEC3B7DFC2AA9CA47C37821E57020D6807043BE9", Depositor: del}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Category != string(CategoryExpense) || out[0].Amount != "10000000" || out[0].Denom != fixtureNativeDenom ||
		out[0].FromAddr != del || out[0].ToAddr != distributionModuleAddress || out[0].RewardTrigger != TriggerConsumerRewardDenomFee {
		t.Fatalf("events = %+v", out)
	}
}
