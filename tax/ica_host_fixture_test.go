package tax

import (
	"encoding/json"
	"os"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/stretchr/testify/require"
)

// icaHostFixture rebuilds a real cosmoshub-4 ICA host MsgRecvPacket from its
// stored events, as a bodiless replay does (addresses are same-length
// placeholders, also inside the packet's CosmosTx).
func icaHostFixture(t *testing.T, name string) []TaxableEvent {
	t.Helper()
	raw, err := os.ReadFile("testdata/ica-host/" + name + ".json")
	require.NoError(t, err)
	var fx struct {
		MessageType string                  `json:"message_type"`
		Events      []txlog.LogMessageEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &fx))
	log := &txlog.LogMessage{Events: fx.Events}
	msg, err := ReconstructMsg(fx.MessageType, log)
	require.NoError(t, err)
	out, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, cfgForTests())
	require.NoError(t, err)
	return (*out).([]TaxableEvent)
}

// Interchain accounts sent ICS-20 transfers in multi-message EXECUTE_TX
// packets. Each transfer is booked as ibc_out from the host account with the
// key of the one send_packet that carries exactly it, so the timeout refund of
// that packet can be matched back to the send.
func TestICAHostBatchTransfersCarryTheirPacketKeys(t *testing.T) {
	for name, want := range map[string]int{"seven-transfers": 7, "send-and-transfer": 1} {
		t.Run(name, func(t *testing.T) {
			rows := icaHostFixture(t, name)
			require.Len(t, rows, want+1, "the bank send plus one row per transfer")
			keys := map[string]bool{}
			var host string
			for _, r := range rows {
				if host == "" {
					host = r.FromAddr
				}
				require.Equal(t, host, r.FromAddr, "every row leaves the host account")
				if r.Category != string(CategoryIBCOut) {
					require.Equal(t, string(CategoryTransfer), r.Category)
					continue
				}
				key := IBCPacketKey(r.Detail)
				require.NotEmpty(t, key)
				require.False(t, keys[key], "each transfer has its own packet")
				keys[key] = true
			}
			require.Len(t, keys, want)
		})
	}
}

// Without the exact send_packet the transfer is still booked, without a key;
// two identical candidates are never split between transfers.
func TestICAHostBatchTransferPacketMustBeUnique(t *testing.T) {
	raw, err := os.ReadFile("testdata/ica-host/send-and-transfer.json")
	require.NoError(t, err)
	var fx struct {
		MessageType string                  `json:"message_type"`
		Events      []txlog.LogMessageEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &fx))
	parse := func(events []txlog.LogMessageEvent) []TaxableEvent {
		log := &txlog.LogMessage{Events: events}
		msg, err := ReconstructMsg(fx.MessageType, log)
		require.NoError(t, err)
		out, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, cfgForTests())
		require.NoError(t, err)
		return (*out).([]TaxableEvent)
	}
	var without, doubled []txlog.LogMessageEvent
	for _, ev := range fx.Events {
		if ev.Type == eventSendPacket {
			doubled = append(doubled, ev)
		} else {
			without = append(without, ev)
		}
		doubled = append(doubled, ev)
	}
	for name, events := range map[string][]txlog.LogMessageEvent{"missing": without, "duplicated": doubled} {
		rows := parse(events)
		require.Len(t, rows, 2, name)
		for _, r := range rows {
			if r.Category == string(CategoryIBCOut) {
				require.Empty(t, IBCPacketKey(r.Detail), name)
				require.Equal(t, "107024", r.Amount, name)
			}
		}
	}
}
