package tax

import (
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// Real mainnet message at height 31878991: STARS minted onto Cosmos Hub as a
// tokenfactory denom and credited to a Hub address.
func TestTFMintUnmarshalAndClassifyMainnetFixture(t *testing.T) {
	sender := "cosmos1s8qx0zvz8yd6e4x0mqmqf7fr9vvfn6226hkvrq"
	recipient := "cosmos1q327qxhcdwnvk72mqlrseacjp6sguepsu7vk46"
	denom := "factory/cosmos1s8qx0zvz8yd6e4x0mqmqf7fr9vvfn6226hkvrq/ustars"

	coin := append(fieldBytes(1, denom), fieldBytes(2, "3783243479843")...)
	b := append(fieldBytes(1, sender), nested(2, coin)...)
	b = append(b, fieldBytes(3, recipient)...)

	var m MsgTFMint
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Sender != sender || m.MintToAddress != recipient {
		t.Fatalf("addresses: %+v", m)
	}
	if m.Denom != denom || m.Amount != "3783243479843" {
		t.Fatalf("coin = %s / %s", m.Amount, m.Denom)
	}

	out := classify(&m, &indexerTxTypes.LogMessage{})
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	if out[0].ToAddr != recipient || out[0].Amount != "3783243479843" || out[0].Denom != denom {
		t.Fatalf("event = %+v", out[0])
	}
	if out[0].Category != string(CategoryTransfer) {
		t.Fatalf("category = %q", out[0].Category)
	}
}

// With no mintToAddress the coins go to the sender, so that is the recipient.
func TestTFMintWithoutMintToAddressCreditsSender(t *testing.T) {
	sender := "cosmos1s8qx0zvz8yd6e4x0mqmqf7fr9vvfn6226hkvrq"
	coin := append(fieldBytes(1, "factory/x/ufoo"), fieldBytes(2, "500")...)
	var m MsgTFMint
	if err := m.Unmarshal(append(fieldBytes(1, sender), nested(2, coin)...)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Recipient() != sender {
		t.Fatalf("recipient = %q, want the sender", m.Recipient())
	}
	out := classify(&m, &indexerTxTypes.LogMessage{})
	if len(out) != 1 || out[0].ToAddr != sender {
		t.Fatalf("event = %+v", out)
	}
}

// Burn, create-denom and change-admin move nothing to a third party.
func TestTokenFactoryNonValueMessagesProduceNoEvent(t *testing.T) {
	coin := append(fieldBytes(1, "factory/x/ufoo"), fieldBytes(2, "500")...)
	burn := &MsgTFBurn{}
	if err := burn.Unmarshal(append(fieldBytes(1, "cosmos1abc"), nested(2, coin)...)); err != nil {
		t.Fatalf("burn unmarshal: %v", err)
	}
	if burn.Amount != "500" {
		t.Fatalf("burn amount = %q", burn.Amount)
	}
	for _, m := range []sdkMsg{burn, &MsgTFCreateDenom{Sender: "cosmos1abc", Subdenom: "ufoo"}, &MsgTFChangeAdmin{Sender: "cosmos1abc"}} {
		if out := classify(m, &indexerTxTypes.LogMessage{}); len(out) != 0 {
			t.Fatalf("%T produced %d events", m, len(out))
		}
	}
}

func TestTokenFactoryTypesAreRegistrable(t *testing.T) {
	types := TokenFactoryMsgTypes()
	if len(types) != 4 {
		t.Fatalf("expected 4 tokenfactory types, got %d", len(types))
	}
	for url, msg := range types {
		u, ok := msg.(interface{ Unmarshal([]byte) error })
		if !ok {
			t.Fatalf("%s does not implement Unmarshal", url)
		}
		if err := u.Unmarshal(nil); err != nil {
			t.Fatalf("%s: empty unmarshal: %v", url, err)
		}
	}
}
