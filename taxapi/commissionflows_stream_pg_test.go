package taxapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A streamed save stores exactly what saving the whole json.Marshal string
// stored: the same complete index, the same chunk rows, the same capture.
// Run only against a dedicated test database, never production.
func TestCommissionFlowVersionStreamedEvidencePostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := fmt.Sprintf("commission_stream_proof_%d", time.Now().UnixNano())
	require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
	bootstrap, _ := db.DB()
	require.NoError(t, bootstrap.Close())
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err = gorm.Open(postgres.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, _ := db.DB()
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, db.Exec("SET TIME ZONE 'UTC'").Error)
	require.NoError(t, CommissionFlowVersionsMigrate(db))
	capture := t.TempDir()
	require.NoError(t, os.Chmod(capture, 0700))
	t.Setenv("COSMOS_TAX_COMMISSION_CAPTURE_DIR", capture)
	s := &Server{db: db}
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	opts := CommissionFlowBuildOptions{Version: "stream-v1", Build: "sha256:one", AsOf: asOf}
	for i, e := range []CommissionFlowEvidence{bigEvidence(), {Channels: map[string]string{"channel-0": "osmosis-1"}}} {
		validator := fmt.Sprintf("%s%d", flowPGValidator, i)
		run := CommissionFlowRun{Chain: apiMainnet, Validator: validator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: asOf, Status: flowStatusReconciled, Commission: "{}", Untracked: "{}", Overdraw: "{}", DriftBase: "0"}
		require.NoError(t, s.saveCommissionFlowVersion(opts, run, nil, e))
		raw, _ := json.Marshal(e)
		got, err := s.LoadCommissionFlowVersion(opts.Version, apiMainnet, validator)
		require.NoError(t, err)
		require.JSONEq(t, string(raw), got.EvidenceJSON)
		var stored CommissionFlowVersion
		require.NoError(t, db.Where("version = ? AND chain = ? AND validator = ?", opts.Version, apiMainnet, validator).Take(&stored).Error)
		if len(raw) <= commissionPayloadInlineLimit {
			require.JSONEq(t, string(raw), stored.EvidenceJSON)
			continue
		}
		require.Equal(t, string(raw), got.EvidenceJSON)
		want := CommissionFlowPayload{Version: opts.Version, Chain: apiMainnet, Validator: validator, Build: opts.Build, Field: "evidence"}
		var wantChunks []CommissionFlowPayloadChunk
		require.NoError(t, encodeCommissionPayload(raw, &want, func(o int, b []byte) error {
			wantChunks = append(wantChunks, CommissionFlowPayloadChunk{Version: want.Version, Chain: want.Chain, Validator: want.Validator, Field: "evidence", Ordinal: o, SHA256: payloadHash(b), Data: append([]byte(nil), b...)})
			return nil
		}))
		var m CommissionFlowPayload
		require.NoError(t, db.Where("version = ? AND validator = ? AND field = 'evidence'", opts.Version, validator).Take(&m).Error)
		require.Equal(t, want, m)
		require.Equal(t, payloadMarker(want), stored.EvidenceJSON)
		var chunks []CommissionFlowPayloadChunk
		require.NoError(t, db.Where("version = ? AND validator = ? AND field = 'evidence'", opts.Version, validator).Order("ordinal").Find(&chunks).Error)
		require.Equal(t, wantChunks, chunks)
		require.Greater(t, len(chunks), 1)
	}
	manifests, _ := filepath.Glob(filepath.Join(capture, "attempt-*", "manifest.json"))
	require.Len(t, manifests, 2)
	for _, p := range manifests {
		b, _ := os.ReadFile(p)
		require.True(t, strings.Contains(string(b), `"Field":"evidence"`))
	}
}
