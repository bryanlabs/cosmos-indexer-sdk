package tax

import (
	"encoding/json"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	"github.com/stretchr/testify/require"
)

const outboundFixtureChannel = "channel-7"

func TestOutboundPacketIdentityPreservesMemoAndGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, channel string
		attrs         [][2]string
	}{
		{"v1", outboundFixtureChannel, [][2]string{{attributeIBCPacketSequence, "23"}, {attributeIBCSrcPort, string(CategoryTransfer)}, {attributeIBCSrcChannel, outboundFixtureChannel}, {attributeIBCDstPort, string(CategoryTransfer)}, {attributeIBCDstChannel, "channel-141"}}},
		{"v2", "08-wasm-1", [][2]string{{attributeIBCPacketSequence, "23"}, {"packet_source_client", "08-wasm-1"}, {"packet_destination_client", "08-wasm-2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &transfertypes.MsgTransfer{SourcePort: string(CategoryTransfer), SourceChannel: tc.channel, Sender: del, Receiver: other, Token: sdk.NewInt64Coin("uatom", 100), Memo: `{"forward":{"receiver":"next","port":"transfer","channel":"channel-2"}}`}
			log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev("send_packet", tc.attrs...)}}
			out := classify(m, log)
			require.Len(t, out, 1)
			var detail map[string]string
			require.NoError(t, json.Unmarshal([]byte(out[0].Detail), &detail))
			require.Equal(t, tc.name, detail["ibc_generation"])
			require.Equal(t, "23", detail[attributeIBCPacketSequence])
			require.NotEmpty(t, IBCPacketKey(out[0].Detail))
			trigger, memo := ibcMemoIntent(m.Memo)
			require.Equal(t, trigger, out[0].RewardTrigger)
			for key, value := range memo {
				require.Equal(t, value, detail[key])
			}
			original := log.Events[0]
			log.Events = append(log.Events, original)
			require.Empty(t, OutboundPacketDetail(log, m), "ambiguous send events must not be assigned arbitrarily")
			original.Attributes = append(original.Attributes, txlog.Attribute{Key: attributeIBCPacketSequence, Value: "24"})
			log.Events = []txlog.LogMessageEvent{original}
			require.Empty(t, OutboundPacketDetail(log, m), "coalesced packet attributes must not silently choose the last packet")
		})
	}
}

func TestPacketKeyRequiresCompleteSourceIdentity(t *testing.T) {
	for _, detail := range []string{"", "broken", `{"ibc_generation":"v1","packet_sequence":"23"}`, `{"ibc_generation":"v2","packet_sequence":"23","packet_source_client":"a"}`, `{"ibc_generation":"v1","packet_sequence":"0","packet_src_port":"transfer","packet_src_channel":"channel-1"}`} {
		require.Empty(t, IBCPacketKey(detail))
	}
}
