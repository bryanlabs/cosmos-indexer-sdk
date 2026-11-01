package tax

import (
	"crypto/sha256"
	"errors"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
	"github.com/stretchr/testify/require"
)

func TestRefundApplicationWithoutCreditIsIncomplete(t *testing.T) {
	packet := safetyPacket(t, fixtureNativeDenom)
	v2 := PacketV2{Sequence: 23, SourceClient: safetyV2Client, DestinationClient: fixtureHubClient}
	sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
	for _, tc := range []struct {
		name string
		msg  sdk.Msg
		log  *txlog.LogMessage
	}{
		{"v1_timeout", &chantypes.MsgTimeout{Packet: packet}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev("timeout", [2]string{testRefundReceiver, other})}}},
		{"v1_ack", &chantypes.MsgAcknowledgement{Packet: packet, Acknowledgement: []byte(`{"error":"failed"}`)}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev(eventFungibleTokenPacket, [2]string{attributeSender, other}, [2]string{attributeDenom, fixtureNativeDenom}, [2]string{attributeAmount, "100"})}}},
		{"v2_timeout", &MsgTimeoutV2{Packet: v2}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev("timeout", [2]string{attributeReceiver, other}, [2]string{testRefundTokens, `{"denom":{"base":"uatom","trace":[]},"amount":"100"}`})}}},
		{"v2_malformed_token", &MsgTimeoutV2{Packet: v2}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev("timeout", [2]string{attributeReceiver, other}, [2]string{testRefundTokens, "malformed"})}}},
		{"v2_ack", &MsgAcknowledgementV2{Packet: v2, AcknowledgementRaw: nested(1, sentinel[:])}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev(eventFungibleTokenPacket, [2]string{attributeSender, other}, [2]string{attributeDenom, fixtureNativeDenom}, [2]string{attributeAmount, "100"})}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := (&Parser{}).ParseMessage(tc.msg, tc.log, cfgForTests())
			var incomplete *ClassificationIncompleteError
			require.True(t, errors.As(err, &incomplete), "explicit committed refund cannot certify an empty classification")
			require.NotEmpty(t, incomplete.Issues)
			require.Empty(t, (*data).([]TaxableEvent))
		})
	}
	for _, log := range []*txlog.LogMessage{{}, {Events: safetyPrefix([]txlog.LogMessageEvent{ev("timeout", [2]string{testRefundReceiver, other})})}} {
		data, err := (&Parser{}).ParseMessage(&chantypes.MsgTimeout{Packet: packet}, log, cfgForTests())
		require.NoError(t, err, "a bare duplicate relay or discarded application is not a refund")
		require.Empty(t, (*data).([]TaxableEvent))
	}
}

func TestUnresolvedRefundRetainsPacketAndOwner(t *testing.T) {
	packet := safetyPacket(t, fixtureNativeDenom)
	msg := &chantypes.MsgTimeout{Packet: packet}
	wire, err := msg.Marshal()
	require.NoError(t, err)
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev("timeout", [2]string{testRefundReceiver, other})}}
	dependencies := UnresolvedRefundDependencies(sdk.MsgTypeURL(msg), wire, log)
	require.Len(t, dependencies, 1)
	require.Equal(t, other, dependencies[0].Owner)
	require.NotEmpty(t, IBCPacketKey(dependencies[0].Detail))
	require.Empty(t, UnresolvedRefundDependencies(sdk.MsgTypeURL(msg), wire, &txlog.LogMessage{}))
	require.Empty(t, UnresolvedRefundDependencies(sdk.MsgTypeURL(msg), wire, &txlog.LogMessage{Events: safetyPrefix(log.Events)}))
	packet.Data = []byte("malformed")
	msg.Packet = packet
	wire, err = msg.Marshal()
	require.NoError(t, err)
	require.Len(t, UnresolvedRefundDependencies(sdk.MsgTypeURL(msg), wire, log), 1)
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	var incomplete *ClassificationIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.Empty(t, (*data).([]TaxableEvent))
}
