package tax

import (
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

// Shapes taken from Cosmos Hub archive blocks. A CW20-ICS20 contract on the
// counterparty sends from its wasm.<contract> port (height 27003615).
func TestV1ReceiveFromCW20ICS20Port(t *testing.T) {
	const (
		denom    = "cw20:neutron15erx8xrlhentrzkud2zj6dywp23vkdfmed2ztyl3f5hjm39myn3shrqasq"
		receiver = "cosmos1r6urzhdn72mypazwefer3zq3ypwaxke2ce4ksf"
		escrow   = "cosmos1yl6hdjhmkf37639730gffanpzndzdpmhwlkfhr"
		voucher  = "ibc/88C8D98E544B20319F69F3A7E0F9E0FB3D4163DF2FFCAF7B603B42DE9E0EC333"
		srcPort  = "wasm.neutron1hzrh0s88zlh8vzuw9k6afhuyn2ltdcgwrj85qac7acm79v7rt0ts9rwxq6"
	)
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", escrow}, [2]string{"amount", "50000000" + voucher}),
		ev("coin_received", [2]string{"receiver", receiver}, [2]string{"amount", "50000000" + voucher}),
		ev("transfer", [2]string{"recipient", receiver}, [2]string{"sender", escrow}, [2]string{"amount", "50000000" + voucher}),
		ev("fungible_token_packet", [2]string{"sender", "neutron16a87dpmkutsfx7cddxt749urv62nkcajluw35d"}, [2]string{"receiver", receiver}, [2]string{"denom", denom}, [2]string{"amount", "50000000"}, [2]string{"memo", ""}, [2]string{"success", "true"}),
		ev("write_acknowledgement", [2]string{"packet_sequence", "25373"}, [2]string{"packet_src_port", srcPort}, [2]string{"packet_src_channel", "channel-7196"}, [2]string{"packet_dst_port", "transfer"}, [2]string{"packet_dst_channel", "channel-1434"}, [2]string{"packet_ack_hex", "7b22726573756c74223a2241513d3d227d"}),
	}}
	packet := chantypes.Packet{Sequence: 25373, SourcePort: srcPort, SourceChannel: "channel-7196", DestinationPort: "transfer", DestinationChannel: "channel-1434"}
	data := transfertypes.FungibleTokenPacketData{Denom: denom, Amount: "50000000", Sender: "neutron16a87dpmkutsfx7cddxt749urv62nkcajluw35d", Receiver: receiver}
	out, issues := ibcV1ReceiveEvents(log, packet, data)
	if len(issues) != 0 || len(out) != 1 || out[0].Category != string(CategoryIBCIn) || out[0].Denom != voucher || out[0].Amount != "50000000" || out[0].ToAddr != receiver {
		t.Fatalf("CW20-ICS20 receive not classified as one voucher credit: out=%+v issues=%v", out, issues)
	}
	// The local end must still be the transfer app.
	packet.DestinationPort = "wasm.cosmos1contract"
	if out, issues := ibcV1ReceiveEvents(log, packet, data); len(out) != 0 || len(issues) != 1 {
		t.Fatalf("non-transfer destination port classified: out=%+v issues=%v", out, issues)
	}
}

// Packet-forward middleware on ibc-go v7 emitted ibc_transfer without an
// amount (height 18623497). The memo's next-hop receiver identifies the hop.
func TestV1ForwardPassThroughWithoutTransferAmount(t *testing.T) {
	const (
		placeholder  = "cosmos1kkxgg2lhz753wgrrgl0ehzp5lxfam9qs7ur9p6"
		intermediate = "cosmos1jy3852fdwn9crt9urznu0mjd4rt0pwgz2se09z"
		nextHop      = "neutron1kkxgg2lhz753wgrrgl0ehzp5lxfam9qs6r28ma"
		escrow       = "cosmos1aqas7uqu6wazrlwc9ng4rzfax2nnmc8qhngtjs"
		denom        = "transfer/channel-0/uatom"
	)
	build := func(forwardTo string) *txlog.LogMessage {
		return &txlog.LogMessage{Events: []txlog.LogMessageEvent{
			ev("coin_received", [2]string{"receiver", intermediate}, [2]string{"amount", "15394236uatom"}),
			ev("fungible_token_packet", [2]string{"module", "transfer"}, [2]string{"sender", "osmo1kkxgg2lhz753wgrrgl0ehzp5lxfam9qsk8s4hg"}, [2]string{"receiver", intermediate}, [2]string{"denom", denom}, [2]string{"amount", "15394236"}, [2]string{"memo", ""}, [2]string{"success", "true"}),
			ev("coin_received", [2]string{"receiver", escrow}, [2]string{"amount", "15394236uatom"}),
			ev("ibc_transfer", [2]string{"sender", intermediate}, [2]string{"receiver", forwardTo}),
		}}
	}
	packet := chantypes.Packet{Sequence: 2458280, SourcePort: "transfer", SourceChannel: "channel-0", DestinationPort: "transfer", DestinationChannel: "channel-141"}
	data := transfertypes.FungibleTokenPacketData{Denom: denom, Amount: "15394236", Sender: "osmo1kkxgg2lhz753wgrrgl0ehzp5lxfam9qsk8s4hg", Receiver: placeholder,
		Memo: `{"forward":{"receiver":"` + nextHop + `","port":"transfer","channel":"channel-569"}}`}
	if out, issues := ibcV1ReceiveEvents(build(nextHop), packet, data); len(out) != 0 || len(issues) != 0 {
		t.Fatalf("forward hop should pass through: out=%+v issues=%v", out, issues)
	}
	// An outgoing transfer to anyone but the memo's next hop is not this forward.
	if out, issues := ibcV1ReceiveEvents(build("neutron1someoneelse"), packet, data); len(out) != 0 || len(issues) != 1 {
		t.Fatalf("unrelated outgoing transfer accepted as the forward: out=%+v issues=%v", out, issues)
	}
}
