package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/rpc"
	tmjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
)

// Uses only previously retained private captures. It never contacts a provider
// or copies a private payload into committed testdata. Failures print metadata.
func TestLegacyRetainedCacheAllHubEras(t *testing.T) {
	directory := os.Getenv("LEGACY_RETAINED_CACHE_DIR")
	if directory == "" {
		t.Skip("retained local Hub1-3 captures not supplied")
	}
	count := 0
	for _, tc := range []struct{ block, result string }{{"b1.json", "r1.json"}, {"b2.json", "r2.json"}, {"b3.json", "r3.json"}} {
		raw, err := os.ReadFile(filepath.Join(directory, tc.block))
		if err != nil {
			t.Fatal("retained block unavailable")
		}
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
		if json.Unmarshal(raw, &block) != nil {
			t.Fatal("retained block JSON invalid")
		}
		raw, err = os.ReadFile(filepath.Join(directory, tc.result))
		if err != nil {
			t.Fatal("retained results unavailable")
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			t.Fatal("retained results envelope invalid")
		}
		var results rpc.CustomBlockResults
		if err := tmjson.Unmarshal(envelope.Result, &results); err != nil {
			t.Fatal("retained result normalization failed", err)
		}
		input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: block.Result.Block.Header.ChainID, Height: block.Result.Block.Header.Height, Time: block.Result.Block.Header.Time}}}
		for _, encoded := range block.Result.Block.Data.Txs {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal("retained tx encoding invalid")
			}
			input.Block.Txs = append(input.Block.Txs, raw)
		}
		candidates, err := DecodeLegacyBlock(input, &results)
		if err != nil {
			t.Fatal("retained offline adapter failed", err)
		}
		if len(candidates) != len(input.Block.Txs) || len(candidates) == 0 {
			t.Fatal("retained inclusions missing")
		}
		for i, candidate := range candidates {
			if !bytes.Equal(candidate.Tx.Raw, input.Block.Txs[i]) {
				t.Fatal("retained transaction changed")
			}
			var messages [][]byte
			for _, field := range candidate.Tx.Fields {
				if field.Number == 1 {
					messages = append(messages, field.Value)
				}
			}
			if len(messages) != len(candidate.Tx.Messages) {
				t.Fatal("retained message count changed")
			}
			for j, msg := range candidate.Tx.Messages {
				if !bytes.Equal(msg.Raw, messages[j]) {
					t.Fatal("retained message changed")
				}
				if msg.Derived == nil {
					t.Fatal("retained common-message projection unsupported")
				}
			}
			if !candidate.Tx.SignersComplete || len(candidate.Tx.Signers) == 0 {
				t.Fatal("retained signers unresolved")
			}
			if candidate.Result.Code != 0 && candidate.Fee.Charge != "unknown" {
				t.Fatal("retained failed fees incorrectly resolved")
			}
			if candidate.ClassificationReady {
				t.Fatal("retained intake enabled classification")
			}
			count++
		}
	}
	if count != 4 {
		t.Fatal("unexpected retained inclusion count")
	}
}
