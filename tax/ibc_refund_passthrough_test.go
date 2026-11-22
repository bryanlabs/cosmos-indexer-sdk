package tax

import (
	"encoding/json"
	"os"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
)

// refundFixture loads a real cosmoshub-4 refund message (stored body and full
// event log) from testdata/ibc-refund-passthrough, with addresses replaced by
// same-length placeholders consistently across body, events and packet data.
func refundFixture(t *testing.T, name string) (sdk.Msg, *txlog.LogMessage) {
	t.Helper()
	data, err := os.ReadFile("testdata/ibc-refund-passthrough/" + name + ".json")
	require.NoError(t, err)
	var fixture struct {
		MessageType  string                  `json:"message_type"`
		Code         int                     `json:"code"`
		MessageBytes []byte                  `json:"message_bytes"`
		Events       []txlog.LogMessageEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Zero(t, fixture.Code)
	msg, err := unpackAuthzInner(&codectypes.Any{TypeUrl: fixture.MessageType, Value: fixture.MessageBytes})
	require.NoError(t, err)
	return msg, &txlog.LogMessage{Events: fixture.Events}
}

func parseRefundFixture(t *testing.T, name string) []TaxableEvent {
	t.Helper()
	msg, log := refundFixture(t, name)
	out, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, cfgForTests())
	require.NoError(t, err)
	return (*out).([]TaxableEvent)
}

// Packet-forward middleware retried a timed-out forward: the refund landed on
// the forward's intermediate account and left it again in a new send_packet
// in the same message, with an ibc_transfer carrying the amount (mid 2024
// events) or, in a coalesced early-2024 log, without one.
func TestIBCRefundResentByForwardIsPassThrough(t *testing.T) {
	for _, name := range []string{"pfm-retry-resend", "pfm-retry-resend-coalesced"} {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, parseRefundFixture(t, name), "the intermediate never holds the refund")
		})
	}
}

// An IBC v2 source-callback contract received the timeout refund and, in its
// successful callback, bank-sent exactly those coins to one account. The
// refund is booked as that transfer, not as the contract's principal return.
func TestIBCRefundPaidOnByCallbackIsTransfer(t *testing.T) {
	rows := parseRefundFixture(t, "v2-source-callback-onward")
	require.Len(t, rows, 1)
	r := rows[0]
	require.Equal(t, string(CategoryTransfer), r.Category)
	require.Equal(t, "9911", r.Amount)
	require.Equal(t, "uatom", r.Denom)
	require.NotEqual(t, r.FromAddr, r.ToAddr)
	require.Empty(t, r.RewardTrigger)
	var detail map[string]string
	require.NoError(t, json.Unmarshal([]byte(r.Detail), &detail))
	require.Equal(t, "true", detail[detailRefundPassedThrough])
	require.Equal(t, "v2", detail[attributeIBCGeneration])
	require.NotEmpty(t, detail[attributeIBCPacketSequence])
	require.Empty(t, detail[detailPrincipalReturn])
}

// A refund the sender keeps is unchanged: one principal-return row.
func TestIBCRefundKeptStillBooked(t *testing.T) {
	rows := parseRefundFixture(t, "refund-kept")
	require.Len(t, rows, 1)
	require.Equal(t, string(CategoryProtocolWithdrawal), rows[0].Category)
	require.Equal(t, "ibc_timeout_refund", rows[0].RewardTrigger)
	require.Contains(t, rows[0].Detail, `"principal_return":"true"`)
}

// Each piece of onward evidence is required: removing the re-send, the bank
// debit or the callback restores the refund row.
func TestIBCRefundOnwardNeedsEveryPiece(t *testing.T) {
	drop := func(log *txlog.LogMessage, types ...string) *txlog.LogMessage {
		out := &txlog.LogMessage{}
		for _, ev := range log.Events {
			keep := true
			for _, ty := range types {
				if ev.Type == ty {
					keep = false
				}
			}
			if keep {
				out.Events = append(out.Events, ev)
			}
		}
		return out
	}
	parse := func(msg sdk.Msg, log *txlog.LogMessage) []TaxableEvent {
		out, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, cfgForTests())
		require.NoError(t, err)
		return (*out).([]TaxableEvent)
	}
	msg, log := refundFixture(t, "pfm-retry-resend")
	rows := parse(msg, drop(log, eventSendPacket))
	require.Len(t, rows, 1, "no re-send: the refund stays with the account")
	require.Equal(t, "ibc_timeout_refund", rows[0].RewardTrigger)

	msg, log = refundFixture(t, "v2-source-callback-onward")
	rows = parse(msg, drop(log, eventIBCSourceCallback))
	require.Len(t, rows, 1, "no callback: the bank send is not tied to the refund")
	require.Equal(t, "ibc_timeout_refund", rows[0].RewardTrigger)

	// Without any debit of the refunded coin from the receiver, a send_packet
	// naming it cannot have carried the refund.
	msg, log = refundFixture(t, "pfm-retry-resend")
	var receiver string
	for _, ev := range log.Events {
		if ev.Type == eventTimeout {
			a := ibcAttrs(ev)
			receiver = a[attributeReceiver] + a[attributeRefundReceiver]
		}
	}
	require.NotEmpty(t, receiver)
	stripped := &txlog.LogMessage{}
	for _, ev := range log.Events {
		if ev.Type == eventCoinSpent || ev.Type == string(CategoryTransfer) {
			keep := true
			for _, a := range ev.Attributes {
				if (a.Key == attributeSpender || a.Key == attributeSender) && a.Value == receiver {
					keep = false
				}
			}
			if !keep {
				continue
			}
		}
		stripped.Events = append(stripped.Events, ev)
	}
	rows = parse(msg, stripped)
	require.Len(t, rows, 1)
	require.Equal(t, "ibc_timeout_refund", rows[0].RewardTrigger)
}
