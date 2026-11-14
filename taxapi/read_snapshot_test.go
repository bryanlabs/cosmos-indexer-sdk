package taxapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Uses only the existing connection-local TEMP fixture. This test verifies the
// real PostgreSQL transaction properties and that actual HTTP materialization
// uses that transaction. It does NOT pretend another connection sees TEMP data.
func TestAuthoritativeSnapshotPropertiesPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	at := time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC)
	insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", at)
	var financialReads int
	require.NoError(t, s.db.Callback().Query().Before("gorm:query").Register("test:verify_snapshot", func(q *gorm.DB) {
		if q.Statement.Table != apiTaxableEvents {
			return
		}
		_, ok := q.Statement.ConnPool.(*sql.Tx)
		require.True(t, ok, "authoritative financial queries must use the snapshot transaction")
		var isolation, readOnly string
		require.NoError(t, q.Session(&gorm.Session{NewDB: true}).Raw("SHOW transaction_isolation").Scan(&isolation).Error)
		require.NoError(t, q.Session(&gorm.Session{NewDB: true}).Raw("SHOW transaction_read_only").Scan(&readOnly).Error)
		require.Equal(t, "repeatable read", isolation)
		require.Equal(t, "on", readOnly)
		financialReads++
	}))
	t.Cleanup(func() { _ = s.db.Callback().Query().Remove("test:verify_snapshot") })
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?address=wallet&start=2025-01-01&end=2026-01-01", nil))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), apiSymbolATOM)
	require.GreaterOrEqual(t, financialReads, 2, "IBC dependencies and output rows both use the snapshot")
}

func TestSnapshotReleasesConnectionBeforePricingPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	at := time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC)
	insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", at)
	var calls atomic.Int32
	pricing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The fixture has max-open=1. This would time out if any external
		// pricing were attempted while holding the read transaction open.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var one int
		if err := s.db.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error; err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"usd":7,"found":true}`)
	}))
	defer pricing.Close()
	s.oracle.base = pricing.URL
	// Preserve the configured oracle and its shared cache. Only the price is
	// cold, so the request must use this original instance after materializing.
	delete(s.oracle.prices, apiMainnet+"|"+apiUatom+"|2025-12-30")
	rows, err := s.reviewedRows(apiMainnet, []string{apiWallet}, time.Time{}, at.Add(time.Hour), nil, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "7", rows[0].PriceUSD.String())
	before := calls.Load()
	require.Positive(t, before)
	_, err = s.reviewedRows(apiMainnet, []string{apiWallet}, time.Time{}, at.Add(time.Hour), nil, false)
	require.NoError(t, err)
	require.Equal(t, before, calls.Load(), "original oracle cache survives snapshot readers")
}

func TestSnapshotEvidenceIncludesClassificationAndPacketDependenciesPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	fingerprint := func() string {
		v, err := s.currentReviewFingerprint(apiMainnet, []string{apiWallet}, time.Time{}, nowUTC())
		require.NoError(t, err)
		return v
	}
	before := fingerprint()
	for _, statement := range []string{
		"UPDATE pg_temp.messages SET opaque=true WHERE id=3",
		"INSERT INTO pg_temp.message_parser_errors VALUES (3)",
		"INSERT INTO pg_temp.failed_messages VALUES (3)",
		"INSERT INTO pg_temp.failed_txes VALUES (3,'refund')",
		testInsertSendPacketEventType,
		"INSERT INTO pg_temp.message_events (id,message_id,message_event_type_id) VALUES (1,2,1)",
		"INSERT INTO pg_temp.message_event_attribute_keys VALUES (1,'packet_sequence')",
		"INSERT INTO pg_temp.message_event_attributes VALUES (1,1,0,'23')",
		"UPDATE pg_temp.message_event_attributes SET value='24'",
		"DELETE FROM pg_temp.message_event_attributes",
	} {
		require.NoError(t, s.db.Exec(statement).Error)
		after := fingerprint()
		// Unreferenced lookup rows are not evidence until an event uses them.
		if !strings.Contains(statement, "INSERT INTO pg_temp.message_event_types") && !strings.Contains(statement, "INSERT INTO pg_temp.message_event_attribute_keys") {
			require.NotEqual(t, before, after, statement)
		}
		before = after
	}
}

// A genuinely disposable Docker PostgreSQL instance, with real tables visible
// to two independent connections. Never use COSMOS_TAX_TEST_DSN here: those
// native fixtures deliberately use connection-local TEMP tables instead.
func TestDirectExportSnapshotConcurrentReplayDocker(t *testing.T) {
	if os.Getenv("COSMOS_TAX_SNAPSHOT_DOCKER") != "1" {
		t.Skip("set COSMOS_TAX_SNAPSHOT_DOCKER=1 with Docker for disposable two-connection MVCC regression")
	}
	s, db, wallet, _ := assetReviewFixture(t)
	require.NoError(t, db.Exec("INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES (1,1)").Error)
	require.NoError(t, db.Exec("UPDATE taxable_events SET denom='uatom', category='transfer', reward_trigger='' WHERE message_id=1").Error)
	parser := models.MessageParser{Identifier: "test:snapshot_replay"}
	require.NoError(t, db.Create(&parser).Error)
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(2)
	paused, resume := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	var gateReads atomic.Int32
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:observe_readiness", func(q *gorm.DB) {
		if strings.Contains(q.Statement.SQL.String(), "opaque_scope") {
			gateReads.Add(1)
		}
	}))
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:pause_after_readiness", func(q *gorm.DB) {
		if q.Statement.Table != apiTaxableEvents || !strings.Contains(fmt.Sprint(q.Statement.Clauses["WHERE"].Expression), "timestamp >=") || !once.CompareAndSwap(false, true) {
			return
		}
		if gateReads.Load() == 0 {
			_ = q.AddError(fmt.Errorf("financial load reached before readiness"))
			return
		}
		close(paused)
		select {
		case <-resume:
		case <-q.Statement.Context.Done():
			_ = q.AddError(q.Statement.Context.Err())
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove("test:observe_readiness")
		_ = db.Callback().Query().Remove("test:pause_after_readiness")
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?address="+wallet+"&start=2026-01-01&end=2027-01-01", nil))
		done <- rr
	}()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("direct HTTP export did not pause after readiness")
	}
	// This must commit on connection 2 while the request still owns connection
	// 1's repeatable-read snapshot, just like a failed atomic replay replacement.
	err = db.Transaction(func(writer *gorm.DB) error {
		if err := writer.Exec("DELETE FROM taxable_events WHERE message_id=1").Error; err != nil {
			return err
		}
		return writer.Omit("Message", "MessageParser").Create(&models.MessageParserError{MessageID: 1, MessageParserID: parser.ID, Error: "classification incomplete"}).Error
	})
	close(resume)
	require.NoError(t, err)
	select {
	case rr := <-done:
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.Contains(t, rr.Body.String(), "SCAMTX", "coherent pre-replay acquisition must remain, not empty ready output")
	case <-time.After(10 * time.Second):
		t.Fatal("direct export did not resume")
	}
	// A subsequent snapshot must instead expose the structured block.
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?address="+wallet+"&start=2026-01-01&end=2027-01-01", nil))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), `"status":"blocked"`)
}
