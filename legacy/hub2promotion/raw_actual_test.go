package hub2promotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Opt-in real-source SQL exercise, never production or another native replay.
// It retains its new database and files on the established private PG helper.
func TestPrivateActualHub1RawStageDriver(t *testing.T) {
	dir := os.Getenv("HUB1_RAW_ACTUAL_DIRECTORY")
	if dir == "" {
		t.Skip("explicit real Hub1 private SQL exercise only")
	}
	require.True(t, filepath.IsAbs(dir))
	root := os.Getenv("HUB1_RAW_ACTUAL_RUN_ROOT")
	require.True(t, filepath.IsAbs(root))
	require.NoError(t, os.Mkdir(root, 0700))
	started := time.Now()
	phase := func(name string, extra map[string]any) {
		v := map[string]any{"phase": name, "utc": time.Now().UTC(), "seconds": time.Since(started).Seconds(), "production_writes": 0, "full_chain_books_tax_acceptance": false}
		for k, x := range extra {
			v[k] = x
		}
		b, e := json.Marshal(v)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(root, "current.safe.json"), b, 0600))
	}
	pin := func(path string) Pin {
		file, e := os.Open(path)
		require.NoError(t, e)
		defer file.Close()
		h := sha256.New()
		_, e = io.Copy(h, file)
		require.NoError(t, e)
		return Pin{Path: path, SHA256: hex.EncodeToString(h.Sum(nil))}
	}
	phase("binding_actual_normalized_files", nil)
	producer := pin("/Users/danb/.cache/cosmos-tax-hub1-recording-recovery-20261115/raw-projection-attempt2/projection.safe.json")
	b, e := readPinnedBytes(producer, 16<<20)
	require.NoError(t, e)
	var receipt struct {
		Tables     []Projection `json:"tables"`
		Components []struct {
			Opening json.RawMessage `json:"opening_commit"`
			Closing json.RawMessage `json:"closing_commit"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(b, &receipt))
	require.Len(t, receipt.Tables, 2)
	require.Len(t, receipt.Components, 4)
	for i := range receipt.Tables {
		receipt.Tables[i].File = pin(filepath.Join(dir, receipt.Tables[i].Table+".ndjson"))
	}
	sourcePin := Pin{Path: "/Users/danb/.cache/cosmos-tax-hub1-recording-recovery-20261115/hub1-history-adapter.inputs.safe.json", SHA256: hub1RawSourceSHA}
	data, e := readPinnedBytes(sourcePin, 16<<20)
	require.NoError(t, e)
	var source rawSource
	require.NoError(t, json.Unmarshal(data, &source))
	var components []Component
	for i, c := range source.Components {
		opening, e := decodeUnique(receipt.Components[i].Opening)
		require.NoError(t, e)
		closing, e := decodeUnique(receipt.Components[i].Closing)
		require.NoError(t, e)
		components = append(components, Component{First: c.First, Last: c.Last, Receipt: c.History, Opening: valueSHA(opening), Closing: valueSHA(closing), HistoricalOnly: true})
	}
	f, _ := rawFixture(t, false)
	m := f.m
	m.Profile = PrivateRawProfile
	m.First = 1
	m.Last = 500015
	m.Canonical = 500000
	m.MigrationLast = 500000
	m.TailFirst = 500001
	m.TailLast = 500015
	m.Source = sourcePin
	m.Genesis = pin("/Users/danb/.cache/cosmos-tax-hub1-native-boundary-aa6b004/genesis.cosmoshub-1.json")
	m.Tables = receipt.Tables
	m.Components = components
	m.Financial = Pin{}
	m.FinancialSignature = Pin{}
	m.Approved = &ApprovedLoad{ApprovalQuote: ApprovalQuote, ApprovalSource: "Current Dan Nov15 15:49 approval, actual real-source private SQL exercise; no production destination", CanonicalExport: pin("/Users/danb/.cache/cosmos-tax-hub1-native-boundary-aa6b004/native-export.json"), NextGenesis: pin("/Users/danb/.cache/cosmos-tax-hub1-native-boundary-aa6b004/genesis.cosmoshub-2.json")}
	m.RawHistory = &RawHistoryLoad{ProjectionReceipt: producer, RoundtripReceipt: pin("/Users/danb/.cache/cosmos-tax-hub1-recording-recovery-20261115/hub1-history-adapter.full-roundtrip.safe.json")}
	executable, e := os.Executable()
	require.NoError(t, e)
	binary := pin(executable)
	sourcePaths, e := filepath.Glob("*.go")
	require.NoError(t, e)
	var code []Pin
	for _, path := range sourcePaths {
		absolute, e := filepath.Abs(path)
		require.NoError(t, e)
		code = append(code, pin(absolute))
	}
	m.BuildSHA = valueSHA(code)
	m.BinarySHA = binary.SHA256
	build, e := json.Marshal(map[string]any{"profile": "actual private test execution, not production build acceptance", "binary": binary, "source_files": code, "source_sha": m.BuildSHA, "source_is_under_development": true})
	require.NoError(t, e)
	m.BuildIdentity = savePin(t, root, "actual-build.json", build)
	manifest, e := json.MarshalIndent(m, "", "  ")
	require.NoError(t, e)
	savePin(t, root, "private-manifest.json", manifest)
	phase("strict_actual_source_admission", map[string]any{"private_database": m.Destination.Database})
	require.NoError(t, m.Recheck())
	phase("actual_atomic_sql_merge", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	chunks := 0
	result, e := merge(ctx, f.loader, f.owner, m, f.runtime, hooks{afterChunk: func(n int) error {
		chunks = n
		if n%512 == 0 {
			phase("actual_atomic_sql_merge", map[string]any{"chunks_inserted": n})
		}
		return nil
	}})
	require.NoError(t, e)
	phase("committed_and_exact_readback", map[string]any{"rows": result.Rows, "chunks": chunks, "merge_seconds": result.Duration.Seconds(), "legacy_all14_preserved": true, "unpublished": true, "source_artifacts_preserved": true})
}
