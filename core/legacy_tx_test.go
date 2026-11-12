package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	tmjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	"github.com/stretchr/testify/require"
)

// Original Amino payloads remain local-only. The offline adapter must preserve
// them; the serving path stays blocked until all intake gates are satisfied.
func TestLegacyAminoPrivateOriginalsOfflineIntake(t *testing.T) {
	dir := os.Getenv("LEGACY_PRIVATE_FIXTURE_DIR")
	if dir == "" {
		t.Skip("local private historical captures not supplied")
	}
	var observedTx, observedFailure, observedDerived, observedNonzeroFee bool
	for _, name := range []string{"hub3-original.json", "hub3-failed-original.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		var captures map[string]map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &captures))
		for _, capture := range captures {
			var block struct {
				Result struct {
					Block struct {
						Header struct {
							ChainID string    `json:"chain_id"`
							Height  int64     `json:"height,string"`
							Time    time.Time `json:"time"`
						} `json:"header"`
						Data struct {
							Txs []string `json:"txs"`
						} `json:"data"`
					} `json:"block"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(capture["block"], &block))
			var envelope struct {
				Result json.RawMessage `json:"result"`
			}
			require.NoError(t, json.Unmarshal(capture["block_results"], &envelope))
			var results rpc.CustomBlockResults
			if tmjson.Unmarshal(envelope.Result, &results) != nil {
				t.Fatal("historical results could not be normalized")
			}
			var txs tm.Txs
			for _, encoded := range block.Result.Block.Data.Txs {
				raw, err := base64.StdEncoding.DecodeString(encoded)
				require.NoError(t, err)
				txs = append(txs, raw)
			}
			originals := make([][]byte, len(txs))
			for i, raw := range txs {
				originals[i] = bytes.Clone(raw)
			}
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: block.Result.Block.Header.ChainID, Height: block.Result.Block.Header.Height, Time: block.Result.Block.Header.Time}, Data: tm.Data{Txs: txs}}}
			candidates, decodeErr := DecodeLegacyBlock(input, &results)
			if decodeErr != nil {
				t.Fatal("original Amino offline intake failed", decodeErr)
			}
			if len(candidates) != len(txs) {
				t.Fatal("offline intake lost inclusions")
			}
			for i, candidate := range candidates {
				if !bytes.Equal(candidate.Tx.Raw, originals[i]) || !bytes.Equal(candidate.RawResultEnvelope, envelope.Result) {
					t.Fatal("offline intake changed original evidence")
				}
				if candidate.ClassificationReady {
					t.Fatal("offline candidate bypassed classification gate")
				}
				for _, message := range candidate.Tx.Messages {
					if message.Derived != nil {
						observedDerived = true
					}
				}
				if !candidate.Fee.Declared.IsZero() {
					observedNonzeroFee = true
				}
				if candidate.Result.Code != 0 && (candidate.Execution != "failed" || candidate.Fee.Charge != "unknown") {
					t.Fatal("failed execution or uncertain fees became successful/free")
				}
			}
			cfg := &config.IndexConfig{}
			cfg.Flags.IndexEmptyTransactions = true
			wrappers, _, err := ProcessRPCBlockByHeightTXs(cfg, nil, &client.ChainClient{Codec: client.MakeCodecConfig()}, nil, nil, input, &results, nil)
			if len(txs) == 0 {
				require.Error(t, err)
				require.Empty(t, wrappers)
				continue
			}
			if err == nil || len(wrappers) != 0 {
				t.Fatal("unsupported original Amino transactions must not become accepted index rows")
			}
			observedTx = true
			for i, raw := range txs {
				if !bytes.Equal(originals[i], raw) {
					t.Fatal("historical transaction wire bytes changed")
				}
			}
			for _, result := range results.TxsResults {
				if result.Code != 0 {
					observedFailure = true
				}
			}
		}
	}
	require.True(t, observedTx)
	require.True(t, observedFailure)
	require.True(t, observedDerived)
	require.True(t, observedNonzeroFee)
}
