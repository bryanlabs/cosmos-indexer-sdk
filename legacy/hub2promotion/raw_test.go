package hub2promotion

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func rawByteFixture(t *testing.T) (Manifest, rawSource, []map[string]any) {
	dir := artifactDir(t)
	m := Manifest{First: 1, Last: 4, Scope: Scope{ChainID: "cosmoshub-1", RawStage: true}}
	source := rawSource{Version: 1, Chain: "cosmoshub-1"}
	var events []map[string]any
	var blocks strings.Builder
	for height := int64(1); height <= 4; height++ {
		hash := strings.Repeat("AB", 32)
		hashBytes := make([]byte, 32)
		for i := range hashBytes {
			hashBytes[i] = 0xab
		}
		lines := []map[string]any{
			{"kind": "source_anchor", "height": height - 1, "unknown": "retained-anchor"},
			{"kind": "height", "height": height, "block_hash": base64.StdEncoding.EncodeToString(hashBytes), "transactions": []string{"AQID"}, "deliver": []any{map[string]any{"code": 5, "unknown_failure": "retained"}}, "header": map[string]any{"chain_id": "cosmoshub-1", "height": height, "time": "2020-01-01T00:00:00Z", "proposer_address": "synthetic-proposer"}, "unknown_native_module": map[string]any{"exact": "retained"}},
			{"kind": "history_completed", "height": height},
		}
		var history strings.Builder
		for _, line := range lines {
			b, e := json.Marshal(line)
			require.NoError(t, e)
			history.Write(b)
			history.WriteByte('\n')
		}
		original := savePin(t, dir, fmt.Sprintf("history-%d.jsonl", height), []byte(history.String()))
		artifactBytes := []byte("synthetic raw module bytes retained without interpretation")
		artifact := savePin(t, dir, fmt.Sprintf("artifact-%d.raw", height), artifactBytes)
		source.Components = append(source.Components, rawComponent{First: height, Last: height, History: original, Artifacts: []Pin{artifact}, Compression: "none"})
		index := 0
		event := func(kind string, attrs [][]string) {
			events = append(events, map[string]any{"chain_id": "cosmoshub-1", "height": height, "phase": 1, "event_index": index, "type": kind, "attributes": attrs})
			index++
		}
		rawEvent := func(line map[string]any) {
			b, _ := json.Marshal(line)
			b = append(b, '\n')
			event("hub1_native033_raw_history", [][]string{{"encoding", "base64-exact-jsonl-line"}, {"kind", line["kind"].(string)}, {"raw", base64.StdEncoding.EncodeToString(b)}})
		}
		rawEvent(lines[0])
		event("hub1_raw_history_status", [][]string{{"profile", hub1ProjectionProfile}, {"derived_inclusions", "deferred"}, {"derived_tax_rows", "deferred"}, {"tax_complete", "false"}, {"canonical_height", "500000"}, {"historical_tail", "500001..500015"}, {"intermediate_zero_height_payouts", "validation-only-not-income"}})
		event("hub1_source_artifact_chunk", [][]string{{"artifact", "0"}, {"chunk", "0"}, {"encoding", "base64-exact-source-bytes"}, {"raw", base64.StdEncoding.EncodeToString(artifactBytes)}})
		event("hub1_source_artifact_complete", [][]string{{"artifact", "0"}, {"source_path", artifact.Path}, {"sha256", artifact.SHA256}, {"bytes", fmt.Sprint(len(artifactBytes))}, {"chunks", "1"}})
		rawEvent(lines[1])
		rawEvent(lines[2])
		block := map[string]any{"chain_id": "cosmoshub-1", "height": height, "time": "2020-01-01T00:00:00+00:00", "block_hash": hash, "proposer": "synthetic-proposer", "tx_count": 1}
		b, e := json.Marshal(block)
		require.NoError(t, e)
		blocks.Write(b)
		blocks.WriteByte('\n')
	}
	var out strings.Builder
	for _, row := range events {
		b, e := json.Marshal(row)
		require.NoError(t, e)
		out.Write(b)
		out.WriteByte('\n')
	}
	m.Tables = []Projection{{Table: "blocks", Columns: projectionColumns(inserts[6]), Rows: 4, File: savePin(t, dir, "blocks.ndjson", []byte(blocks.String()))}, {Table: "block_events", Columns: projectionColumns(inserts[8]), Rows: int64(len(events)), File: savePin(t, dir, "block_events.ndjson", []byte(out.String()))}}
	return m, source, events
}
func TestRawBytesPreserveUnknownFieldsFailuresAndAllArtifactBytes(t *testing.T) {
	m, source, _ := rawByteFixture(t)
	require.NoError(t, m.verifyRawBytes(source))
}
func TestRawBytesRejectMissingArtifactChangedHeaderStatusAndSource(t *testing.T) {
	for _, kind := range []string{"missing-artifact", "changed-native", "wrong-status", "duplicate-attribute", "incomplete-tail", "header-count"} {
		t.Run(kind, func(t *testing.T) {
			m, source, events := rawByteFixture(t)
			switch kind {
			case "missing-artifact":
				events = append(events[:2], events[4:]...)
			case "changed-native":
				attrs := events[4]["attributes"].([][]string)
				raw, _ := base64.StdEncoding.DecodeString(attrs[2][1])
				attrs[2][1] = base64.StdEncoding.EncodeToString(append(raw[:len(raw)-1], []byte(" \n")...))
			case "wrong-status":
				events[1]["attributes"].([][]string)[3][1] = "true"
			case "duplicate-attribute":
				attrs := events[1]["attributes"].([][]string)
				events[1]["attributes"] = append(attrs, attrs[0])
			case "incomplete-tail":
				events = events[:len(events)-1]
			case "header-count":
				p := m.Tables[0].File
				b, e := readPinnedBytes(p, 1<<20)
				require.NoError(t, e)
				m.Tables[0].File = savePin(t, artifactDir(t), "wrong-header.ndjson", []byte(strings.Replace(string(b), `"tx_count":1`, `"tx_count":0`, 1)))
			}
			var out strings.Builder
			for _, row := range events {
				b, e := json.Marshal(row)
				require.NoError(t, e)
				out.Write(b)
				out.WriteByte('\n')
			}
			m.Tables[1].File = savePin(t, artifactDir(t), "changed-events.ndjson", []byte(out.String()))
			require.Error(t, m.verifyRawBytes(source))
		})
	}
}
