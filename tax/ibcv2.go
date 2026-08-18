// IBC channel v2 (the "Eureka" packet flow) landed in ibc-go v10. This indexer
// links ibc-go v7, so ibc.core.channel.v2 messages are absent from the codec and
// used to be skipped. That mattered: a v2 MsgRecvPacket is an inbound transfer,
// which is an acquisition, and on Cosmos Hub these carry bridged assets in from
// Ethereum.
//
// As with x/liquid, we declare the wire shape rather than pull in an
// incompatible ibc-go. Field numbers come from ibc-go
// proto/ibc/core/channel/v2/{tx,packet}.proto. Note MsgTimeout's signer is
// field 5, not 4.
//
// The packet payload is deliberately kept as raw bytes. v2 payloads are not
// necessarily the JSON FungibleTokenPacketData that v1 carries: on Cosmos Hub
// they arrive encoded as application/x-solidity-abi from the Ethereum bridge, so
// decoding them would mean an ABI decoder. The module emits a
// fungible_token_packet event with the sender, receiver, denom, amount and
// success flag already extracted, which is the same four fields the v1 path
// reads out of the packet data, so classification uses that instead.

package tax

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	TypeURLV2RecvPacket      = "/ibc.core.channel.v2.MsgRecvPacket"
	TypeURLV2Timeout         = "/ibc.core.channel.v2.MsgTimeout"
	TypeURLV2Acknowledgement = "/ibc.core.channel.v2.MsgAcknowledgement"
	TypeURLV2SendPacket      = "/ibc.core.channel.v2.MsgSendPacket"
)

// IBCChannelV2MsgTypes maps the channel v2 type URLs to registrable instances.
func IBCChannelV2MsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLV2RecvPacket:      &MsgRecvPacketV2{},
		TypeURLV2Timeout:         &MsgTimeoutV2{},
		TypeURLV2Acknowledgement: &MsgAcknowledgementV2{},
		TypeURLV2SendPacket:      &MsgSendPacketV2{},
	}
}

// PacketV2 is ibc.core.channel.v2.Packet. Payloads stay as raw bytes.
type PacketV2 struct {
	Sequence          uint64   `protobuf:"varint,1,opt,name=sequence,proto3" json:"sequence,omitempty"`
	SourceClient      string   `protobuf:"bytes,2,opt,name=source_client,json=sourceClient,proto3" json:"source_client,omitempty"`
	DestinationClient string   `protobuf:"bytes,3,opt,name=destination_client,json=destinationClient,proto3" json:"destination_client,omitempty"`
	TimeoutTimestamp  uint64   `protobuf:"varint,4,opt,name=timeout_timestamp,json=timeoutTimestamp,proto3" json:"timeout_timestamp,omitempty"`
	PayloadsRaw       [][]byte `protobuf:"bytes,5,rep,name=payloads,proto3" json:"payloads,omitempty"`
}

func (p *PacketV2) unmarshal(b []byte) error {
	*p = PacketV2{}
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			p.Sequence = f.varint
		case 2:
			p.SourceClient = string(f.bytes)
		case 3:
			p.DestinationClient = string(f.bytes)
		case 4:
			p.TimeoutTimestamp = f.varint
		case 5:
			p.PayloadsRaw = append(p.PayloadsRaw, append([]byte(nil), f.bytes...))
		}
		return nil
	})
}

// --- MsgRecvPacket -----------------------------------------------------------

type MsgRecvPacketV2 struct {
	rawMsg
	Packet          PacketV2 `protobuf:"bytes,1,opt,name=packet,proto3" json:"packet"`
	ProofCommitment []byte   `protobuf:"bytes,2,opt,name=proof_commitment,json=proofCommitment,proto3" json:"proof_commitment,omitempty"`
	ProofHeightRaw  []byte   `protobuf:"bytes,3,opt,name=proof_height,json=proofHeight,proto3" json:"proof_height,omitempty"`
	Signer          string   `protobuf:"bytes,4,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgRecvPacketV2) Reset()        { *m = MsgRecvPacketV2{} }
func (m *MsgRecvPacketV2) ProtoMessage() {}
func (m *MsgRecvPacketV2) String() string {
	return fmt.Sprintf("channel/v2 MsgRecvPacket{seq:%d %s->%s}",
		m.Packet.Sequence, m.Packet.SourceClient, m.Packet.DestinationClient)
}
func (m *MsgRecvPacketV2) ValidateBasic() error         { return nil }
func (m *MsgRecvPacketV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgRecvPacketV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			return m.Packet.unmarshal(f.bytes)
		case 2:
			m.ProofCommitment = append([]byte(nil), f.bytes...)
		case 3:
			m.ProofHeightRaw = append([]byte(nil), f.bytes...)
		case 4:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}

// --- MsgTimeout (signer is field 5) ------------------------------------------

type MsgTimeoutV2 struct {
	rawMsg
	Packet          PacketV2 `protobuf:"bytes,1,opt,name=packet,proto3" json:"packet"`
	ProofUnreceived []byte   `protobuf:"bytes,2,opt,name=proof_unreceived,json=proofUnreceived,proto3" json:"proof_unreceived,omitempty"`
	ProofHeightRaw  []byte   `protobuf:"bytes,3,opt,name=proof_height,json=proofHeight,proto3" json:"proof_height,omitempty"`
	Signer          string   `protobuf:"bytes,5,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgTimeoutV2) Reset()        { *m = MsgTimeoutV2{} }
func (m *MsgTimeoutV2) ProtoMessage() {}
func (m *MsgTimeoutV2) String() string {
	return fmt.Sprintf("channel/v2 MsgTimeout{seq:%d}", m.Packet.Sequence)
}
func (m *MsgTimeoutV2) ValidateBasic() error         { return nil }
func (m *MsgTimeoutV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgTimeoutV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			return m.Packet.unmarshal(f.bytes)
		case 2:
			m.ProofUnreceived = append([]byte(nil), f.bytes...)
		case 3:
			m.ProofHeightRaw = append([]byte(nil), f.bytes...)
		case 5:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}

// --- MsgAcknowledgement (signer is field 5) ----------------------------------

type MsgAcknowledgementV2 struct {
	rawMsg
	Packet             PacketV2 `protobuf:"bytes,1,opt,name=packet,proto3" json:"packet"`
	AcknowledgementRaw []byte   `protobuf:"bytes,2,opt,name=acknowledgement,proto3" json:"acknowledgement,omitempty"`
	ProofAcked         []byte   `protobuf:"bytes,3,opt,name=proof_acked,json=proofAcked,proto3" json:"proof_acked,omitempty"`
	ProofHeightRaw     []byte   `protobuf:"bytes,4,opt,name=proof_height,json=proofHeight,proto3" json:"proof_height,omitempty"`
	Signer             string   `protobuf:"bytes,5,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgAcknowledgementV2) Reset()        { *m = MsgAcknowledgementV2{} }
func (m *MsgAcknowledgementV2) ProtoMessage() {}
func (m *MsgAcknowledgementV2) String() string {
	return fmt.Sprintf("channel/v2 MsgAcknowledgement{seq:%d}", m.Packet.Sequence)
}
func (m *MsgAcknowledgementV2) ValidateBasic() error         { return nil }
func (m *MsgAcknowledgementV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgAcknowledgementV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			return m.Packet.unmarshal(f.bytes)
		case 2:
			m.AcknowledgementRaw = append([]byte(nil), f.bytes...)
		case 3:
			m.ProofAcked = append([]byte(nil), f.bytes...)
		case 4:
			m.ProofHeightRaw = append([]byte(nil), f.bytes...)
		case 5:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}

// --- MsgSendPacket -----------------------------------------------------------

type MsgSendPacketV2 struct {
	rawMsg
	SourceClient     string   `protobuf:"bytes,1,opt,name=source_client,json=sourceClient,proto3" json:"source_client,omitempty"`
	TimeoutTimestamp uint64   `protobuf:"varint,2,opt,name=timeout_timestamp,json=timeoutTimestamp,proto3" json:"timeout_timestamp,omitempty"`
	PayloadsRaw      [][]byte `protobuf:"bytes,3,rep,name=payloads,proto3" json:"payloads,omitempty"`
	Signer           string   `protobuf:"bytes,4,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgSendPacketV2) Reset()        { *m = MsgSendPacketV2{} }
func (m *MsgSendPacketV2) ProtoMessage() {}
func (m *MsgSendPacketV2) String() string {
	return fmt.Sprintf("channel/v2 MsgSendPacket{%s}", m.SourceClient)
}
func (m *MsgSendPacketV2) ValidateBasic() error         { return nil }
func (m *MsgSendPacketV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgSendPacketV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.SourceClient = string(f.bytes)
		case 2:
			m.TimeoutTimestamp = f.varint
		case 3:
			m.PayloadsRaw = append(m.PayloadsRaw, append([]byte(nil), f.bytes...))
		case 4:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}
