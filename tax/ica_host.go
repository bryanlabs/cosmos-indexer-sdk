package tax

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/gogoproto/proto"
	icatypes "github.com/cosmos/ibc-go/v7/modules/apps/27-interchain-accounts/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

// icaHostPort is the destination port of every interchain-account packet the
// Hub executes. The relayer's MsgRecvPacket is the only message on record for
// that execution, so the ICA account's own messages are classified from it.
const icaHostPort = "icahost"

const (
	typeURLBankSend      = "/cosmos.bank.v1beta1.MsgSend"
	typeURLBankMultiSend = "/cosmos.bank.v1beta1.MsgMultiSend"
)

// icaHostEvents classifies a successful EXECUTE_TX packet as if the ICA account
// had signed its inner message itself. The stored log merges same-type events
// across messages, so a packet with several inner messages cannot be split per
// message and is reported as unresolved instead, except for bank sends, which
// are classified from the message alone.
func icaHostEvents(packet chantypes.Packet, log *txlog.LogMessage, state *classificationState, depth int) []TaxableEvent {
	executed, known := icaHostExecuted(log)
	if !known {
		state.issues = append(state.issues, "ica host packet: no ics27_packet result or acknowledgement")
		return nil
	}
	if !executed {
		return nil
	}
	data, isTx, err := icaPacketTx(packet.GetData())
	if err != nil {
		state.issues = append(state.issues, fmt.Sprintf("ica host packet: %v", err))
		return nil
	}
	if !isTx {
		return nil
	}
	var tx icatypes.CosmosTx
	if err := proto.Unmarshal(data, &tx); err != nil {
		state.issues = append(state.issues, fmt.Sprintf("ica host packet: decode CosmosTx: %v", err))
		return nil
	}
	var out []TaxableEvent
	for i, a := range tx.Messages {
		path := "ica/" + strconv.Itoa(i)
		inner, err := unpackAuthzInner(a)
		if err != nil {
			state.issues = append(state.issues, fmt.Sprintf("ica %s: %v", path, err))
			return nil
		}
		if len(tx.Messages) > 1 {
			if !messageOnlyClassification(a.TypeUrl) {
				state.issues = append(state.issues, fmt.Sprintf("ica host packet: %d inner messages share one event log (%s; coins moved %t)", len(tx.Messages), icaTypeSummary(tx.Messages), authzMovedCoins(log)))
				return nil
			}
			out = append(out, classifyWithState(inner, &txlog.LogMessage{}, state, depth+1, path)...)
			continue
		}
		supported := false
		for _, url := range MessageTypeURLs {
			if a.TypeUrl == url {
				supported = true
				break
			}
		}
		if !supported && authzNoTaxEffect[a.TypeUrl] && !authzMovedCoins(log) {
			continue
		}
		if !supported {
			state.issues = append(state.issues, fmt.Sprintf("ica %s: unsupported inner %s", path, a.TypeUrl))
			return nil
		}
		out = append(out, classifyWithState(inner, log, state, depth+1, path)...)
	}
	return out
}

// icaTypeSummary names each inner type once with its count, in sorted order,
// so unresolved packets can be grouped by what they contain.
func icaTypeSummary(msgs []*codectypes.Any) string {
	counts := map[string]int{}
	for _, a := range msgs {
		counts[a.GetTypeUrl()]++
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)
	parts := make([]string, len(types))
	for i, t := range types {
		parts[i] = fmt.Sprintf("%s x%d", t, counts[t])
	}
	return strings.Join(parts, ", ")
}

// Bank sends are booked from the message fields alone, so each inner send of
// a batch is classified without the shared log.
func messageOnlyClassification(typeURL string) bool {
	return typeURL == typeURLBankSend || typeURL == typeURLBankMultiSend
}

// icaHostExecuted reports whether the host committed the packet's messages.
// ibc-go emits ics27_packet success from v6 on; older packets carry only the
// acknowledgement, which is an error when execution was reverted.
func icaHostExecuted(log *txlog.LogMessage) (executed, known bool) {
	for _, e := range log.Events {
		if e.Type != "ics27_packet" {
			continue
		}
		switch attrMap(e)[attributeSuccess] {
		case attributeTrue:
			return true, true
		case "false":
			return false, true
		}
	}
	for _, e := range log.Events {
		if e.Type != eventWriteAcknowledgement {
			continue
		}
		a := attrMap(e)
		raw, err := hex.DecodeString(a[attributePacketAckHex])
		if err != nil || len(raw) == 0 {
			raw = []byte(a["packet_ack"])
		}
		var ack struct {
			Error  string          `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(raw, &ack) != nil {
			continue
		}
		if ack.Error != "" {
			return false, true
		}
		if len(ack.Result) != 0 {
			return true, true
		}
	}
	return false, false
}

// icaPacketTx returns the CosmosTx bytes of an EXECUTE_TX packet. Controllers
// encode the packet as protobuf JSON, where the enum is a name or a number and
// the bytes are base64, and CosmWasm controllers send the bytes as a number
// array.
func icaPacketTx(raw []byte) ([]byte, bool, error) {
	var pd struct {
		Type json.RawMessage `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, false, fmt.Errorf("packet data: %w", err)
	}
	var name string
	var number int32
	switch {
	case json.Unmarshal(pd.Type, &name) == nil:
		if name != icatypes.EXECUTE_TX.String() {
			return nil, false, nil
		}
	case json.Unmarshal(pd.Type, &number) == nil:
		if number != int32(icatypes.EXECUTE_TX) {
			return nil, false, nil
		}
	default:
		return nil, false, fmt.Errorf("packet type %s", pd.Type)
	}
	// encoding/json reads a []byte from either form.
	var data []byte
	if err := json.Unmarshal(pd.Data, &data); err != nil {
		return nil, false, fmt.Errorf("packet data bytes: %w", err)
	}
	return data, true, nil
}
