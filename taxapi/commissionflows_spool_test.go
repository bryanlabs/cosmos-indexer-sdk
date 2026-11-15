package taxapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCommissionStreamJSONMatchesNativeBytes(t *testing.T) {
	values := []any{
		CommissionFlowRun{Chain: "synthetic", ComputedAt: flowTestStart, Reason: "quote\"\\\b\f\n\r\t<>&\u2028\u2029\xff"},
		[]CommissionFlow{{Detail: "fixture", ViaUnbonding: true, AmountBase: "0"}},
		CommissionFlowEvidence{Channels: map[string]string{"z": "\x00", "a": "😀"}, Wallets: []CommissionFlowWalletEvidence{{Address: "synthetic", ActualStakedBase: map[string]string{"fixture": "0"}}}, ReadFailure: &CommissionReadFailure{PhaseMillis: map[string]int64{"test": 3}}},
		CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: "\x01\x08\x0c\x1f<>&", SHA256: "fixture"}}},
		map[string]any{"array": []any{nil, true, 12, 1.5}, "nil": []string(nil), "bytes": []byte{0, 1, 2}},
		flowTestStart.In(time.FixedZone("fixture", 3600)),
		CommissionFlowEvidence{SourceCorrectionReads: []CommissionSourceCorrectionReceipt{{Effects: []CommissionSourceEffectReceipt{{ResolvedSourceKey: &flowKey{Time: flowTestStart, Height: 123}, ProofSHA256: []string{"fixture"}}}}}},
		string(func() []byte {
			b := make([]byte, 256)
			for i := range b {
				b[i] = byte(i)
			}
			return b
		}()),
	}
	for _, value := range values {
		want, err := json.Marshal(value)
		require.NoError(t, err)
		var out bytes.Buffer
		require.NoError(t, streamCommissionJSON(&out, value))
		require.Equal(t, string(want), out.String())
	}
}

type commissionFailWriter struct{ left int }

func (w *commissionFailWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("synthetic disk fault")
	}
	w.left -= len(p)
	return len(p), nil
}
func TestCommissionStreamJSONFaultsAndBounds(t *testing.T) {
	require.Error(t, streamCommissionJSON(&commissionFailWriter{left: 8}, strings.Repeat("x", 1<<20)))
	require.Error(t, streamCommissionJSON(io.Discard, math.NaN()))
	type loop struct{ Next *loop }
	cyclic := &loop{}
	cyclic.Next = cyclic
	require.Error(t, streamCommissionJSON(io.Discard, cyclic))
	large := strings.Repeat("x", 64<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, streamCommissionJSON(io.Discard, CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: large}}}))
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(large)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<20), "serialization must not allocate a second giant receipt string")
}
func commissionTestSpool(t *testing.T, large bool) (*commissionFlowSpool, CommissionFlowVersion) {
	t.Helper()
	root := checkpointTestDir(t)
	snapshot := CommissionFlowVersion{Version: "synthetic", Chain: "synthetic", Validator: "synthetic", Build: "synthetic"}
	e := CommissionFlowEvidence{Channels: map[string]string{"fixture": "fixture"}}
	if large {
		e.Archive = []FlowArchiveReceipt{{Response: strings.Repeat("synthetic-response", 200000)}}
	}
	spool, err := spoolCommissionFlow(root, snapshot, CommissionFlowRun{Chain: "synthetic"}, []CommissionFlow{}, e)
	require.NoError(t, err)
	return spool, snapshot
}
func TestCommissionSpoolCaptureIntegrityAndInline(t *testing.T) {
	spool, snapshot := commissionTestSpool(t, false)
	require.FileExists(t, filepath.Join(spool.dir, "manifest.json"))
	require.NoFileExists(t, filepath.Join(spool.dir, "manifest.json.pending"))
	for _, m := range spool.fields {
		info, err := os.Stat(filepath.Join(spool.dir, m.Field+".json.gz"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		f, err := os.Open(filepath.Join(spool.dir, m.Field+".json.gz"))
		require.NoError(t, err)
		gz, err := gzip.NewReader(f)
		require.NoError(t, err)
		raw, err := io.ReadAll(gz)
		require.NoError(t, err)
		require.NoError(t, gz.Close())
		require.NoError(t, f.Close())
		require.Equal(t, m.RawSHA256, payloadHash(raw))
		require.True(t, json.Valid(raw))
	}
	server, mock := checkpointMockServer(t)
	require.NoError(t, storeCommissionSpools(server.db, &snapshot, spool))
	require.JSONEq(t, `[]`, snapshot.RowsJSON)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestCommissionSpoolCorruptionRejectsBeforeWrites(t *testing.T) {
	for _, mode := range []string{"corrupt", "truncate", "symlink", "mode"} {
		t.Run(mode, func(t *testing.T) {
			spool, snapshot := commissionTestSpool(t, false)
			path := filepath.Join(spool.dir, "run.json.gz")
			switch mode {
			case "corrupt":
				f, err := os.OpenFile(path, os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = f.WriteAt([]byte{0}, 0)
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "truncate":
				require.NoError(t, os.Truncate(path, 1))
			case "symlink":
				require.NoError(t, os.Rename(path, path+".retained"))
				require.NoError(t, os.Symlink(path+".retained", path))
			case "mode":
				require.NoError(t, os.Chmod(path, 0644))
			}
			server, mock := checkpointMockServer(t)
			require.Error(t, storeCommissionSpools(server.db, &snapshot, spool))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestCommissionSpoolLargeReceiptBoundedPackaging(t *testing.T) {
	large := strings.Repeat("x", 64<<20)
	e := CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: large}}}
	root := checkpointTestDir(t)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	spool, err := spoolCommissionFlow(root, CommissionFlowVersion{}, CommissionFlowRun{}, []CommissionFlow{}, e)
	require.NoError(t, err)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(large)
	require.Greater(t, spool.fields[2].RawBytes, int64(64<<20))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20), "packaging must not allocate whole-field JSON buffers")
}

func TestCommissionSpoolSerializationFailureNeverWritesResult(t *testing.T) {
	server, mock := checkpointMockServer(t)
	root := checkpointTestDir(t)
	t.Setenv("COSMOS_TAX_COMMISSION_CAPTURE_DIR", root)
	o := CommissionFlowBuildOptions{Version: "synthetic", Build: "synthetic", AsOf: flowTestStart.AddDate(0, 1, 0)}
	run := CommissionFlowRun{AsOf: o.AsOf.AddDate(0, 0, -1), ComputedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	require.Error(t, server.saveCommissionFlowVersion(o, run, nil))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoFileExists(t, filepath.Join(root, entries[0].Name(), "manifest.json"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCommissionSpoolExternalResultAtomicSuccess(t *testing.T) {
	spool, snapshot := commissionTestSpool(t, true)
	server, mock := checkpointMockServer(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "tax_commission_flow_payload_chunks"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO "tax_commission_flow_payloads"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO "tax_commission_flow_versions"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := server.db.Transaction(func(tx *gorm.DB) error {
		if err := storeCommissionSpools(tx, &snapshot, spool); err != nil {
			return err
		}
		return tx.Create(&snapshot).Error
	})
	require.NoError(t, err)
	require.True(t, isPayloadMarker(snapshot.EvidenceJSON))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCommissionSpoolChunkFailureRollsBack(t *testing.T) {
	spool, snapshot := commissionTestSpool(t, true)
	server, mock := checkpointMockServer(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "tax_commission_flow_payload_chunks"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO "tax_commission_flow_payloads"`).WillReturnError(errors.New("synthetic final metadata failure"))
	mock.ExpectRollback()
	err := server.db.Transaction(func(tx *gorm.DB) error {
		if err := storeCommissionSpools(tx, &snapshot, spool); err != nil {
			return err
		}
		return tx.Create(&snapshot).Error
	})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
