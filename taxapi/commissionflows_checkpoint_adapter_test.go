package taxapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func checkpointMockServer(t *testing.T) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	raw, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: raw}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	require.NoError(t, err)
	return &Server{db: db}, m
}
func TestFlowCheckpointEpochExport(t *testing.T) {
	s, m := checkpointMockServer(t)
	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(commissionCheckpointDatabaseSQL)).WillReturnRows(sqlmock.NewRows([]string{"identity"}).AddRow("synthetic-db-identity"))
	m.ExpectQuery(regexp.QuoteMeta("SELECT pg_export_snapshot()")).WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow("00000003-0000001B-1"))
	m.ExpectRollback()
	epoch, err := s.BeginCommissionFlowCheckpointEpoch(context.Background())
	require.NoError(t, err)
	require.Equal(t, privateSHA([]byte("synthetic-db-identity")), epoch.DatabaseSHA256)
	require.NoError(t, epoch.Close())
	require.NoError(t, m.ExpectationsWereMet())
}
func TestFlowCheckpointEpochUnavailableDoesNotReadOrSave(t *testing.T) {
	for _, mode := range []string{"expired", "database_changed", "injection", "live_fallback"} {
		t.Run(mode, func(t *testing.T) {
			s, m := checkpointMockServer(t)
			opts := &CommissionFlowCheckpointOptions{Directory: checkpointTestDir(t), Snapshot: "00000003-0000001B-1", DatabaseSHA256: privateSHA([]byte("expected-db"))}
			loader := &dbFlowLoader{s: s, private: true, checkpointOptions: opts}
			switch mode {
			case "injection":
				opts.Snapshot = "x'; SELECT 1; --"
			case "live_fallback":
				loader.node = &ledgerNode{rest: &servicehttp.Endpoint{}}
			default:
				m.ExpectBegin()
				q := m.ExpectExec(regexp.QuoteMeta("SET TRANSACTION SNAPSHOT '" + opts.Snapshot + "'"))
				if mode == "expired" {
					q.WillReturnError(errors.New("invalid snapshot identifier"))
				} else {
					q.WillReturnResult(sqlmock.NewResult(0, 0))
					m.ExpectQuery(regexp.QuoteMeta(commissionCheckpointDatabaseSQL)).WillReturnRows(sqlmock.NewRows([]string{"identity"}).AddRow("different-db"))
				}
				m.ExpectRollback()
			}
			saves := 0
			_, err := s.traceCommissionSaved("synthetic", loader, "synthetic-root", flowTestStart, checkpointTestID().AsOf, func(CommissionFlowRun, []CommissionFlow, CommissionFlowEvidence) error { saves++; return nil })
			require.Error(t, err)
			require.Zero(t, saves)
			require.Empty(t, mustCheckpointEntries(t, opts.Directory))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func checkpointReceiptLoader() *dbFlowLoader {
	p := &CommissionPrivateProfile{digest: "profile", SourceSHA256: "source"}
	c := &CommissionSourceCorrections{digest: "correction"}
	return &dbFlowLoader{chain: "synthetic", private: true, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}, s: &Server{commissionPrivateProfile: p, commissionSourceCorrections: c, privateProjectionReceipts: &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}, sourceCorrectionReceipts: newCorrectionJournal()}, archive: &flowArchive{holdings: map[string]flowHoldings{}}}
}
func TestFlowCheckpointPrivateReceiptRoundTrip(t *testing.T) {
	original := checkpointReceiptLoader()
	original.labels["synthetic-wallet"] = AddressLabel{Chain: "synthetic", Address: "synthetic-wallet", Name: "fixture", CreatedAt: flowTestStart}
	original.chans["port/channel"] = "synthetic-remote"
	hooks := original.checkpointReceiptHooks()
	initial, err := hooks.Capture()
	require.NoError(t, err)
	projection := CommissionPrivateProjectionReceipt{ProfileSHA256: "profile", SourceSHA256: "source", Snapshot: "original-source-snapshot", OriginalParserErrors: 1, ReclassifiedParserErrors: 1}
	original.s.privateProjectionReceipts.retain([]string{"synthetic-wallet"}, flowTestStart, checkpointTestID().AsOf, projection)
	correction := CommissionSourceCorrectionReceipt{ProfileSHA256: "correction", Snapshot: "original-correction-snapshot", WalletSHA256: privateSHA([]byte("synthetic-wallet")), Start: flowTestStart, End: checkpointTestID().AsOf, Status: "fixture-retained"}
	original.s.sourceCorrectionReceipts.retain(correction)
	raw := `{"synthetic":"retained-response"}`
	receipt := FlowArchiveReceipt{Address: "synthetic-wallet", Height: 123, Method: "block", SHA256: privateSHA([]byte(raw)), Response: raw}
	original.archive.receipts = append(original.archive.receipts, receipt)
	delta, err := hooks.Capture()
	require.NoError(t, err)
	empty, err := hooks.Capture()
	require.NoError(t, err)
	var emptyEvidence CommissionFlowEvidence
	require.NoError(t, json.Unmarshal(empty, &emptyEvidence))
	require.Empty(t, emptyEvidence.Archive)
	require.Empty(t, emptyEvidence.PrivateProjections)
	require.Empty(t, emptyEvidence.SourceCorrectionReads)
	require.Empty(t, emptyEvidence.Labels)
	restored := checkpointReceiptLoader()
	restore := restored.checkpointReceiptHooks()
	require.NoError(t, restore.Restore(initial))
	require.NoError(t, restore.Restore(delta))
	before, _ := json.Marshal(original.evidence())
	after, _ := json.Marshal(restored.evidence())
	require.Equal(t, string(before), string(after))
	require.Equal(t, original.archive.receipts, restored.archive.receipts)
	require.Equal(t, "original-source-snapshot", restored.s.privateProjectionReceipts.receipts()[0].Snapshot)
	require.Equal(t, "original-correction-snapshot", restored.s.sourceCorrectionReceipts.receipts()[0].Snapshot)
	cached, reused, err := restored.archive.headers.get(123, func() (string, error) { return "", errors.New("must not reread header") })
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, raw, cached)
	holdings := flowHoldings{Liquid: amt(7), Source: CommissionFlowHoldingSource{Kind: "archive_exact", Height: 123}}
	restored.restoreCheckpointResult(flowCheckpointRequest{Kind: "opening", Address: "synthetic-wallet", Start: flowTestStart}, flowCheckpointResult{Holdings: holdings, Known: true})
	actual, known := restored.archive.cachedHoldings("synthetic-wallet", flowTestStart)
	require.True(t, known)
	require.Equal(t, holdings, actual)
	// Foreign profiles fail before changing any other receipt collection.
	poisoned := checkpointReceiptLoader()
	poisoned.s.commissionPrivateProfile.digest = "different"
	require.Error(t, poisoned.checkpointReceiptHooks().Restore(delta))
	require.Empty(t, poisoned.archive.receipts)
	require.Empty(t, poisoned.s.sourceCorrectionReceipts.receipts())
}
func TestFlowCheckpointReceiptDeltaIsLinear(t *testing.T) {
	loader := checkpointReceiptLoader()
	hooks := loader.checkpointReceiptHooks()
	for i := 0; i < 1000; i++ {
		loader.s.sourceCorrectionReceipts.retain(CommissionSourceCorrectionReceipt{ProfileSHA256: "correction", Snapshot: "original", Start: flowTestStart.Add(time.Duration(i) * time.Second)})
		raw, err := hooks.Capture()
		require.NoError(t, err)
		var delta CommissionFlowEvidence
		require.NoError(t, json.Unmarshal(raw, &delta))
		require.Len(t, delta.SourceCorrectionReads, 1)
		require.Less(t, len(raw), 2048)
	}
	require.Len(t, loader.s.sourceCorrectionReceipts.receipts(), 1000)
}
