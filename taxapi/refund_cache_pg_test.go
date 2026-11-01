package taxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Native Postgres, one connection and exclusively connection-local TEMP tables.
// No migrations, public-schema writes, production RPC or archive access.
func refundCachePGFixture(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres refund/cache regressions")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	db = db.WithContext(ctx)
	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE chains (id bigint, chain_id text)",
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, time_stamp timestamptz)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint, hash text, code bigint, memo text)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint, opaque boolean)",
		"CREATE TEMP TABLE message_parser_errors (message_id bigint)",
		"CREATE TEMP TABLE failed_messages (tx_id bigint)",
		"CREATE TEMP TABLE failed_txes (block_id bigint, hash text)",
		"CREATE TEMP TABLE addresses (id bigint, address text)",
		"CREATE TEMP TABLE tx_signer_addresses (tx_id bigint, address_id bigint)",
		"CREATE TEMP TABLE denoms (id bigint, base text)",
		"CREATE TEMP TABLE fees (id bigint, tx_id bigint, payer_address_id bigint, denomination_id bigint, amount numeric(78,0))",
		"CREATE TEMP TABLE taxable_events (id bigint, message_id bigint, sub_index bigint, category text, amount text, denom text, asset text, from_addr text, to_addr text, validator_address text, reward_trigger text, detail text, block_height bigint, timestamp timestamptz, tx_hash text)",
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint)",
		"CREATE TEMP TABLE message_event_types (id bigint, type text)",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, message_event_attribute_key_id bigint, index bigint, value text)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE report_jobs (id bigserial PRIMARY KEY, key text UNIQUE, chain text, addresses text, format text, status text, csv text, row_count bigint, readiness_json text, review_fingerprint text, computed_through timestamptz, error text, created_at timestamptz, updated_at timestamptz)",
		"INSERT INTO pg_temp.chains VALUES (1,'cosmoshub-4'),(2,'provider-1')",
		"INSERT INTO pg_temp.blocks VALUES (1,1,'2025-12-30T12:00:00Z'),(2,1,'2025-12-31T12:00:00Z'),(3,1,'2026-01-02T12:00:00Z'),(4,1,'2026-01-03T12:00:00Z')",
		"INSERT INTO pg_temp.txes VALUES (1,1,'acquisition',0,''),(2,2,'send',0,''),(3,3,'refund',0,''),(4,4,'spend',0,'')",
		"INSERT INTO pg_temp.messages VALUES (1,1,false),(2,2,false),(3,3,false),(4,4,false)",
		"INSERT INTO pg_temp.addresses VALUES (1,'wallet'),(2,'other-wallet')",
		"INSERT INTO pg_temp.tx_signer_addresses VALUES (1,1),(2,1),(3,1),(4,1)",
		"INSERT INTO pg_temp.denoms VALUES (1,'uatom')",
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	o := NewOracle("http://127.0.0.1:1", "")
	o.denoms[apiMainnet] = map[string]DenomMeta{apiUatom: {Symbol: apiSymbolATOM, Decimals: 6}}
	o.at[apiMainnet] = nowUTC()
	for _, day := range []string{"2025-12-30", "2025-12-31", "2026-01-02", "2026-01-03"} {
		o.prices[apiMainnet+"|"+apiUatom+"|"+day] = priceCacheEntry{usd: 2, found: true, cached: nowUTC()}
	}
	o.prices[apiMainnet+"|"+apiUatom+"|2025-12-30"] = priceCacheEntry{usd: 1, found: true, cached: nowUTC()}
	return NewServer(db, o, DefaultNativeAsset)
}

func insertNativeFinancialEvent(t *testing.T, s *Server, id uint, category, from, to, trigger, detail string, at time.Time) {
	t.Helper()
	e := tax.TaxableEvent{ID: id, MessageID: id, Category: category, FromAddr: from, ToAddr: to, Amount: "100000000", Denom: apiUatom, RewardTrigger: trigger, Detail: detail, Timestamp: at, TxHash: financialEventKind}
	require.NoError(t, s.db.Omit("Message").Create(&e).Error)
}

func nativePreview(t *testing.T, s *Server, wallet string, start, end time.Time) *ReportJob {
	t.Helper()
	var job *ReportJob
	require.Eventually(t, func() bool {
		var err error
		job, err = s.getOrStartReportJob(apiMainnet, []string{wallet}, start, end, start.Format(apiDateLayout), end.Format(apiDateLayout), apiSumm, false, nil, nil)
		require.NoError(t, err)
		if job.Status == ReportJobFailed {
			t.Fatalf("computation failed: %s", job.Error)
		}
		return job.Status == ReportJobReady
	}, 5*time.Second, 10*time.Millisecond)
	return job
}

func TestHistoricalNativeCacheCorrectionsPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC))
	base := nativePreview(t, s, apiWallet, start, end)
	require.Equal(t, 1, base.RowCount)
	// Successful native historical edit, still no parser/semantic errors and
	// timestamp long before ComputedThrough. Cached CSV must be recomputed.
	require.NoError(t, s.db.Exec("UPDATE pg_temp.taxable_events SET amount='200000000' WHERE id=1").Error)
	edited := nativePreview(t, s, apiWallet, start, end)
	require.NotEqual(t, base.ReviewFingerprint, edited.ReviewFingerprint)
	require.NotEqual(t, base.CSV, edited.CSV)
	require.NoError(t, s.db.Exec("DELETE FROM pg_temp.taxable_events WHERE id=1").Error)
	deleted := nativePreview(t, s, apiWallet, start, end)
	require.NotEqual(t, edited.ReviewFingerprint, deleted.ReviewFingerprint)
	require.Zero(t, deleted.RowCount)
	require.NotEqual(t, edited.CSV, deleted.CSV)
	// Ordinary financial inserts also invalidate a ready cache.
	insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC))
	require.Equal(t, 1, nativePreview(t, s, apiWallet, start, end).RowCount)
	// Exact persisted fee ownership participates, even if its amount is equal.
	require.NoError(t, s.db.Exec("INSERT INTO pg_temp.fees VALUES (1,1,1,1,1000000)").Error)
	payerA := nativePreview(t, s, apiWallet, start, end)
	payerB := nativePreview(t, s, "other-wallet", start, end)
	require.Equal(t, 2, payerA.RowCount)
	require.Zero(t, payerB.RowCount)
	require.NoError(t, s.db.Exec("UPDATE pg_temp.fees SET payer_address_id=2 WHERE id=1").Error)
	correctA := nativePreview(t, s, apiWallet, start, end)
	correctB := nativePreview(t, s, "other-wallet", start, end)
	require.Equal(t, 1, correctA.RowCount)
	require.Equal(t, 1, correctB.RowCount)
	require.NotEqual(t, payerA.ReviewFingerprint, correctA.ReviewFingerprint)
	require.NotEqual(t, payerB.ReviewFingerprint, correctB.ReviewFingerprint)
	require.NotEqual(t, payerA.CSV, correctA.CSV)
	require.NotEqual(t, payerB.CSV, correctB.CSV)
	require.NoError(t, s.db.Exec("UPDATE pg_temp.fees SET amount=2000000 WHERE id=1").Error)
	amountEdit := nativePreview(t, s, "other-wallet", start, end)
	require.NotEqual(t, correctB.CSV, amountEdit.CSV)
	require.NoError(t, s.db.Exec("DELETE FROM pg_temp.fees WHERE id=1").Error)
	require.Zero(t, nativePreview(t, s, "other-wallet", start, end).RowCount)
}

func TestNativeRefundBlocksHistoricalCachedDisposalAndLaterSpendPostgres(t *testing.T) {
	for _, trigger := range []string{ibcTimeoutRefund, ibcErrorAckRefund} {
		t.Run(trigger, func(t *testing.T) {
			s := refundCachePGFixture(t)
			dec := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
			jan := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
			start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(1, 0, 0)
			insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", dec.Add(-24*time.Hour))
			insertNativeFinancialEvent(t, s, 2, "ibc_out", apiWallet, "destination", "", basisPacket, dec)
			// Cache is ready before learning of the next-year refund.
			require.Equal(t, 2, nativePreview(t, s, apiWallet, start, end).RowCount)
			insertNativeFinancialEvent(t, s, 3, "protocol_withdrawal", "", apiWallet, trigger, basisPacket, jan)
			insertNativeFinancialEvent(t, s, 4, apiTransfer, apiWallet, "merchant", "", "", jan.Add(24*time.Hour))
			for _, path := range []string{"/8949", "/positions", "/journal", "/events", "/report-job"} {
				for _, period := range []string{"start=2025-01-01&end=2026-01-01", "start=2026-01-03&end=2027-01-01"} {
					rr := httptest.NewRecorder()
					s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path+"?address=wallet&chain=mainnet&"+period, nil))
					require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
					require.Contains(t, rr.Body.String(), ibcRefundBasisRequired)
					var body map[string]any
					require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
					require.Nil(t, body[apiCSV], "no phantom gain, unknown replacement basis or missing-position CSV may leak")
				}
			}
			// A different packet must not retroactively block the original send.
			require.NoError(t, s.db.Exec("UPDATE pg_temp.taxable_events SET detail=? WHERE id=3", strings.Replace(basisPacket, `"23"`, `"24"`, 1)).Error)
			require.NoError(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end))
			require.NoError(t, s.requireIBCBasisReady(apiMainnet, []string{"other-wallet"}, end.AddDate(1, 0, 0)))
			require.NoError(t, s.requireIBCBasisReady(apiTestnet, []string{apiWallet}, end.AddDate(1, 0, 0)))
		})
	}
}

func TestLegacyOutboundPacketEvidencePostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	dec := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertNativeFinancialEvent(t, s, 2, "ibc_out", apiWallet, "destination", "", "", dec)
	insertNativeFinancialEvent(t, s, 3, "protocol_withdrawal", "", apiWallet, ibcTimeoutRefund, basisPacket, end.AddDate(0, 0, 1))
	for _, sql := range []string{
		"INSERT INTO pg_temp.message_event_types VALUES (1,'send_packet')",
		"INSERT INTO pg_temp.message_events VALUES (1,2,1)",
		"INSERT INTO pg_temp.message_event_attribute_keys VALUES (1,'packet_sequence'),(2,'packet_src_port'),(3,'packet_src_channel')",
		"INSERT INTO pg_temp.message_event_attributes VALUES (1,1,0,'23'),(1,2,1,'transfer'),(1,3,2,'channel-7')",
	} {
		require.NoError(t, s.db.Exec(sql).Error)
	}
	var blocked reportReadinessRequired
	require.ErrorAs(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), &blocked)
	require.Contains(t, blocked.Readiness.ReviewRequired, ibcRefundBasisRequired)
	require.NotContains(t, blocked.Readiness.ReviewRequired, ibcPacketIdentityRequired)
	require.NoError(t, s.db.Exec("UPDATE pg_temp.message_event_attributes SET value='24' WHERE message_event_attribute_key_id=1").Error)
	require.NoError(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), "different retained packet is not guessed to match")
	require.NoError(t, s.db.Exec("DELETE FROM pg_temp.message_event_attributes").Error)
	require.ErrorAs(t, s.requireIBCBasisReady(apiMainnet, []string{apiWallet}, end), &blocked)
	require.Contains(t, blocked.Readiness.ReviewRequired, ibcPacketIdentityRequired)
}

func TestFullHistoryFinancialFingerprintScopePostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	fingerprint := func(wallet string) string {
		value, err := s.currentReviewFingerprint(apiMainnet, []string{wallet}, start, end)
		require.NoError(t, err)
		return value
	}
	base := fingerprint(apiWallet)
	other := fingerprint("other-wallet")
	// Out-of-period native evidence participates without pricing or rounding.
	insertNativeFinancialEvent(t, s, 3, apiReward, "", apiWallet, "claim", "", end.Add(24*time.Hour))
	before := fingerprint(apiWallet)
	require.NotEqual(t, base, before)
	require.Equal(t, other, fingerprint("other-wallet"))
	for _, sql := range []string{
		"UPDATE pg_temp.txes SET memo='corrected historical memo' WHERE id=3",
		"UPDATE pg_temp.taxable_events SET detail='{\"corrected\":\"true\"}' WHERE id=3",
		"UPDATE pg_temp.taxable_events SET category='income',reward_trigger='corrected' WHERE id=3",
		"UPDATE pg_temp.taxable_events SET amount='999999999999999999999999999999999999999999999999999999999999999' WHERE id=3",
		"DELETE FROM pg_temp.taxable_events WHERE id=3",
	} {
		require.NoError(t, s.db.Exec(sql).Error)
		after := fingerprint(apiWallet)
		require.NotEqual(t, before, after)
		before = after
	}
	require.Equal(t, base, before)
}
