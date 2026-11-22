package taxapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommissionCaptureExactAndPrivate(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0700))
	snapshot := CommissionFlowVersion{Version: "private-fixture", Chain: "fixture", Validator: "fixture", Build: "fixture", RunJSON: "{\n\"x\":1.00}", RowsJSON: "[]", EvidenceJSON: `{"raw":"` + strings.Repeat("x", 2<<20) + `"}`}
	require.NoError(t, captureCommissionFlowPayload(root, snapshot))
	require.NoError(t, captureCommissionFlowPayload(root, snapshot))
	dirs, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, dirs, 2)
	for _, dir := range dirs {
		info, err := dir.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0700), info.Mode().Perm())
		raw, err := os.ReadFile(filepath.Join(root, dir.Name(), "manifest.json"))
		require.NoError(t, err)
		var manifest struct{ Fields []CommissionFlowPayload }
		require.NoError(t, json.Unmarshal(raw, &manifest))
		require.Len(t, manifest.Fields, 3)
		for _, field := range manifest.Fields {
			path := filepath.Join(root, dir.Name(), field.Field+".json.gz")
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			encoded, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, field.EncodedSHA256, payloadHash(encoded))
			require.EqualValues(t, len(encoded), field.EncodedBytes)
			gz, err := gzip.NewReader(bytes.NewReader(encoded))
			require.NoError(t, err)
			decoded, err := io.ReadAll(gz)
			require.NoError(t, err)
			require.NoError(t, gz.Close())
			expected, _ := payloadField(&snapshot, field.Field)
			require.Equal(t, *expected, string(decoded))
			require.Equal(t, field.RawSHA256, payloadHash(decoded))
			require.EqualValues(t, len(decoded), field.RawBytes)
		}
	}
	require.NoError(t, captureCommissionFlowPayload("", snapshot))
	require.Error(t, captureCommissionFlowPayload("relative", snapshot))
	require.NoError(t, os.Chmod(root, 0755))
	require.Error(t, captureCommissionFlowPayload(root, snapshot))
	require.NoError(t, os.Chmod(root, 0700))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(root, link))
	require.Error(t, captureCommissionFlowPayload(link, snapshot))
}

func TestCommissionCaptureFailureBeforeSave(t *testing.T) {
	t.Setenv("COSMOS_TAX_COMMISSION_CAPTURE_DIR", filepath.Join(t.TempDir(), "missing"))
	o, run := fixtureRun("capture-refusal")
	s := &Server{}
	require.Error(t, s.saveCommissionFlowVersion(o, run, nil))
}
