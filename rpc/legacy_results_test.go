package rpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tmjson "github.com/cometbft/cometbft/libs/json"
	"github.com/stretchr/testify/require"
)

// Captured cosmoshub-3 result envelopes, with account-bearing values, data,
// validator updates and free text removed. Height/code/gas and envelope shape
// are historical; these are not synthetic successful replacements for failures.
func TestLegacyResultsHistoricalFixtures(t *testing.T) {
	for _, tc := range []struct {
		height string
		codes  []uint32
		gas    int64
	}{
		{"1", nil, 0}, {"100000", []uint32{0, 0}, 89327}, {"3410168", []uint32{10, 0}, 0},
	} {
		t.Run(tc.height, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "legacy", tc.height+"-results.json"))
			require.NoError(t, err)
			var result CustomBlockResults
			require.NoError(t, tmjson.Unmarshal(data, &result))
			require.Len(t, result.TxsResults, len(tc.codes))
			for i, code := range tc.codes {
				require.Equal(t, code, result.TxsResults[i].Code)
			}
			require.NotEmpty(t, result.BeginBlockEvents)
			if tc.gas != 0 {
				require.Equal(t, tc.gas, result.TxsResults[0].GasUsed)
				require.Equal(t, int64(250000), result.TxsResults[0].GasWanted)
			}
			if tc.height == "100000" {
				require.Equal(t, "validator", result.TxsResults[0].Events[0].Attributes[0].Key)
				require.Equal(t, "[redacted]", result.TxsResults[0].Events[0].Attributes[0].Value)
			}
		})
	}
}

func TestLegacyResultsRejectAmbiguityAndCorruption(t *testing.T) {
	for _, data := range []string{
		`{"height":"1","results":null}`,
		`{"height":"1","results":{}}`,
		`{"height":"1","results":{"deliver_tx":[null]}}`,
		`{"height":"1","results":{"deliver_tx":[]},"txs_results":null}`,
		`{"height":"1","results":{"deliver_tx":[]},"begin_block_events":[]}`,
		`{"height":"1","results":{"deliver_tx":[{"gasWanted":"not-a-number"}]}}`,
		`{"height":"1","results":{"deliver_tx":[],"begin_block":{"events":[{"type":"transfer","attributes":[{"key":"not base64!","value":""}]}]}}}`,
	} {
		var result CustomBlockResults
		require.Error(t, tmjson.Unmarshal([]byte(data), &result))
	}
	// A modern text attribute that happens to look like base64 stays literal.
	var modern CustomBlockResults
	require.NoError(t, tmjson.Unmarshal([]byte(`{"height":"7","txs_results":[{"code":0,"gas_used":"12","events":[{"type":"x","attributes":[{"key":"YW1vdW50","value":"Nw=="}]}]}]}`), &modern))
	require.Equal(t, "YW1vdW50", modern.TxsResults[0].Events[0].Attributes[0].Key)
	require.Equal(t, "Nw==", modern.TxsResults[0].Events[0].Attributes[0].Value)
	require.Equal(t, int64(12), modern.TxsResults[0].GasUsed)
}

// Optional local-only originals verify that sanitization did not hide a codec
// incompatibility. Never place these files in the repository or log their data.
func TestLegacyResultsPrivateOriginals(t *testing.T) {
	directory := os.Getenv("LEGACY_PRIVATE_FIXTURE_DIR")
	if directory == "" {
		t.Skip("local private historical captures not supplied")
	}
	count := 0
	for _, name := range []string{"hub3-original.json", "hub3-failed-original.json"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, err)
		var captures map[string]map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &captures))
		for _, capture := range captures {
			var envelope struct {
				Result json.RawMessage `json:"result"`
			}
			require.NoError(t, json.Unmarshal(capture["block_results"], &envelope))
			var result CustomBlockResults
			if tmjson.Unmarshal(envelope.Result, &result) != nil {
				t.Fatal("original historical result decoding failed")
			}
			var original struct {
				Height  string `json:"height"`
				Results struct {
					DeliverTx []json.RawMessage `json:"deliver_tx"`
				} `json:"results"`
			}
			require.NoError(t, json.Unmarshal(envelope.Result, &original))
			require.Len(t, result.TxsResults, len(original.Results.DeliverTx))
			count++
		}
	}
	require.Equal(t, 3, count)
}
