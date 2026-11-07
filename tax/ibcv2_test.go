package tax

import (
	"encoding/binary"
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

func nested(num int, payload []byte) []byte {
	if num < 1 || num > (1<<29)-1 {
		panic("invalid protobuf field number")
	}
	out := binary.AppendUvarint(nil, uint64(num)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

// Real mainnet packet at height 29240383: sequence 2985, cosmoshub-0 ->
// 08-wasm-1369, one solidity-ABI encoded payload.
func TestRecvPacketV2Unmarshal(t *testing.T) {
	signer := fixtureIBCReceiver
	payload := append(fieldBytes(1, string(CategoryTransfer)), fieldBytes(2, string(CategoryTransfer))...)
	payload = append(payload, fieldBytes(3, "ics20-1")...)
	payload = append(payload, fieldBytes(4, "application/x-solidity-abi")...)
	payload = append(payload, nested(5, []byte{0xde, 0xad, 0xbe, 0xef})...)

	packet := fieldVarint(1, 2985)
	packet = append(packet, fieldBytes(2, fixtureHubClient)...)
	packet = append(packet, fieldBytes(3, "08-wasm-1369")...)
	packet = append(packet, fieldVarint(4, 1767992242)...)
	packet = append(packet, nested(5, payload)...)

	b := nested(1, packet)
	b = append(b, nested(2, []byte{0x01, 0x02})...)
	b = append(b, fieldBytes(4, signer)...)

	var m MsgRecvPacketV2
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Packet.Sequence != 2985 {
		t.Fatalf("sequence = %d", m.Packet.Sequence)
	}
	if m.Packet.SourceClient != fixtureHubClient || m.Packet.DestinationClient != safetyV2Client {
		t.Fatalf("clients: %+v", m.Packet)
	}
	if len(m.Packet.PayloadsRaw) != 1 {
		t.Fatalf("payloads = %d, want 1", len(m.Packet.PayloadsRaw))
	}
	if m.Signer != signer {
		t.Fatalf("signer = %q", m.Signer)
	}
}

// MsgTimeout puts the signer on field 5, not 4. Getting that wrong would leave
// the signer empty and silently drop the tx's signer attribution.
func TestTimeoutV2SignerIsFieldFive(t *testing.T) {
	signer := fixtureIBCReceiver
	b := append(nested(1, fieldVarint(1, 7)), fieldBytes(5, signer)...)
	var m MsgTimeoutV2
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Signer != signer {
		t.Fatalf("signer = %q, want %q (field 5)", m.Signer, signer)
	}
	// field 4 must NOT populate the signer
	var other MsgTimeoutV2
	if err := other.Unmarshal(append(nested(1, fieldVarint(1, 7)), fieldBytes(4, signer)...)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if other.Signer != "" {
		t.Fatalf("field 4 should not be the signer on MsgTimeout")
	}
}

// This packet-event excerpt omits bank evidence. The Solidity packet denom is
// not the Hub's local bank denom and must not become a fabricated acquisition.
func TestClassifyIBCV2ReceivePacketExcerptNeedsBankEvidence(t *testing.T) {
	receiver := "cosmos1lqu9662kd4my6dww4gzp3730vew0gkwe0nl9ztjh0n5da0a8zc4swsvd22"
	sender := "0x40b186e853910d385c0492b137dede6fa61df432"
	denom := "0x812ba41e071c7b7fa4ebcfb62df5f45f6fa853ee"

	msg := &MsgRecvPacketV2{Signer: fixtureIBCReceiver}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("recv_packet", [2]string{"packet_source_client", fixtureHubClient}, [2]string{attributeIBCPacketSequence, "2985"}),
		ev(eventFungibleTokenPacket,
			[2]string{attributeSender, sender},
			[2]string{attributeReceiver, receiver},
			[2]string{attributeDenom, denom},
			[2]string{attributeAmount, "755073664548422"},
			[2]string{attributeSuccess, attributeTrue}),
	}}

	if out := safetyParseReceive(t, msg, log, true); len(out) != 0 {
		t.Fatalf("packet-only evidence produced acquisitions: %+v", out)
	}
}

// A failed receive writes an error acknowledgement and moves no funds, so it is
// not an acquisition.
func TestClassifyIBCV2FailedReceiveIsNotIncome(t *testing.T) {
	msg := &MsgRecvPacketV2{}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(eventFungibleTokenPacket,
			[2]string{attributeReceiver, fixtureCosmosAddress},
			[2]string{attributeDenom, fixtureNativeDenom},
			[2]string{attributeAmount, "1000"},
			[2]string{attributeSuccess, attributeFalse}),
	}}
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("a failed receive produced %d events: %+v", len(out), out)
	}
}

// One receive can carry several payloads, each its own acquisition.
func TestClassifyIBCV2MultiplePayloads(t *testing.T) {
	msg := &MsgRecvPacketV2{Packet: safetyV2Packet(2)}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(eventCoinReceived, [2]string{attributeReceiver, fixtureCosmosAddress}, [2]string{attributeAmount, "100uatom,250uosmo"}),
		ev(eventFungibleTokenPacket, [2]string{attributeReceiver, fixtureCosmosAddress}, [2]string{attributeDenom, "transfer/cosmoshub-0/uatom"}, [2]string{attributeAmount, "100"}, [2]string{attributeSuccess, attributeTrue}),
		ev(eventFungibleTokenPacket, [2]string{attributeReceiver, fixtureCosmosAddress}, [2]string{attributeDenom, "transfer/cosmoshub-0/uosmo"}, [2]string{attributeAmount, "250"}, [2]string{attributeSuccess, attributeTrue}),
	}}
	out := safetyParseReceive(t, msg, log, false)
	if len(out) != 2 {
		t.Fatalf("expected 2 events, got %d", len(out))
	}
}

func TestIBCV2TypesAreRegistrable(t *testing.T) {
	types := IBCChannelV2MsgTypes()
	if len(types) != 4 {
		t.Fatalf("expected 4 channel v2 types, got %d", len(types))
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

// Real mainnet event excerpt at height 29349107, not proof of acquisition.
// Core v10.1.1 prefixes events from a discarded application receive cache on
// both event types and attribute keys. A lower transfer app's success=true
// does not establish the final middleware outcome or a committed wallet credit.
func TestClassifyIBCV2CallbackPacketExcerptNeedsBankEvidence(t *testing.T) {
	receiver := "cosmos1lqu9662kd4my6dww4gzp3730vew0gkwe0nl9ztjh0n5da0a8zc4swsvd22"
	sender := "0xca6d9fd15df411de3c1324ac01f2205e583b5c21"
	denom := "transfer/cosmoshub-0/transfer/channel-1337/aseda"

	msg := &MsgRecvPacketV2{}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("ibccallbackerror-fungible_token_packet",
			[2]string{"ibccallbackerror-sender", sender},
			[2]string{"ibccallbackerror-receiver", receiver},
			[2]string{"ibccallbackerror-denom", denom},
			[2]string{"ibccallbackerror-amount", "54994958330602429076480"},
			[2]string{"ibccallbackerror-success", attributeTrue}),
	}}

	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("callback packet excerpt without bank credit produced acquisitions: %+v", out)
	}
}

// The prefix must not smuggle in a genuinely failed transfer.
func TestClassifyIBCV2PrefixedFailedTransferStillExcluded(t *testing.T) {
	msg := &MsgRecvPacketV2{}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("ibccallbackerror-fungible_token_packet",
			[2]string{"ibccallbackerror-receiver", fixtureCosmosAddress},
			[2]string{"ibccallbackerror-denom", fixtureNativeDenom},
			[2]string{"ibccallbackerror-amount", "1000"},
			[2]string{"ibccallbackerror-success", attributeFalse}),
	}}
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("a failed transfer produced %d events", len(out))
	}
}
