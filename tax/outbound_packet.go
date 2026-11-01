package tax

import (
	"encoding/json"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

// OutboundPacketDetail retains the source-side packet identity, independently of
// memo intent. A v2 transfer uses client IDs, not v1 channel IDs. Ambiguous logs
// do not justify assigning an arbitrary packet to an economic leg.
func OutboundPacketDetail(log *txlog.LogMessage, m *transfertypes.MsgTransfer) map[string]string {
	var found map[string]string
	if log == nil {
		return nil
	}
	for _, ev := range log.Events {
		if ev.Type != "send_packet" {
			continue
		}
		// A coalesced legacy event can contain multiple packets. attrMap
		// alone would silently choose its last packet and attach the wrong key.
		seen := map[string]bool{}
		for _, attr := range ev.Attributes {
			if seen[attr.Key] {
				return nil
			}
			seen[attr.Key] = true
		}
		a := attrMap(ev)
		seq, err := strconv.ParseUint(a[attributeIBCPacketSequence], 10, 64)
		if err != nil || seq == 0 {
			continue
		}
		d := map[string]string{attributeIBCPacketSequence: strconv.FormatUint(seq, 10)}
		if a["packet_source_client"] != "" && a["packet_dest_client"] != "" {
			if m.SourceChannel != "" && m.SourceChannel != a["packet_source_client"] {
				continue
			}
			d["ibc_generation"] = "v2"
			d["packet_source_client"], d["packet_destination_client"] = a["packet_source_client"], a["packet_dest_client"]
		} else {
			if a[attributeIBCSrcPort] == "" || a[attributeIBCSrcChannel] == "" ||
				(m.SourcePort != "" && m.SourcePort != a[attributeIBCSrcPort]) ||
				(m.SourceChannel != "" && m.SourceChannel != a[attributeIBCSrcChannel]) {
				continue
			}
			d["ibc_generation"] = "v1"
			for _, key := range []string{attributeIBCSrcPort, attributeIBCSrcChannel, attributeIBCDstPort, attributeIBCDstChannel} {
				d[key] = a[key]
			}
		}
		if found != nil {
			return nil
		}
		found = d
	}
	return found
}

// IBCPacketKey is source-chain local. Callers must separately scope by chain
// and original sender. It intentionally never falls back to amount or denom.
func IBCPacketKey(detail string) string {
	var d map[string]string
	if json.Unmarshal([]byte(detail), &d) != nil {
		return ""
	}
	seq, err := strconv.ParseUint(d[attributeIBCPacketSequence], 10, 64)
	if err != nil || seq == 0 {
		return ""
	}
	var parts []string
	switch d["ibc_generation"] {
	case "v1":
		if d[attributeIBCSrcPort] == "" || d[attributeIBCSrcChannel] == "" {
			return ""
		}
		parts = []string{"v1", d[attributeIBCSrcPort], d[attributeIBCSrcChannel], strconv.FormatUint(seq, 10)}
	case "v2":
		if d["packet_source_client"] == "" || d["packet_destination_client"] == "" {
			return ""
		}
		parts = []string{"v2", d["packet_source_client"], d["packet_destination_client"], strconv.FormatUint(seq, 10)}
	default:
		return ""
	}
	key, _ := json.Marshal(parts)
	return string(key)
}
