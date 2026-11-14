package taxapi

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCommissionPayloadCodecExactJSONBytes(t *testing.T) {
	// Preserve whitespace, key order, exact numeric spelling and opaque archive
	// response strings, rather than comparing a JSONB-normalized approximation.
	raw := []byte("{\n  \"z\": [1.00, \"\\u0061\"], \"archive\": \"{\\n  \\\"raw\\\": 7}\\n\"\n}")
	var m CommissionFlowPayload
	var chunks [][]byte
	require.NoError(t, encodeCommissionPayload(raw, &m, func(i int, b []byte) error {
		require.Equal(t, len(chunks), i)
		chunks = append(chunks, append([]byte(nil), b...))
		return nil
	}))
	require.Equal(t, int64(len(raw)), m.RawBytes)
	require.Equal(t, payloadHash(raw), m.RawSHA256)
	reader, err := gzip.NewReader(bytes.NewReader(bytes.Join(chunks, nil)))
	require.NoError(t, err)
	decoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, raw, decoded)
	require.NoError(t, reader.Close())
	require.True(t, isPayloadMarker(payloadMarker(m)))
	var old CommissionFlowEvidence
	require.Error(t, json.Unmarshal([]byte(payloadMarker(m)), &old), "old direct struct reader must fail closed, not accept empty evidence")
}

func TestCommissionPayloadCodecRefusesInvalidJSONAndSinkFailure(t *testing.T) {
	var m CommissionFlowPayload
	require.Error(t, encodeCommissionPayload([]byte("{broken"), &m, func(int, []byte) error { return nil }))
	err := encodeCommissionPayload([]byte(`{"valid":true}`), &m, func(int, []byte) error { return fmt.Errorf("fixture sink refusal") })
	require.ErrorContains(t, err, "fixture sink refusal")
	require.Empty(t, m.EncodedSHA256, "a failed encode cannot publish a complete index")
}

func payloadFixtureDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_PAYLOAD_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_PAYLOAD_TEST_DSN to the isolated payload fixture database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	var database string
	require.NoError(t, db.Raw("SELECT current_database()").Scan(&database).Error)
	require.True(t, strings.HasPrefix(database, "commission_payload_writer_"), "refuse DDL outside an explicitly named empty private fixture database")
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, db.Exec("SET statement_timeout='90s'").Error)
	require.NoError(t, db.Exec("SET lock_timeout='2s'").Error)
	require.NoError(t, db.Exec("SET work_mem='4MB'").Error)
	require.NoError(t, db.Exec("SET max_parallel_workers_per_gather=0").Error)
	require.NoError(t, db.Exec("SET jit=off").Error)
	require.NoError(t, CommissionFlowVersionsMigrate(db))
	return db
}

func fixtureVersion(t *testing.T) string {
	return fmt.Sprintf("payload-%s-%d", t.Name(), time.Now().UnixNano())
}

func fixtureRun(version string) (CommissionFlowBuildOptions, CommissionFlowRun) {
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	o := CommissionFlowBuildOptions{Version: version, Build: "fixture-source-not-production", AsOf: asOf}
	run := CommissionFlowRun{Chain: apiMainnet, Validator: "fixture-validator-not-a-real-address", FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: asOf, Status: flowStatusReconciled, Commission: "{}", Untracked: "{}", Overdraw: "{}", DriftBase: "0"}
	return o, run
}

func fixtureSnapshot(t *testing.T, db *gorm.DB, version string) CommissionFlowVersion {
	t.Helper()
	var snapshot CommissionFlowVersion
	require.NoError(t, db.Where("version = ?", version).Take(&snapshot).Error)
	return snapshot
}

func TestCommissionPayloadPostgresRoundtripAndImmutability(t *testing.T) {
	db := payloadFixtureDB(t)
	s := &Server{db: db}
	o, run := fixtureRun(fixtureVersion(t))
	// Pseudorandom encoded raw text exercises multiple compressed bytea chunks.
	rng := rand.New(rand.NewSource(19))
	noise := make([]byte, 3*commissionPayloadChunkSize)
	_, err := rng.Read(noise)
	require.NoError(t, err)
	response := "{\n\"opaque\":\"" + hex.EncodeToString(noise) + "\"\n}"
	evidence := CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Address: "fixture-not-a-real-wallet", Height: 77, SHA256: payloadHash([]byte(response)), Response: response}}}
	raw, err := json.Marshal(evidence)
	require.NoError(t, err)
	require.NoError(t, s.saveCommissionFlowVersion(o, run, nil, evidence))
	stored := fixtureSnapshot(t, db, o.Version)
	require.True(t, isPayloadMarker(stored.EvidenceJSON))
	var manifest CommissionFlowPayload
	require.NoError(t, db.Where("version = ? AND field = 'evidence'", o.Version).Take(&manifest).Error)
	require.Greater(t, manifest.ChunkCount, 1)
	var decoded bytes.Buffer
	require.NoError(t, s.ReadCommissionFlowPayload(stored, "evidence", &decoded))
	require.Equal(t, raw, decoded.Bytes())
	loaded, err := s.LoadCommissionFlowVersion(o.Version, run.Chain, run.Validator)
	require.NoError(t, err)
	require.Equal(t, string(raw), loaded.EvidenceJSON)
	var typed CommissionFlowEvidence
	require.NoError(t, json.Unmarshal([]byte(loaded.EvidenceJSON), &typed))
	require.Equal(t, evidence, typed)
	var exported bytes.Buffer
	require.NoError(t, s.ExportCommissionFlowVersions(o, run.Chain, run.FromDay, []string{run.Validator}, &exported))
	var auditInput []map[string]any
	require.NoError(t, json.Unmarshal(exported.Bytes(), &auditInput))
	require.Len(t, auditInput, 1)
	require.Equal(t, string(raw), auditInput[0]["evidence_json"], "offline audit export must resolve markers without changing original JSON bytes")
	wrongExport := o
	wrongExport.Build = "different-computation"
	require.Error(t, s.ExportCommissionFlowVersions(wrongExport, run.Chain, run.FromDay, []string{run.Validator}, io.Discard))
	require.Error(t, s.ExportCommissionFlowVersions(o, run.Chain, run.FromDay, []string{run.Validator, run.Validator}, io.Discard))
	require.Error(t, s.ExportCommissionFlowVersions(o, run.Chain, run.FromDay, []string{"fixture-missing"}, io.Discard))
	exists, err := s.reuseCommissionFlowVersion(o, run.Chain, run.Validator, run.FromDay)
	require.NoError(t, err)
	require.True(t, exists)
	require.Error(t, s.saveCommissionFlowVersion(o, run, nil, evidence))
	require.Equal(t, stored, fixtureSnapshot(t, db, o.Version), "duplicate save must preserve original result")
	var after CommissionFlowPayload
	require.NoError(t, db.Where("version = ? AND field = 'evidence'", o.Version).Take(&after).Error)
	require.Equal(t, manifest, after)
	// A mismatched computation build is still refused, never a compatibility waiver.
	wrong := o
	wrong.Build = "different-computation"
	_, err = s.reuseCommissionFlowVersion(wrong, run.Chain, run.Validator, run.FromDay)
	require.Error(t, err)
}

func TestCommissionPayloadPostgresNegativeControls(t *testing.T) {
	db := payloadFixtureDB(t)
	s := &Server{db: db}
	for _, control := range []string{"missing_manifest", "missing_chunk", "extra_chunk", "wrong_ordinal", "chunk_checksum", "raw_checksum", "raw_length", "encoded_checksum", "encoded_length", "wrong_build", "wrong_codec", "gzip_corrupt"} {
		t.Run(control, func(t *testing.T) {
			o, run := fixtureRun(fixtureVersion(t))
			evidence := CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: strings.Repeat("opaque fixture ", commissionPayloadInlineLimit/4)}}}
			require.NoError(t, s.saveCommissionFlowVersion(o, run, nil, evidence))
			stored := fixtureSnapshot(t, db, o.Version)
			where := db.Where("version = ? AND field = 'evidence'", o.Version)
			switch control {
			case "missing_manifest":
				require.NoError(t, where.Delete(&CommissionFlowPayload{}).Error)
			case "missing_chunk":
				require.NoError(t, where.Delete(&CommissionFlowPayloadChunk{}).Error)
			case "extra_chunk":
				require.NoError(t, db.Create(&CommissionFlowPayloadChunk{Version: o.Version, Chain: run.Chain, Validator: run.Validator, Field: "evidence", Ordinal: 100, SHA256: payloadHash([]byte("x")), Data: []byte("x")}).Error)
			case "wrong_ordinal":
				require.NoError(t, where.Model(&CommissionFlowPayloadChunk{}).Update("ordinal", 2).Error)
			case "chunk_checksum":
				require.NoError(t, where.Model(&CommissionFlowPayloadChunk{}).Update("sha256", strings.Repeat("0", 64)).Error)
			case "raw_checksum":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("raw_sha256", strings.Repeat("0", 64)).Error)
			case "raw_length":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("raw_bytes", commissionPayloadInlineLimit+1).Error)
			case "encoded_checksum":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("encoded_sha256", strings.Repeat("0", 64)).Error)
			case "encoded_length":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("encoded_bytes", 1).Error)
			case "wrong_build":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("build", "wrong").Error)
			case "wrong_codec":
				require.NoError(t, where.Model(&CommissionFlowPayload{}).Update("codec", "unknown").Error)
			case "gzip_corrupt":
				var chunk CommissionFlowPayloadChunk
				require.NoError(t, where.Take(&chunk).Error)
				chunk.Data[len(chunk.Data)/2] ^= 0xff
				require.NoError(t, where.Model(&CommissionFlowPayloadChunk{}).Updates(map[string]any{"data": chunk.Data, "sha256": payloadHash(chunk.Data)}).Error)
			}
			require.Error(t, s.ReadCommissionFlowPayload(stored, "evidence", io.Discard))
			_, err := s.LoadCommissionFlowVersion(o.Version, run.Chain, run.Validator)
			require.Error(t, err)
			_, err = s.reuseCommissionFlowVersion(o, run.Chain, run.Validator, run.FromDay)
			require.Error(t, err, "corrupt/incomplete payload cannot be reused as a success")
		})
	}
}

func TestCommissionPayloadPostgresReadOnlyExportProfile(t *testing.T) {
	db := payloadFixtureDB(t)
	s := &Server{db: db}
	o, run := fixtureRun(fixtureVersion(t))
	require.NoError(t, s.saveCommissionFlowVersion(o, run, []CommissionFlow{}, CommissionFlowEvidence{}))
	require.NoError(t, db.Exec("SET default_transaction_read_only=on").Error)
	var readOnly string
	require.NoError(t, db.Raw("SHOW default_transaction_read_only").Scan(&readOnly).Error)
	require.Equal(t, "on", readOnly)
	require.NoError(t, s.ExportCommissionFlowVersions(o, run.Chain, run.FromDay, []string{run.Validator}, io.Discard))
	err := db.Exec("CREATE TABLE fixture_forbidden_write (id int)").Error
	require.Error(t, err)
	var state interface{ SQLState() string }
	require.ErrorAs(t, err, &state)
	require.Equal(t, "25006", state.SQLState(), "the same source reader session must refuse writes")
}

func TestCommissionPayloadPostgresTransactionRollback(t *testing.T) {
	db := payloadFixtureDB(t)
	s := &Server{db: db}
	o, run := fixtureRun(fixtureVersion(t))
	// A pre-existing conflicting chunk forces an INSERT failure after other
	// chunks could have been created. Only fixture-owned evidence is modified.
	require.NoError(t, db.Create(&CommissionFlowPayloadChunk{Version: o.Version, Chain: run.Chain, Validator: run.Validator, Field: "evidence", Ordinal: 0, SHA256: "fixture-conflict", Data: []byte("fixture")}).Error)
	evidence := CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: strings.Repeat("x", 2*commissionPayloadInlineLimit)}}}
	require.Error(t, s.saveCommissionFlowVersion(o, run, nil, evidence))
	for _, model := range []any{&CommissionFlowVersion{}, &CommissionFlowGeneration{}, &CommissionFlowPayload{}} {
		var n int64
		require.NoError(t, db.Model(model).Where("version = ?", o.Version).Count(&n).Error)
		require.Zero(t, n, "no complete result or generation may escape rollback")
	}
	var chunks int64
	require.NoError(t, db.Model(&CommissionFlowPayloadChunk{}).Where("version = ?", o.Version).Count(&chunks).Error)
	require.EqualValues(t, 1, chunks, "the original conflicting fixture must remain unchanged")
}

func TestCommissionPayloadPostgresOversizedJSONBAndLosslessSave(t *testing.T) {
	if os.Getenv("COSMOS_TAX_PAYLOAD_HUGE_TEST") != "1" {
		t.Skip("opt-in synthetic oversized proof only, never the actual unavailable failed payload")
	}
	db := payloadFixtureDB(t)
	started := time.Now()
	// This is the actual PostgreSQL JEntry array limit, not a mocked error.
	// It is intentionally performed only on the isolated fixture database.
	var result string
	err := db.Raw("SELECT jsonb_build_array(repeat('a', 134217728), repeat('b', 134217728))::text").Scan(&result).Error
	require.Error(t, err)
	var state interface{ SQLState() string }
	require.ErrorAs(t, err, &state)
	require.Equal(t, "54000", state.SQLState())
	require.Contains(t, err.Error(), "jsonb array elements")
	t.Logf("synthetic PostgreSQL JSONB limit refused in %s, original financial payload unavailable", time.Since(started).Round(time.Millisecond))

	s := &Server{db: db}
	o, run := fixtureRun(fixtureVersion(t))
	// Two individually legal opaque responses exceed the shared array bound.
	// The complete original JSON is kept, with no omitted wallet or byte.
	response := strings.Repeat("x", 134217728)
	evidence := CommissionFlowEvidence{Archive: []FlowArchiveReceipt{{Response: response}, {Response: response}}}
	raw, err := json.Marshal(evidence)
	require.NoError(t, err)
	require.Greater(t, len(raw), 268435455)
	encodedRun, err := json.Marshal(run)
	require.NoError(t, err)
	legacyAttempt := CommissionFlowVersion{Version: o.Version + "-legacy-rejected", Chain: run.Chain, Validator: run.Validator, Build: o.Build, ComputedAt: run.ComputedAt, RunJSON: string(encodedRun), RowsJSON: "[]", EvidenceJSON: string(raw)}
	err = db.Create(&legacyAttempt).Error
	require.Error(t, err, "the unchanged old JSONB column cannot store this exact synthetic evidence value")
	require.ErrorAs(t, err, &state)
	require.Equal(t, "54000", state.SQLState())
	require.Contains(t, err.Error(), "jsonb array elements")
	var rejectedRows int64
	require.NoError(t, db.Model(&CommissionFlowVersion{}).Where("version = ?", legacyAttempt.Version).Count(&rejectedRows).Error)
	require.Zero(t, rejectedRows)
	legacyAttempt.EvidenceJSON = ""
	require.NoError(t, s.saveCommissionFlowVersion(o, run, nil, evidence))
	stored := fixtureSnapshot(t, db, o.Version)
	var m CommissionFlowPayload
	require.NoError(t, db.Where("version = ? AND field = 'evidence'", o.Version).Take(&m).Error)
	require.EqualValues(t, len(raw), m.RawBytes)
	require.Equal(t, payloadHash(raw), m.RawSHA256)
	require.NoError(t, s.ReadCommissionFlowPayload(stored, "evidence", io.Discard))
	// Decoder consumer sees the exact two responses and lengths, not a sentinel.
	loaded, err := s.LoadCommissionFlowVersion(o.Version, run.Chain, run.Validator)
	require.NoError(t, err)
	require.Equal(t, raw, []byte(loaded.EvidenceJSON))
	var decoded CommissionFlowEvidence
	require.NoError(t, json.Unmarshal([]byte(loaded.EvidenceJSON), &decoded))
	require.Len(t, decoded.Archive, 2)
	require.Equal(t, response, decoded.Archive[0].Response)
	require.Equal(t, response, decoded.Archive[1].Response)
	// Exercise the real offline audit export reader on the oversized value,
	// without printing or writing payloads into the test log.
	require.NoError(t, s.ExportCommissionFlowVersions(o, run.Chain, run.FromDay, []string{run.Validator}, io.Discard))
	t.Logf("synthetic oversized raw_bytes=%d encoded_bytes=%d chunks=%d elapsed=%s", m.RawBytes, m.EncodedBytes, m.ChunkCount, time.Since(started).Round(time.Millisecond))
}
