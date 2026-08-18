package tax

import (
	"encoding/binary"
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

func nested(num int, payload []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(num)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

// Real mainnet packet at height 29240383: sequence 2985, cosmoshub-0 ->
// 08-wasm-1369, one solidity-ABI encoded payload.
func TestRecvPacketV2Unmarshal(t *testing.T) {
	signer := "cosmos1as3hmygw03zvwzm76fx4rjpjpskq6fhwgj5vq6"
	payload := append(fieldBytes(1, "transfer"), fieldBytes(2, "transfer")...)
	payload = append(payload, fieldBytes(3, "ics20-1")...)
	payload = append(payload, fieldBytes(4, "application/x-solidity-abi")...)
	payload = append(payload, nested(5, []byte{0xde, 0xad, 0xbe, 0xef})...)

	packet := fieldVarint(1, 2985)
	packet = append(packet, fieldBytes(2, "cosmoshub-0")...)
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
	if m.Packet.SourceClient != "cosmoshub-0" || m.Packet.DestinationClient != "08-wasm-1369" {
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
	signer := "cosmos1as3hmygw03zvwzm76fx4rjpjpskq6fhwgj5vq6"
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

// The real bridge receive at height 29240383: 755073664548422 of the Ethereum
// denom credited to a Cosmos address, read off the fungible_token_packet event
// because the payload itself is solidity-ABI encoded.
func TestClassifyIBCV2ReceiveMainnetFixture(t *testing.T) {
	receiver := "cosmos1lqu9662kd4my6dww4gzp3730vew0gkwe0nl9ztjh0n5da0a8zc4swsvd22"
	sender := "0x40b186e853910d385c0492b137dede6fa61df432"
	denom := "0x812ba41e071c7b7fa4ebcfb62df5f45f6fa853ee"

	msg := &MsgRecvPacketV2{Signer: "cosmos1as3hmygw03zvwzm76fx4rjpjpskq6fhwgj5vq6"}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("recv_packet", [2]string{"packet_source_client", "cosmoshub-0"}, [2]string{"packet_sequence", "2985"}),
		ev("fungible_token_packet",
			[2]string{"sender", sender},
			[2]string{"receiver", receiver},
			[2]string{"denom", denom},
			[2]string{"amount", "755073664548422"},
			[2]string{"success", "true"}),
	}}

	out := classify(msg, log)
	if len(out) != 1 {
		t.Fatalf("expected 1 ibc_in event, got %d: %+v", len(out), out)
	}
	if out[0].Category != string(CategoryIBCIn) {
		t.Fatalf("category = %q, want ibc_in", out[0].Category)
	}
	if out[0].Amount != "755073664548422" || out[0].Denom != denom {
		t.Fatalf("amount/denom = %s / %s", out[0].Amount, out[0].Denom)
	}
	if out[0].ToAddr != receiver || out[0].FromAddr != sender {
		t.Fatalf("addresses = %s -> %s", out[0].FromAddr, out[0].ToAddr)
	}
}

// A failed receive writes an error acknowledgement and moves no funds, so it is
// not an acquisition.
func TestClassifyIBCV2FailedReceiveIsNotIncome(t *testing.T) {
	msg := &MsgRecvPacketV2{}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("fungible_token_packet",
			[2]string{"receiver", "cosmos1abc"},
			[2]string{"denom", "uatom"},
			[2]string{"amount", "1000"},
			[2]string{"success", "false"}),
	}}
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("a failed receive produced %d events: %+v", len(out), out)
	}
}

// One receive can carry several payloads, each its own acquisition.
func TestClassifyIBCV2MultiplePayloads(t *testing.T) {
	msg := &MsgRecvPacketV2{}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("fungible_token_packet", [2]string{"receiver", "cosmos1abc"}, [2]string{"denom", "uatom"}, [2]string{"amount", "100"}, [2]string{"success", "true"}),
		ev("fungible_token_packet", [2]string{"receiver", "cosmos1abc"}, [2]string{"denom", "uosmo"}, [2]string{"amount", "250"}, [2]string{"success", "true"}),
	}}
	out := classify(msg, log)
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
