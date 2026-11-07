package taxapi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The external pricing phase is deliberately outside the read transaction, so
// another commit can change either classification or legacy packet evidence.
// Neither kind of change may publish the old CSV as an accepted ready cache.
func TestSnapshotComputationRejectsChangedEvidencePostgres(t *testing.T) {
	for _, kind := range []string{"classification", testSnapshotPacketKind} {
		t.Run(kind, func(t *testing.T) {
			s := refundCachePGFixture(t)
			at := time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC)
			insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", at)
			if kind == testSnapshotPacketKind {
				for _, statement := range []string{
					testInsertSendPacketEventType,
					"INSERT INTO pg_temp.message_events (id,message_id,message_event_type_id) VALUES (1,1,1)",
					"INSERT INTO pg_temp.message_event_attribute_keys VALUES (1,'packet_sequence')",
					"INSERT INTO pg_temp.message_event_attributes VALUES (1,1,0,'23')",
				} {
					require.NoError(t, s.db.Exec(statement).Error)
				}
			}
			var changed atomic.Bool
			pricing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if changed.CompareAndSwap(false, true) {
					err := s.db.Transaction(func(writer *gorm.DB) error {
						if kind == testSnapshotPacketKind {
							return writer.Exec("UPDATE pg_temp.message_event_attributes SET value='24'").Error
						}
						if err := writer.Exec("DELETE FROM pg_temp.taxable_events WHERE id=1").Error; err != nil {
							return err
						}
						return writer.Exec("INSERT INTO pg_temp.message_parser_errors VALUES (1)").Error
					})
					if err != nil {
						t.Error(err)
					}
				}
				_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
			}))
			defer pricing.Close()
			s.oracle.base = pricing.URL
			delete(s.oracle.prices, apiMainnet+"|"+apiUatom+"|2025-12-30")
			job := ReportJob{Key: "test:snapshot", Status: ReportJobPending}
			require.NoError(t, s.db.Create(&job).Error)
			s.computeReportJob(job.Key, apiMainnet, []string{apiWallet}, time.Time{}, at.Add(time.Hour), apiSumm, nil, nil)
			require.True(t, changed.Load())
			require.NoError(t, s.db.First(&job, job.ID).Error)
			require.Equal(t, ReportJobFailed, job.Status, job.Error)
			require.Empty(t, job.CSV)
			if kind == testSnapshotPacketKind {
				require.Contains(t, job.Error, "financial history changed")
			} else {
				require.Contains(t, job.Error, "report not ready")
			}
		})
	}
}
