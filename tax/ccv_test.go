package tax

import (
	"bytes"
	"testing"

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
	for _, url := range urls {
		if !noEffect[url] {
			t.Fatalf("%s decodes but has no tax decision, so it would hold reports back", url)
		}
	}
}
