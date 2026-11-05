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

// Without events, burn and create-denom have no movement to record, and
// change-admin never moves anything.
func TestTokenFactoryNonValueMessagesProduceNoEvent(t *testing.T) {
	coin := append(fieldBytes(1, "factory/x/ufoo"), fieldBytes(2, "500")...)
	burn := &MsgTFBurn{}
	if err := burn.Unmarshal(append(fieldBytes(1, fixtureCosmosAddress), nested(2, coin)...)); err != nil {
		t.Fatalf("burn unmarshal: %v", err)
	}
	if burn.Amount != "500" {
		t.Fatalf("burn amount = %q", burn.Amount)
	}
	for _, m := range []sdkMsg{burn, &MsgTFCreateDenom{Sender: fixtureCosmosAddress, Subdenom: "ufoo"}, &MsgTFChangeAdmin{Sender: fixtureCosmosAddress}} {
		if out := classify(m, &indexerTxTypes.LogMessage{}); len(out) != 0 {
			t.Fatalf("%T produced %d events", m, len(out))
		}
	}
}

func TestTokenFactoryTypesAreRegistrable(t *testing.T) {
	types := TokenFactoryMsgTypes()
	if len(types) != 6 {
		t.Fatalf("expected 6 tokenfactory types, got %d", len(types))
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

// SetDenomMetadata is display information only, so it must decode but never
// produce a taxable event.
func TestSetDenomMetadataDecodesWithoutValue(t *testing.T) {
	sender := "cosmos1z3dvpke5pq3752pum6x4zd2fyfjz69p0rusqzt"
	meta := append(fieldBytes(1, "Pepe is fren"), fieldBytes(3, "factory/x/pepe")...)
	var m MsgTFSetDenomMetadata
	if err := m.Unmarshal(append(fieldBytes(1, sender), nested(2, meta)...)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Sender != sender || len(m.MetadataRaw) == 0 {
		t.Fatalf("decoded = %+v", m)
	}
	if out := classify(&m, &indexerTxTypes.LogMessage{}); len(out) != 0 {
		t.Fatalf("metadata produced %d taxable events", len(out))
	}
}

// ForceTransfer has never appeared on Cosmos Hub, but if it ever does it moves
// real coins between two parties and must be recorded for both.
func TestForceTransferIsClassifiedAsAMovement(t *testing.T) {
	from := "cosmos1dycvuk8xxqr9mt4vatev3ygdmt7csazum3zu4z"
	to := "cosmos1z8hye2cqvxusvvpcwk5l6uxw3zxv73pew5rdgs"
	coin := append(fieldBytes(1, "factory/x/pepe"), fieldBytes(2, "4200")...)
	b := append(fieldBytes(1, "cosmos1admin"), nested(2, coin)...)
	b = append(b, fieldBytes(3, from)...)
	b = append(b, fieldBytes(4, to)...)

	var m MsgTFForceTransfer
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Amount != "4200" || m.Denom != "factory/x/pepe" {
		t.Fatalf("coin = %s / %s", m.Amount, m.Denom)
	}
	out := classify(&m, &indexerTxTypes.LogMessage{})
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	if out[0].FromAddr != from || out[0].ToAddr != to || out[0].Amount != "4200" {
		t.Fatalf("event = %+v", out[0])
	}
}

// Real mainnet burn at height 30016526: the holder's coins go to the module
// before they are burned, and only that transfer is the holder's disposal.
func TestTFBurnRecordsTheTransferToTheModule(t *testing.T) {
	holder := "cosmos1warp46ynf3f0z24pn4lqkpeumlcj5863f374zg"
	module := "cosmos19ejy8n9qsectrf4semdp9cpknflld0j6aqaddp"
	denom := "factory/cosmos1warp46ynf3f0z24pn4lqkpeumlcj5863f374zg/cosmowarp"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		{Type: "transfer", Attributes: []indexerTxTypes.Attribute{{Key: "recipient", Value: module}, {Key: "sender", Value: holder}, {Key: "amount", Value: "1000000" + denom}}},
		{Type: "burn", Attributes: []indexerTxTypes.Attribute{{Key: "burner", Value: module}, {Key: "amount", Value: "1000000" + denom}}},
	}}
	out := classify(&MsgTFBurn{Sender: holder, Denom: denom, Amount: "1000000"}, log)
	if len(out) != 1 || out[0].FromAddr != holder || out[0].ToAddr != module || out[0].Amount != "1000000" || out[0].Denom != denom || out[0].Category != string(CategoryTransfer) {
		t.Fatalf("events = %+v", out)
	}
}

// Real mainnet create-denom at height 30172324 paid 10 ATOM to the community pool.
func TestTFCreateDenomRecordsTheFeeAsExpense(t *testing.T) {
	creator := "cosmos1fec7fxmxrz00m6vu80vcjf48ppjq35l0ktx4et"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		{Type: "transfer", Attributes: []indexerTxTypes.Attribute{{Key: "recipient", Value: distributionModuleAddress}, {Key: "sender", Value: creator}, {Key: "amount", Value: "10000000uatom"}}},
	}}
	out := classify(&MsgTFCreateDenom{Sender: creator, Subdenom: "fomog"}, log)
	if len(out) != 1 || out[0].Category != string(CategoryExpense) || out[0].Amount != "10000000" || out[0].Denom != "uatom" || out[0].ToAddr != distributionModuleAddress || out[0].RewardTrigger != "denom_creation_fee" {
		t.Fatalf("events = %+v", out)
	}
}
