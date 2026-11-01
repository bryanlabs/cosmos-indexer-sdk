package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
	"github.com/stretchr/testify/require"
)

const (
	testOtherWallet               = "other-wallet"
	apiJournalPath                = "/journal"
	apiPositionsPath              = "/positions"
	testInsertSendPacketEventType = "INSERT INTO pg_temp.message_event_types VALUES (1,'send_packet')"
	testSnapshotPacketKind        = "packet"
)

func TestFutureIncompleteRefundBlocksHistoricalReportPostgres(t *testing.T) {
	for _, failure := range []string{"parser", "opaque"} {
		t.Run(failure, func(t *testing.T) {
			s := refundCachePGFixture(t)
			start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(1, 0, 0)
			dec := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
			insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", dec.Add(-24*time.Hour))
			insertNativeFinancialEvent(t, s, 2, "ibc_out", apiWallet, "destination", "", basisPacket, dec)
			require.Equal(t, 2, nativePreview(t, s, apiWallet, start, end).RowCount)
			msg := &chantypes.MsgTimeout{Packet: chantypes.Packet{Sequence: 23, SourcePort: "transfer", SourceChannel: "channel-7", Data: []byte(`{"denom":"uatom","amount":"100000000","sender":"wallet","receiver":"remote"}`)}}
			wire, err := msg.Marshal()
			require.NoError(t, err)
			require.NoError(t, s.db.Exec("INSERT INTO pg_temp.message_types VALUES (1,?)", sdk.MsgTypeURL(msg)).Error)
			require.NoError(t, s.db.Exec("UPDATE pg_temp.messages SET message_type_id=1,message_bytes=? WHERE id=3", wire).Error)
			for _, sql := range []string{
				"INSERT INTO pg_temp.message_event_types VALUES (1,'timeout')",
				"INSERT INTO pg_temp.message_events (id,message_id,message_event_type_id) VALUES (1,3,1)",
				"INSERT INTO pg_temp.message_event_attribute_keys VALUES (1,'refund_receiver')",
				"INSERT INTO pg_temp.message_event_attributes VALUES (1,1,0,'wallet')",
			} {
				require.NoError(t, s.db.Exec(sql).Error)
			}
			if failure == "parser" {
				require.NoError(t, s.db.Exec("INSERT INTO pg_temp.message_parser_errors VALUES (3)").Error)
			} else {
				require.NoError(t, s.db.Exec("UPDATE pg_temp.messages SET opaque=true WHERE id=3").Error)
			}
			var blocked reportReadinessRequired
			require.ErrorAs(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), &blocked)
			require.Contains(t, blocked.Readiness.ReviewRequired, ibcRefundBasisRequired)
			// No financial refund row exists, so only retained unresolved source
			// evidence can prevent a cached December phantom disposal.
			for _, path := range []string{"/8949", apiPositionsPath, apiJournalPath, "/events", "/report-job"} {
				rr := httptest.NewRecorder()
				s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path+"?address=wallet&chain=mainnet&start=2025-01-01&end=2026-01-01", nil))
				require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
				require.Contains(t, rr.Body.String(), `"status":"blocked"`)
				var body map[string]any
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
				require.Nil(t, body[apiCSV])
			}
			msg.Packet.Sequence = 24
			wire, err = msg.Marshal()
			require.NoError(t, err)
			require.NoError(t, s.db.Exec("UPDATE pg_temp.messages SET message_bytes=? WHERE id=3", wire).Error)
			require.NoError(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), "a provably different future packet must not block this send")
			require.NoError(t, s.requireIBCBasisReady(apiTestnet, []string{apiWallet}, end.AddDate(1, 0, 0)))
			require.NoError(t, s.requireIBCBasisReady(apiMainnet, []string{testOtherWallet}, end.AddDate(1, 0, 0)))
		})
	}
}

func TestMalformedFutureRefundIdentityBlocksKnownSendPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertNativeFinancialEvent(t, s, 2, "ibc_out", apiWallet, "destination", "", basisPacket, end.Add(-time.Hour))
	for _, detail := range []string{"", "malformed", `{"ibc_generation":"v1"}`} {
		insertNativeFinancialEvent(t, s, 3, "protocol_withdrawal", "", apiWallet, ibcTimeoutRefund, detail, end.Add(24*time.Hour))
		var blocked reportReadinessRequired
		require.ErrorAs(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), &blocked)
		require.Contains(t, blocked.Readiness.ReviewRequired, ibcPacketIdentityRequired)
		require.NoError(t, s.db.Exec("DELETE FROM pg_temp.taxable_events WHERE id=3").Error)
	}
}
