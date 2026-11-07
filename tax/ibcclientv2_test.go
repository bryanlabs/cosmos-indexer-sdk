package tax

import "testing"

func TestClientV2MessagesDecodeSigner(t *testing.T) {
	b := append(fieldBytes(1, "08-wasm-1369"), nested(2, []byte{0x01})...)
	b = append(b, fieldBytes(3, fixtureHubClient)...)
	b = append(b, fieldBytes(4, fixtureIBCReceiver)...)
	var counterparty MsgRegisterCounterpartyV2
	if err := counterparty.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if counterparty.ClientID != "08-wasm-1369" || counterparty.CounterpartyClientID != fixtureHubClient || len(counterparty.GetSigners()) != 1 {
		t.Fatalf("decoded %+v", counterparty)
	}

	// MsgUpdateClientConfig puts the signer on field 3, after the config message.
	b = append(fieldBytes(1, "08-wasm-1369"), nested(2, fieldBytes(1, fixtureIBCReceiver))...)
	b = append(b, fieldBytes(3, fixtureIBCReceiver)...)
	var config MsgUpdateClientConfigV2
	if err := config.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if config.Signer != fixtureIBCReceiver || len(config.GetSigners()) != 1 {
		t.Fatalf("decoded %+v", config)
	}
}
