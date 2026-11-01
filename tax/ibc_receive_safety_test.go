package tax

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

// Synthetic source-shaped cases. These are not archive incidence assertions.
func TestSafetyReceiveRollbackAndOptionalCallbackCommit(t *testing.T) {
	for _, generation := range []string{"v1", "v2"} {
		t.Run(generation, func(t *testing.T) {
			var msg sdk.Msg = &chantypes.MsgRecvPacket{Packet: safetyPacket(t, safetyNativeReturn)}
			packetDenom, bankDenom := safetyNativeReturn, fixtureNativeDenom
			ack := ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "23"}, [2]string{"packet_ack", `{"error":"enclosing middleware rejected receipt"}`})
			if generation == "v2" {
				msg = &MsgRecvPacketV2{Packet: safetyV2Packet(1)}
				packetDenom, bankDenom = "ucoin", safetyV2Voucher
				sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
				ack = ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "23"}, [2]string{attributeIBCEncodedAckHex, hex.EncodeToString(nested(1, sentinel[:]))})
			}
			app := safetyReceive(packetDenom)
			credits := safetyCredit(del, bankDenom)
			// The app emitted success before enclosing middleware rejected it.
			rolledBack := append(safetyPrefix(append(credits, app)), ack)
			if out := safetyParseReceive(t, msg, &txlog.LogMessage{Events: rolledBack}, false); len(out) != 0 {
				t.Fatalf("error ack and discarded bank events fabricated acquisition: %+v", out)
			}
			// Even unrelated committed credits cannot resurrect a prefixed app.
			rolledBack = append(rolledBack, safetyCredit(del, bankDenom)...)
			if out := safetyParseReceive(t, msg, &txlog.LogMessage{Events: rolledBack}, false); len(out) != 0 {
				t.Fatalf("unrelated bank credit resurrected discarded app: %+v", out)
			}
			// Optional callback failure can leave the transfer committed (v1
			// callbacks v8.7.0). Keep real unprefixed app and bank evidence. The
			// same evidence boundary applies to v2 middleware that permits it;
			// callbacks/v2 v10.1.1 instead returns Failure on callback error.
			committed := append(safetyCredit(del, bankDenom), app)
			committed = append(committed, safetyPrefix(safetyCredit(del, "unrelated"))...)
			committed = append(committed, ev("ibc_receive_packet", [2]string{"callback_error", "optional callback failed"}))
			if out := safetyParseReceive(t, msg, &txlog.LogMessage{Events: committed}, false); len(out) != 1 || out[0].Denom != bankDenom {
				t.Fatalf("committed transfer lost after optional callback failure: %+v", out)
			}
		})
	}
}

func TestSafetyV1ExplicitErrorAckVetoesAppSuccess(t *testing.T) {
	msg := &chantypes.MsgRecvPacket{Packet: safetyPacket(t, safetyNativeReturn)}
	for _, key := range []string{"packet_ack", "packet_ack_hex"} {
		ack := `{"error":"enclosing middleware failed"}`
		if key == "packet_ack_hex" {
			ack = hex.EncodeToString([]byte(ack))
		}
		log := &txlog.LogMessage{Events: append(safetyCredit(del, fixtureNativeDenom), safetyReceive(safetyNativeReturn), ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "23"}, [2]string{key, ack}))}
		if out := safetyParseReceive(t, msg, log, false); len(out) != 0 {
			t.Fatalf("%s: final error ack fabricated receipt: %+v", key, out)
		}
		// An acknowledgement of a different packet is not a veto.
		log.Events[len(log.Events)-1] = ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "24"}, [2]string{key, ack})
		if out := safetyParseReceive(t, msg, log, false); len(out) != 1 {
			t.Fatalf("%s: unrelated ack removed committed receive: %+v", key, out)
		}
	}
}

func TestSafetyV2SinglePayloadFinalErrorCannotFabricateReceipt(t *testing.T) {
	sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
	ack := hex.EncodeToString(nested(1, sentinel[:]))
	msg := &MsgRecvPacketV2{Packet: safetyV2Packet(1)}
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyReceive("ucoin"), ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "23"}, [2]string{attributeIBCEncodedAckHex, ack}))}
	if out := safetyParseReceive(t, msg, log, true); len(out) != 0 {
		t.Fatalf("contradictory final failure fabricated receipt: %+v", out)
	}
	log.Events[len(log.Events)-1] = ev("write_acknowledgement", [2]string{attributeIBCPacketSequence, "24"}, [2]string{attributeIBCEncodedAckHex, ack})
	if out := safetyParseReceive(t, msg, log, false); len(out) != 1 {
		t.Fatalf("different packet's ack suppressed committed receipt: %+v", out)
	}
}

func TestSafetyReceivePrefixedBankEvidenceNeverCommits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]txlog.LogMessageEvent) []txlog.LogMessageEvent
	}{
		{"all_prefixed", safetyPrefix},
		{"types_only", func(events []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			for i := range events {
				events[i].Type = ibcCallbackErrorPrefix + events[i].Type
			}
			return events
		}},
		{"keys_only", func(events []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			for i := range events {
				for j := range events[i].Attributes {
					events[i].Attributes[j].Key = ibcCallbackErrorPrefix + events[i].Attributes[j].Key
				}
			}
			return events
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &txlog.LogMessage{Events: append(tc.mutate(safetyCredit(del, safetyV2Voucher)), safetyReceive("ucoin"))}
			if out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(1)}, log, true); len(out) != 0 {
				t.Fatalf("prefixed bank evidence fabricated receipt: %+v", out)
			}
		})
	}
	// A prefixed coin_received must not suppress a committed transfer fallback.
	log := &txlog.LogMessage{Events: append(safetyPrefix(safetyCredit(del, safetyV2Voucher)[:1]), safetyCredit(del, safetyV2Voucher)[1], safetyReceive("ucoin"))}
	if out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(1)}, log, false); len(out) != 1 {
		t.Fatalf("committed transfer fallback lost: %+v", out)
	}
}

func TestSafetyV2EqualAmountsUsePacketDenomNotCallbackCoin(t *testing.T) {
	packet := safetyV2Packet(2)
	otherVoucher := ibcLocalCandidates("transfer/08-wasm-1369/uother")[1]
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyCredit(del, otherVoucher)...)}
	log.Events = append(log.Events, safetyReceive("ucoin"), safetyReceive("uother"))
	out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: packet}, log, false)
	if len(out) != 2 || out[0].Denom != safetyV2Voucher || out[1].Denom != otherVoucher {
		t.Fatalf("equal multi-payload credits lost or swapped: %+v", out)
	}
	// One successful payload plus an unrelated exact-size callback credit.
	log.Events = log.Events[:len(log.Events)-1]
	out = safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(1)}, log, false)
	if len(out) != 1 || out[0].Denom != safetyV2Voucher {
		t.Fatalf("callback coin chosen as packet asset: %+v", out)
	}
	// Without the actual packet credit, that unrelated exact-size coin is not
	// evidence of this acquisition, even though it is the only bank credit.
	log.Events = append(safetyCredit(del, otherVoucher), safetyReceive("ucoin"))
	if out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(1)}, log, true); len(out) != 0 {
		t.Fatalf("unrelated callback coin fabricated receipt: %+v", out)
	}
}

func TestSafetyV2IncompleteReceiveRetainsKnownPayloadDiagnostically(t *testing.T) {
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyReceive("ucoin"), safetyReceive("missing"))}
	out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(2)}, log, true)
	if len(out) != 1 || out[0].Denom != safetyV2Voucher {
		t.Fatalf("known payload lost or missing payload fabricated: %+v", out)
	}
}

func TestSafetyV2LaterFailedPayloadKeepsEarlierCommittedCredit(t *testing.T) {
	// channel-v2 keeper v10.1.1 writes each successful payload cache before
	// executing the next. A sentinel ack alone cannot undo that earlier credit.
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyReceive("ucoin"))}
	log.Events = append(log.Events, safetyPrefix(append(safetyCredit(del, "unrelated"), safetyReceive("failed")))...)
	sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
	log.Events = append(log.Events, ev("write_acknowledgement", [2]string{attributeIBCEncodedAckHex, hex.EncodeToString(nested(1, sentinel[:]))}))
	out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(2)}, log, false)
	if len(out) != 1 || out[0].Denom != safetyV2Voucher {
		t.Fatalf("earlier committed payload lost or later rollback fabricated: %+v", out)
	}
}

func TestSafetyV2ReceiveMissingOrAmbiguousEvidenceIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  PacketV2
		credits []txlog.LogMessageEvent
	}{
		{"no_bank_credit", safetyV2Packet(1), nil},
		{"missing_packet_context_equal_coins", PacketV2{}, append(safetyCredit(del, safetyV2Voucher), safetyCredit(del, fixtureNativeDenom)...)},
		{"raw_trace_is_not_local_bank_denom", safetyV2Packet(1), safetyCredit(del, "transfer/08-wasm-1369/ucoin")},
		{"insufficient_credit", safetyV2Packet(1), []txlog.LogMessageEvent{ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "99" + safetyV2Voucher})}},
		{"malformed_payload", PacketV2{SourceClient: fixtureHubClient, DestinationClient: safetyV2Client, PayloadsRaw: [][]byte{{0xff}}}, safetyCredit(del, safetyV2Voucher)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &txlog.LogMessage{Events: append(tc.credits, safetyReceive("ucoin"))}
			if out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: tc.packet}, log, true); len(out) != 0 {
				t.Fatalf("incomplete evidence fabricated receipt: %+v", out)
			}
		})
	}
}

func TestSafetyV2LocalDenomIsNotAnArbitraryPathOrNativeHash(t *testing.T) {
	msg := &MsgRecvPacketV2{Packet: safetyV2Packet(1)}
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyCredit(del, "transfer/08-wasm-1369/ucoin")...)}
	log.Events = append(log.Events, safetyReceive("ucoin"))
	out := safetyParseReceive(t, msg, log, false)
	if len(out) != 1 || out[0].Denom != safetyV2Voucher {
		t.Fatalf("raw path masqueraded as local voucher: %+v", out)
	}
	log.Events = append(safetyCredit(del, ibcLocalCandidates(fixtureNativeDenom)[1]), safetyReceive("transfer/cosmoshub-0/uatom"))
	if out := safetyParseReceive(t, msg, log, true); len(out) != 0 {
		t.Fatalf("unrelated native-denom hash fabricated receipt: %+v", out)
	}
}

func TestSafetyV1AmbiguousLocalCreditIsIncomplete(t *testing.T) {
	path := "transfer/08-wasm-1369/ucoin"
	packetDenom := "transfer/channel-7/" + path
	packet := safetyPacket(t, packetDenom)
	log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyCredit(del, path)...)}
	log.Events = append(log.Events, safetyReceive(packetDenom))
	if out := safetyParseReceive(t, &chantypes.MsgRecvPacket{Packet: packet}, log, true); len(out) != 0 {
		t.Fatalf("ambiguous local denominations fabricated receipt: %+v", out)
	}
}

func TestSafetyV1OnlyActualLocalDenominationCanMatch(t *testing.T) {
	for _, tc := range []struct{ packetDenom, unrelatedCredit string }{
		{fixtureOsmosisDenom, "transfer/channel-141/uosmo"},
		{safetyNativeReturn, ibcLocalCandidates(fixtureNativeDenom)[1]},
	} {
		packet := safetyPacket(t, tc.packetDenom)
		log := &txlog.LogMessage{Events: append(safetyCredit(del, tc.unrelatedCredit), safetyReceive(tc.packetDenom))}
		if out := safetyParseReceive(t, &chantypes.MsgRecvPacket{Packet: packet}, log, true); len(out) != 0 {
			t.Fatalf("unrelated local credit fabricated receipt: %+v", out)
		}
	}
}

func TestSafetyV1IncompleteReceiveRetainsAuthzSiblingDiagnostically(t *testing.T) {
	packet := safetyPacket(t, fixtureOsmosisDenom)
	msg := &authz.MsgExec{Msgs: []*codectypes.Any{
		safetyUncached(t, &bank.MsgSend{FromAddress: other, ToAddress: del, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))}),
		safetyUncached(t, &chantypes.MsgRecvPacket{Packet: packet}),
	}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{safetyReceive(fixtureOsmosisDenom)}}
	log.Events[0].Attributes = append(log.Events[0].Attributes, txlog.Attribute{Key: attributeAuthzMsgIndex, Value: "1"})
	out := safetyParseReceive(t, msg, log, true)
	if len(out) != 1 || out[0].Category != string(CategoryTransfer) || out[0].Amount != "7" {
		t.Fatalf("known authz sibling lost or unsupported receipt fabricated: %+v", out)
	}
	// A successful receive with malformed packet data is incomplete, not empty.
	packet.Data = []byte("bad packet JSON")
	if out := safetyParseReceive(t, &chantypes.MsgRecvPacket{Packet: packet}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{safetyReceive(fixtureOsmosisDenom)}}, true); len(out) != 0 {
		t.Fatalf("malformed packet fabricated receipt: %+v", out)
	}
}

// Shape of cosmoshub-4 23763495 tx 706ED2B0: PFM credits a derived
// intermediate and forwards the coins on, so the packet receiver gets nothing.
func TestSafetyV1ForwardPassThroughIsNotAcquisition(t *testing.T) {
	const intermediate = "cosmos1intermediate"
	data, err := json.Marshal(map[string]string{attributeSender: other, attributeReceiver: del, attributeAmount: "100", attributeDenom: safetyNativeReturn, "memo": `{"forward":{"receiver":"pryzm1next","port":"transfer","channel":"channel-859"}}`})
	if err != nil {
		t.Fatal(err)
	}
	packet := safetyPacket(t, safetyNativeReturn)
	packet.Data = data
	msg := &chantypes.MsgRecvPacket{Packet: packet}
	app := ev(eventFungibleTokenPacket, [2]string{attributeSender, other}, [2]string{attributeReceiver, intermediate}, [2]string{attributeAmount, "100"}, [2]string{attributeDenom, safetyNativeReturn}, [2]string{attributeSuccess, attributeTrue})
	onward := ev("ibc_transfer", [2]string{attributeSender, intermediate}, [2]string{attributeReceiver, "pryzm1next"}, [2]string{attributeAmount, "100"}, [2]string{attributeDenom, fixtureNativeDenom})
	events := append(safetyCredit(intermediate, fixtureNativeDenom), app, onward)
	if out := safetyParseReceive(t, msg, &txlog.LogMessage{Events: events}, false); len(out) != 0 {
		t.Fatalf("forward pass-through fabricated receipt: %+v", out)
	}
	// Without the onward transfer the coins stayed somewhere unexplained.
	safetyParseReceive(t, msg, &txlog.LogMessage{Events: append(safetyCredit(intermediate, fixtureNativeDenom), app)}, true)
	// A discarded onward transfer is not evidence either.
	safetyParseReceive(t, msg, &txlog.LogMessage{Events: append(append(safetyCredit(intermediate, fixtureNativeDenom), app), safetyPrefix([]txlog.LogMessageEvent{onward})...)}, true)
	// Without a forward memo the mismatch is still incomplete.
	plain := &chantypes.MsgRecvPacket{Packet: safetyPacket(t, safetyNativeReturn)}
	safetyParseReceive(t, plain, &txlog.LogMessage{Events: events}, true)
}
