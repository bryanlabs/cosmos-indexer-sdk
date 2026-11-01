package tax

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v7/modules/core/02-client/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

const (
	attributeIBCPacketSequence       = "packet_sequence"
	attributeIBCSrcChannel           = "packet_src_channel"
	attributeIBCDstChannel           = "packet_dst_channel"
	attributeIBCSrcPort              = "packet_src_port"
	attributeIBCDstPort              = "packet_dst_port"
	attributeIBCEncodedAckHex        = "encoded_acknowledgement_hex"
	attributeIBCGeneration           = "ibc_generation"
	attributePacketSourceClient      = "packet_source_client"
	attributePacketDestinationClient = "packet_destination_client"
	attributeRefundReceiver          = "refund_receiver"
	attributeRefundTokens            = "refund_tokens"
	eventTimeout                     = "timeout"
	ibcGenerationV1                  = "v1"
	ibcGenerationV2                  = "v2"
)

// ICS20 emits packet paths, while bank emits the actual local denomination.
// Sources: ibc-go v8.7.0 transfer/ibc_module.go and keeper/relay.go; v10.1.1
// transfer/internal/events/events.go, transfer/v2/ibc_module.go and keeper/relay.go.
// Neither transaction success nor packet contents alone prove a wallet credit.
// Core v8.5.1/v8.5.2/v8.7.0/v10.1.1 v1 and v10.1.1 channel-v2 RecvPacket
// re-emit discarded application-cache events with ibccallbackerror- prefixes.
// Normalization is for diagnostics/refund event parsing, NOT commit evidence.
func ibcEventType(ev txlog.LogMessageEvent) string {
	return strings.TrimPrefix(ev.Type, ibcCallbackErrorPrefix)
}

func ibcAttrs(ev txlog.LogMessageEvent) map[string]string {
	a := map[string]string{}
	for _, attr := range ev.Attributes {
		a[strings.TrimPrefix(attr.Key, ibcCallbackErrorPrefix)] = attr.Value
	}
	return a
}

// Attribute groups preserve old ABCI logs that coalesce repeated events of one
// type. All requested keys are scalar in one original event.
func ibcAttributeGroups(ev txlog.LogMessageEvent, keys ...string) []map[string]string {
	var out []map[string]string
	current := map[string]string{}
	for _, attr := range ev.Attributes {
		key := strings.TrimPrefix(attr.Key, ibcCallbackErrorPrefix)
		for _, wanted := range keys {
			if key != wanted {
				continue
			}
			if _, exists := current[key]; exists {
				out = append(out, current)
				current = map[string]string{}
			}
			current[key] = attr.Value
			break
		}
	}
	if len(current) != 0 {
		out = append(out, current)
	}
	return out
}

func ibcCommittedEvent(ev txlog.LogMessageEvent) bool {
	if strings.HasPrefix(ev.Type, ibcCallbackErrorPrefix) {
		return false
	}
	for _, a := range ev.Attributes {
		if strings.HasPrefix(a.Key, ibcCallbackErrorPrefix) {
			return false
		}
	}
	return true
}

// Use committed bank credits once, not both coin_received and transfer for the
// same send. Prefixed events describe discarded caches, even with success=true.
// The transfer fallback supports older event sets lacking coin_received.
func ibcCredits(log *txlog.LogMessage) map[string]sdk.Coins {
	out := map[string]sdk.Coins{}
	fallback := map[string]sdk.Coins{}
	if log == nil {
		return out
	}
	for _, ev := range log.Events {
		if !ibcCommittedEvent(ev) {
			continue
		}
		kind := ev.Type
		if kind != eventCoinReceived && kind != string(CategoryTransfer) {
			continue
		}
		key := attributeReceiver
		if kind == string(CategoryTransfer) {
			key = attributeRecipient
		}
		for _, a := range ibcAttributeGroups(ev, key, attributeAmount) {
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || a[key] == "" {
				continue
			}
			if kind == eventCoinReceived {
				out[a[key]] = out[a[key]].Add(coins...)
			} else {
				fallback[a[key]] = fallback[a[key]].Add(coins...)
			}
		}
	}
	for receiver, coins := range fallback {
		if _, ok := out[receiver]; !ok {
			out[receiver] = coins
		}
	}
	return out
}

// Consume exactly one unambiguous local bank denomination. Explicit packet
// denomination candidates are mandatory; amount-only bank matching is unsafe.
func takeIBCCredit(credits map[string]sdk.Coins, receiver, amount string, denoms ...string) (sdk.Coin, bool) {
	amt, ok := sdk.NewIntFromString(amount)
	if !ok || !amt.IsPositive() || receiver == "" {
		return sdk.Coin{}, false
	}
	var candidates []sdk.Coin
	for _, c := range credits[receiver] {
		if c.Amount.LT(amt) {
			continue
		}
		matches := false
		for _, denom := range denoms {
			if denom == c.Denom {
				matches = true
			}
		}
		if matches {
			candidates = append(candidates, sdk.NewCoin(c.Denom, amt))
		}
	}
	if len(candidates) != 1 {
		return sdk.Coin{}, false
	}
	c := candidates[0]
	credits[receiver] = credits[receiver].Sub(c)
	return c, true
}

func ibcV1ReceiveEvents(log *txlog.LogMessage, packet chantypes.Packet, data transfertypes.FungibleTokenPacketData) ([]TaxableEvent, []string) {
	success, anySuccess := false, false
	issue := fmt.Sprintf("ibc v1 receive sequence %d receiver %s: successful application cannot reconcile unambiguous committed bank credit", packet.Sequence, data.Receiver)
	for _, ev := range log.Events {
		if ev.Type != eventFungibleTokenPacket || !ibcCommittedEvent(ev) {
			continue
		}
		for _, a := range ibcReceiveGroups(ev) {
			anySuccess = anySuccess || (a[attributeSuccess] == attributeTrue && a["error"] == "")
			if a[attributeReceiver] != data.Receiver || a[attributeAmount] != data.Amount {
				continue
			}
			if a[attributeSuccess] == attributeFalse || a["error"] != "" {
				return nil, nil
			}
			success = success || a[attributeSuccess] == attributeTrue
		}
	}
	if ibcV1ReceiveErrorAck(log, packet) || !anySuccess {
		return nil, nil
	}
	if !success {
		return nil, []string{issue + " (packet/application event mismatch)"}
	}
	if packet.SourcePort != string(CategoryTransfer) || packet.DestinationPort != string(CategoryTransfer) || packet.SourceChannel == "" || packet.DestinationChannel == "" || data.Denom == "" {
		return nil, []string{issue + " (missing transfer packet context)"}
	}
	coin, ok := takeIBCCredit(ibcCredits(log), data.Receiver, data.Amount, ibcV1ReceiveCandidates(data.Denom, packet)...)
	if !ok {
		return nil, []string{issue}
	}
	return []TaxableEvent{{Category: string(CategoryIBCIn), FromAddr: data.Sender, ToAddr: data.Receiver, Amount: coin.Amount.String(), Denom: coin.Denom}}, nil
}

func ibcV1ReceiveCandidates(packetDenom string, packet chantypes.Packet) []string {
	path := ibcReceivePath(packetDenom, packet.SourcePort, packet.SourceChannel, packet.DestinationPort, packet.DestinationChannel)
	local := ibcLocalCandidates(path)
	if !strings.HasPrefix(packetDenom, packet.SourcePort+"/"+packet.SourceChannel+"/") || transfertypes.ParseDenomTrace(path).Path != "" {
		return local[1:]
	}
	// v1 can return a v2 trace. The linked v7 parser does not recognize client
	// IDs as hops; retain both interpretations for actual bank evidence to
	// disambiguate, and fail incomplete if both credits really exist.
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 3 && parts[0] == string(CategoryTransfer) && clienttypes.IsValidClientID(parts[1]) {
		return local
	}
	return local[:1]
}

func ibcReceiveGroups(ev txlog.LogMessageEvent) []map[string]string {
	return ibcAttributeGroups(ev, attributeSender, attributeReceiver, attributeDenom, attributeAmount, attributeSuccess, "error")
}

func ibcReceivePath(denom, sourcePort, sourceChannel, destinationPort, destinationChannel string) string {
	sourcePrefix := sourcePort + "/" + sourceChannel + "/"
	if strings.HasPrefix(denom, sourcePrefix) {
		return strings.TrimPrefix(denom, sourcePrefix)
	}
	return destinationPort + "/" + destinationChannel + "/" + denom
}

// A v1 error acknowledgement discards the entire application's receive cache.
// Match supplied packet identity fields, including nested callback ack events.
func ibcV1ReceiveErrorAck(log *txlog.LogMessage, packet chantypes.Packet) bool {
	identity := map[string]string{
		attributeIBCPacketSequence: strconv.FormatUint(packet.Sequence, 10),
		attributeIBCSrcPort:        packet.SourcePort, attributeIBCSrcChannel: packet.SourceChannel,
		attributeIBCDstPort: packet.DestinationPort, attributeIBCDstChannel: packet.DestinationChannel,
	}
	for _, ev := range log.Events {
		if ev.Type != "write_acknowledgement" || !ibcCommittedEvent(ev) {
			continue
		}
		a := ibcAttrs(ev)
		matches := true
		for key, value := range identity {
			if a[key] != "" && value != "" && value != "0" && a[key] != value {
				matches = false
			}
		}
		if !matches {
			continue
		}
		raw, _ := hex.DecodeString(a["packet_ack_hex"])
		if isErrorAcknowledgement(raw) || isErrorAcknowledgement([]byte(a["packet_ack"])) {
			return true
		}
	}
	return false
}

// v2 values can be ABI encoded, but ports and client IDs are ordinary protobuf
// strings. Derive local denominations using that context and the app's decoded
// denom, never amount-only matching against unrelated callback credits.
// Core v10.1.1 commits each successful payload cache before trying the next:
// a later sentinel error ack does not roll back preceding unprefixed credits.
func ibcV2ReceiveEvents(log *txlog.LogMessage, packet PacketV2) ([]TaxableEvent, []string) {
	var out []TaxableEvent
	var issues []string
	credits := ibcCredits(log)
	packet = ibcV2ReceiveContext(log, packet)
	contextOK := ibcV2TransferPorts(packet)
	failedSingle := len(packet.PayloadsRaw) == 1 && ibcV2ReceiveErrorAck(log, packet)
	for _, ev := range log.Events {
		if ev.Type != eventFungibleTokenPacket || !ibcCommittedEvent(ev) {
			continue
		}
		for _, a := range ibcReceiveGroups(ev) {
			if a[attributeSuccess] != attributeTrue || a["error"] != "" {
				continue
			}
			issue := fmt.Sprintf("ibc v2 receive sequence %d receiver %s denom %s: successful application cannot reconcile unambiguous committed bank credit", packet.Sequence, a[attributeReceiver], a[attributeDenom])
			if failedSingle {
				issues = append(issues, issue+" (single-payload final error acknowledgement contradicts app success)")
				continue
			}
			if !contextOK || a[attributeDenom] == "" {
				issues = append(issues, issue+" (missing transfer packet context)")
				continue
			}
			denom := ibcV2ReceiveDenom(a[attributeDenom], packet)
			coin, ok := takeIBCCredit(credits, a[attributeReceiver], a[attributeAmount], denom)
			if !ok {
				issues = append(issues, issue)
				continue
			}
			out = append(out, TaxableEvent{Category: string(CategoryIBCIn), FromAddr: a[attributeSender], ToAddr: a[attributeReceiver], Amount: coin.Amount.String(), Denom: coin.Denom})
		}
	}
	return out, issues
}

func ibcV2ReceiveErrorAck(log *txlog.LogMessage, packet PacketV2) bool {
	for _, ev := range log.Events {
		if ev.Type != "write_acknowledgement" || !ibcCommittedEvent(ev) {
			continue
		}
		a := ibcAttrs(ev)
		if (a[attributeIBCPacketSequence] != "" && packet.Sequence != 0 && a[attributeIBCPacketSequence] != strconv.FormatUint(packet.Sequence, 10)) ||
			(a["packet_source_client"] != "" && a["packet_source_client"] != packet.SourceClient) ||
			(a["packet_dest_client"] != "" && a["packet_dest_client"] != packet.DestinationClient) {
			continue
		}
		raw, err := hex.DecodeString(a[attributeIBCEncodedAckHex])
		if err == nil && isV2ErrorAcknowledgement(raw) {
			return true
		}
	}
	return false
}

// v10.1.1 transfer/v2 rejects slash-containing base denoms. After removing a
// returning packet's source hop, a slash-free path is native; every remaining
// trace and every newly prefixed voucher uses the IBC hash, never the raw path.
func ibcV2ReceiveDenom(packetDenom string, packet PacketV2) string {
	path := ibcReceivePath(packetDenom, string(CategoryTransfer), packet.SourceClient, string(CategoryTransfer), packet.DestinationClient)
	if strings.HasPrefix(packetDenom, string(CategoryTransfer)+"/"+packet.SourceClient+"/") && !strings.Contains(path, "/") {
		return path
	}
	return ibcLocalCandidates(path)[1]
}

// Stored event-only replay may lack a reconstructed packet. Use its original
// encoded_packet_hex, not guessed ports or the relayer's address.
func ibcV2ReceiveContext(log *txlog.LogMessage, packet PacketV2) PacketV2 {
	if len(packet.PayloadsRaw) != 0 || packet.SourceClient != "" || packet.DestinationClient != "" {
		return packet
	}
	for _, ev := range log.Events {
		if ev.Type != "recv_packet" || !ibcCommittedEvent(ev) {
			continue
		}
		raw, err := hex.DecodeString(ibcAttrs(ev)["encoded_packet_hex"])
		if err == nil && len(raw) != 0 {
			var decoded PacketV2
			if decoded.unmarshal(raw) == nil {
				return decoded
			}
		}
	}
	return packet
}

func ibcV2TransferPorts(packet PacketV2) bool {
	if packet.SourceClient == "" || packet.DestinationClient == "" || len(packet.PayloadsRaw) == 0 {
		return false
	}
	for _, raw := range packet.PayloadsRaw {
		var source, destination string
		err := scanFields(raw, func(f wireField) error {
			switch f.num {
			case 1:
				source = string(f.bytes)
			case 2:
				destination = string(f.bytes)
			}
			return nil
		})
		if err != nil || source != string(CategoryTransfer) || destination != string(CategoryTransfer) {
			return false
		}
	}
	return true
}

func isErrorAcknowledgement(raw []byte) bool {
	var ack struct {
		Error  string `json:"error"`
		Result string `json:"result"`
	}
	return json.Unmarshal(raw, &ack) == nil && ack.Error != "" && ack.Result == ""
}

func isV2ErrorAcknowledgement(raw []byte) bool {
	// v2 packet.proto: Acknowledgement.app_acknowledgements = repeated bytes 1.
	// v10.1.1 uses one SHA256 sentinel for the failed packet, not a JSON error.
	var acks [][]byte
	if err := scanFields(raw, func(f wireField) error {
		if f.num == 1 {
			acks = append(acks, f.bytes)
		}
		return nil
	}); err != nil || len(acks) != 1 {
		return false
	}
	sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
	return bytes.Equal(acks[0], sentinel[:])
}

func ibcV1RefundEvents(log *txlog.LogMessage, packet chantypes.Packet, timeout bool, ack []byte) ([]TaxableEvent, []string) {
	if (!timeout && !isErrorAcknowledgement(ack)) || packet.SourcePort != string(CategoryTransfer) {
		return nil, nil
	}
	application := ibcRefundApplication(log, timeout)
	var data transfertypes.FungibleTokenPacketData
	if json.Unmarshal(packet.Data, &data) != nil {
		if application {
			return nil, []string{"committed refund application has malformed packet data"}
		}
		return nil, nil
	}
	coin, ok := takeIBCCredit(ibcCredits(log), data.Sender, data.Amount, ibcRefundCandidates(data.Denom)...)
	if !ok {
		if application {
			return nil, []string{"committed refund application has no reconciled committed bank credit"}
		}
		return nil, nil
	}
	if !application {
		return nil, []string{"refund bank credit lacks committed application evidence"}
	}
	detail := map[string]string{
		attributeIBCPacketSequence: strconv.FormatUint(packet.Sequence, 10), attributeIBCSrcPort: packet.SourcePort,
		attributeIBCSrcChannel: packet.SourceChannel, attributeIBCDstPort: packet.DestinationPort,
		attributeIBCDstChannel: packet.DestinationChannel, attributeIBCGeneration: ibcGenerationV1, "principal_return": attributeTrue,
	}
	return ibcRefundRow(data.Sender, coin, timeout, detail), nil
}

func ibcRefundApplication(log *txlog.LogMessage, timeout bool) bool {
	if log == nil {
		return false
	}
	for _, event := range log.Events {
		if ibcCommittedEvent(event) && ((timeout && event.Type == eventTimeout) || (!timeout && event.Type == eventFungibleTokenPacket)) {
			return true
		}
	}
	return false
}

func ibcRefundCandidates(denom string) []string {
	if !strings.Contains(denom, "/") {
		return []string{denom}
	}
	return ibcLocalCandidates(denom)
}

// Select against bank evidence rather than parsing channel IDs using the old
// SDK: v2 client IDs can also appear in a trace transported through v1.
func ibcLocalCandidates(path string) []string {
	hash := sha256.Sum256([]byte(path))
	return []string{path, "ibc/" + strings.ToUpper(hex.EncodeToString(hash[:]))}
}

func ibcRefundRow(receiver string, coin sdk.Coin, timeout bool, detail map[string]string) []TaxableEvent {
	trigger := "ibc_error_ack_refund"
	if timeout {
		trigger = "ibc_timeout_refund"
	}
	return []TaxableEvent{{Category: string(CategoryProtocolWithdrawal), ToAddr: receiver, Amount: coin.Amount.String(), Denom: coin.Denom, RewardTrigger: trigger, Detail: detailJSON(detail)}}
}

func ibcV2RefundEvents(log *txlog.LogMessage, packet PacketV2, timeout bool) ([]TaxableEvent, []string) {
	var out []TaxableEvent
	var issues []string
	if log == nil {
		return nil, nil
	}
	credits := ibcCredits(log)
	for _, ev := range log.Events {
		if !ibcCommittedEvent(ev) {
			continue
		}
		a := ibcAttrs(ev)
		var receiver, amount, path string
		switch {
		case timeout && ibcEventType(ev) == eventTimeout:
			// v10 timeout changed from refund_receiver/denom/amount to
			// receiver + refund_tokens (a JSON Token with a structured Denom).
			var token struct {
				Amount string `json:"amount"`
				Denom  struct {
					Base  string `json:"base"`
					Trace []struct {
						PortID    string `json:"port_id"`
						ChannelID string `json:"channel_id"`
					} `json:"trace"`
				} `json:"denom"`
			}
			if json.Unmarshal([]byte(a[attributeRefundTokens]), &token) != nil {
				issues = append(issues, "committed refund application has malformed refund token")
				continue
			}
			receiver, amount, path = a[attributeReceiver], token.Amount, ""
			for _, hop := range token.Denom.Trace {
				path += hop.PortID + "/" + hop.ChannelID + "/"
			}
			path += token.Denom.Base
		case !timeout && ibcEventType(ev) == eventFungibleTokenPacket:
			// The full data event precedes a separate error-only event. The
			// message acknowledgement has already proved the packet failure.
			receiver, amount, path = a[attributeSender], a[attributeAmount], a[attributeDenom]
			if receiver == "" && amount == "" && path == "" {
				continue
			}
		default:
			continue
		}
		if path == "" || receiver == "" || amount == "" {
			issues = append(issues, "committed refund application has incomplete token or owner")
			continue
		}
		// Bank evidence chooses native denomination vs hashed trace. An absent
		// matching credit is unresolved evidence, never a certified empty refund.
		coin, ok := takeIBCCredit(credits, receiver, amount, ibcRefundCandidates(path)...)
		if !ok {
			issues = append(issues, "committed refund application has no reconciled committed bank credit")
			continue
		}
		detail := map[string]string{
			attributeIBCPacketSequence: strconv.FormatUint(packet.Sequence, 10), "packet_source_client": packet.SourceClient,
			"packet_destination_client": packet.DestinationClient, attributeIBCGeneration: ibcGenerationV2, "principal_return": attributeTrue,
		}
		out = append(out, ibcRefundRow(receiver, coin, timeout, detail)...)
	}
	if ibcRefundApplication(log, timeout) && len(out) == 0 && len(issues) == 0 {
		issues = append(issues, "committed refund application has no attributable refund token")
	}
	return out, issues
}
