package tax

import (
	"encoding/json"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

// RefundDependency is retained lifecycle evidence, not a financial acquisition.
// An empty Detail means the packet cannot be proved unrelated to an earlier send.
type RefundDependency struct {
	Owner  string
	Detail string
}

// RefundLifecycleTypes includes envelopes that can contain a refund obligation.
func RefundLifecycleTypes() []string {
	var urls []string
	for _, msg := range []sdk.Msg{&chantypes.MsgTimeout{}, &chantypes.MsgTimeoutOnClose{}, &chantypes.MsgAcknowledgement{}, &MsgTimeoutV2{}, &MsgAcknowledgementV2{}, &authz.MsgExec{}} {
		urls = append(urls, sdk.MsgTypeURL(msg))
	}
	return urls
}

// UnresolvedRefundDependencies recovers exact owner/packet evidence from a
// message already known to be incomplete. It neither invents a refund from a
// bare timeout nor considers a successful acknowledgement a refund.
func UnresolvedRefundDependencies(typeURL string, wire []byte, log *txlog.LogMessage) []RefundDependency {
	msg, err := unpackAuthzInner(&codectypes.Any{TypeUrl: typeURL, Value: wire})
	if err != nil {
		return refundEventDependencies(log, "", nil)
	}
	count := 0
	return unresolvedRefundDependencies(msg, log, 0, &count)
}

func unresolvedRefundDependencies(msg sdk.Msg, log *txlog.LogMessage, depth int, count *int) []RefundDependency {
	if depth >= maxAuthzDepth || *count >= maxAuthzMessages {
		return refundEventDependencies(log, "", nil)
	}
	*count++
	var packet *chantypes.Packet
	var v2 *PacketV2
	timeout := true
	switch m := msg.(type) {
	case *authz.MsgExec:
		var out []RefundDependency
		for i, a := range m.Msgs {
			if *count >= maxAuthzMessages {
				out = append(out, refundEventDependencies(log, "", nil)...)
				break
			}
			sub := eventsForAuthzIndex(log, i)
			inner, err := unpackAuthzInner(a)
			if err != nil {
				out = append(out, refundEventDependencies(sub, "", nil)...)
				continue
			}
			out = append(out, unresolvedRefundDependencies(inner, sub, depth+1, count)...)
		}
		return out
	case *chantypes.MsgTimeout:
		packet = &m.Packet
	case *chantypes.MsgTimeoutOnClose:
		packet = &m.Packet
	case *chantypes.MsgAcknowledgement:
		if !isErrorAcknowledgement(m.Acknowledgement) {
			return nil
		}
		packet, timeout = &m.Packet, false
	case *MsgTimeoutV2:
		v2 = &m.Packet
	case *MsgAcknowledgementV2:
		if !isV2ErrorAcknowledgement(m.AcknowledgementRaw) {
			return nil
		}
		v2, timeout = &m.Packet, false
	default:
		return nil
	}
	if !ibcRefundApplication(log, timeout) {
		return nil
	}
	var owner string
	var detail map[string]string
	if packet != nil {
		if packet.SourcePort != string(CategoryTransfer) {
			return nil
		}
		var data struct {
			Sender string `json:"sender"`
		}
		if json.Unmarshal(packet.Data, &data) == nil {
			owner = data.Sender
		}
		detail = map[string]string{attributeIBCGeneration: ibcGenerationV1, attributeIBCPacketSequence: strconv.FormatUint(packet.Sequence, 10), attributeIBCSrcPort: packet.SourcePort, attributeIBCSrcChannel: packet.SourceChannel}
	} else if v2 != nil {
		detail = map[string]string{attributeIBCGeneration: ibcGenerationV2, attributeIBCPacketSequence: strconv.FormatUint(v2.Sequence, 10), attributePacketSourceClient: v2.SourceClient, attributePacketDestinationClient: v2.DestinationClient}
	}
	return refundEventDependencies(log, owner, detail)
}

func refundEventDependencies(log *txlog.LogMessage, owner string, detail map[string]string) []RefundDependency {
	if log == nil {
		return nil
	}
	owners := map[string]bool{}
	for _, event := range log.Events {
		if !ibcCommittedEvent(event) {
			continue
		}
		a := ibcAttrs(event)
		switch event.Type {
		case eventTimeout:
			if a[attributeRefundReceiver] != "" {
				owners[a[attributeRefundReceiver]] = true
			} else if a[attributeReceiver] != "" {
				owners[a[attributeReceiver]] = true
			}
		case eventFungibleTokenPacket:
			if a[attributeSender] != "" {
				owners[a[attributeSender]] = true
			}
		}
	}
	if owner != "" {
		owners = map[string]bool{owner: true}
	}
	if len(owners) == 0 {
		return nil
	}
	var out []RefundDependency
	for address := range owners {
		out = append(out, RefundDependency{Owner: address, Detail: detailJSON(detail)})
	}
	return out
}
