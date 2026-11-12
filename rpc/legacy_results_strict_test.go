package rpc

import (
	tmjson "github.com/cometbft/cometbft/libs/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLegacyResultCodeEvidenceAndTags(t *testing.T) {
	var old CustomBlockResults
	require.NoError(t, tmjson.Unmarshal([]byte(`{"height":"9","results":{"DeliverTx":[{"log":"[]","gasWanted":"90","gasUsed":"31","tags":[{"key":"YWN0aW9u","value":"c2VuZA=="}]}],"EndBlock":{"tags":[{"key":"aw==","value":"dg=="}]},"BeginBlock":{}}}`), &old))
	require.Equal(t, uint32(0), old.TxsResults[0].Code)
	require.Equal(t, "omitted-zero-omitempty", old.Legacy.CodeEvidence[0])
	require.Equal(t, "action", string(old.Legacy.TxTags[0][0].Key))
	require.Empty(t, old.TxsResults[0].Events)
	require.Len(t, old.Legacy.EndTags, 1)
	require.NotEmpty(t, old.Legacy.RawJSON)
	// Reject an empty response as insufficient execution evidence, even though
	// the old serializer can omit individual zero-valued fields.
	var emptyOld CustomBlockResults
	require.Error(t, tmjson.Unmarshal([]byte(`{"height":"9","results":{"DeliverTx":[{}]}}`), &emptyOld))
	var failed CustomBlockResults
	require.NoError(t, tmjson.Unmarshal([]byte(`{"height":"9","results":{"DeliverTx":[{"code":65540,"log":"failed"}]}}`), &failed))
	require.Equal(t, uint32(65540), failed.TxsResults[0].Code)
	require.Equal(t, "explicit", failed.Legacy.CodeEvidence[0])
}
func TestLegacyValidatorKeysExplicitNormalization(t *testing.T) {
	var result CustomBlockResults
	input := []byte(`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"pub_key":{"type":"ed25519","data":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="},"power":"71"}]}}}`)
	require.NoError(t, tmjson.Unmarshal(input, &result))
	require.Len(t, result.ValidatorUpdates, 1)
	require.Len(t, result.ValidatorUpdates[0].PubKey.GetEd25519(), 32)
	require.Equal(t, int64(71), result.ValidatorUpdates[0].Power)
	require.Equal(t, input, result.Legacy.RawJSON)
	for _, bad := range []string{
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"pub_key":{"type":"ed25519","data":"AQ=="},"power":"71"}]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"pub_key":{"type":"ed25519","data":"AQ==","future":0},"power":"71"}]}}}`,
	} {
		require.Error(t, tmjson.Unmarshal([]byte(bad), &result))
	}
}

func TestLegacyResultsRejectUnknownNestedAndMissingExecution(t *testing.T) {
	for _, input := range []string{
		`{"height":"9","results":{"deliver_tx":[{}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":null}]}}`,
		`{"height":"9","results":{"DeliverTx":[{"code":null}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":"0"}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":-1}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":4294967296}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":0,"code":9}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":0,"future":"not ignored"}]}}`,
		`{"height":"9","results":{"deliver_tx":[],"future":[]}}`,
		`{"height":"9","results":{"deliver_tx":[],"DeliverTx":[]}}`,
		`{"height":"9","results":{"DeliverTx":[],"begin_block":{}}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"future":[]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"begin_block":{"events":[{"type":"x","future":0}]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"begin_block":{"events":[{"type":"x","attributes":[{"key":"aw==","value":"dg==","future":0}]}]}}}`,
		`{"height":"9","results":{"DeliverTx":[{"tags":[{"key":"aw==","value":"dg==","future":0}]}]}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"power":"1","future":0}]}}}`,
	} {
		var result CustomBlockResults
		require.Error(t, tmjson.Unmarshal([]byte(input), &result))
	}
}

// The document is read once for repeated keys. That one read has to refuse a
// repeat at any depth, with the message for a repeat.
func TestLegacyResultsRejectARepeatedKeyAtEveryDepth(t *testing.T) {
	for _, input := range []string{
		`{"height":"9","height":"9","results":{"deliver_tx":[]}}`,
		`{"height":"9","results":{"deliver_tx":[],"deliver_tx":[]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":0,"events":[],"events":[]}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":0,"events":[{"type":"x","type":"x","attributes":[]}]}]}}`,
		`{"height":"9","results":{"deliver_tx":[{"code":0,"events":[{"type":"x","attributes":[{"key":"aw==","key":"aw==","value":"dg=="}]}]}]}}`,
		`{"height":"9","results":{"deliver_tx":[],"begin_block":{"events":[],"events":[]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"begin_block":{"events":[{"type":"x","attributes":[{"key":"aw==","value":"dg==","value":"dg=="}]}]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"events":[{"type":"x","attributes":[],"attributes":[]}]}}}`,
		`{"height":"9","results":{"DeliverTx":[{"code":1,"tags":[{"key":"aw==","value":"dg==","value":"dg=="}]}]}}`,
		`{"height":"9","results":{"DeliverTx":[],"EndBlock":{"tags":[{"key":"aw==","key":"aw==","value":"dg=="}]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"pub_key":{"type":"ed25519","data":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="},"power":"1","power":"1"}]}}}`,
		`{"height":"9","results":{"deliver_tx":[],"end_block":{"validator_updates":[{"pub_key":{"type":"ed25519","type":"ed25519","data":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="},"power":"1"}]}}}`,
	} {
		var result CustomBlockResults
		require.ErrorContains(t, tmjson.Unmarshal([]byte(input), &result), "duplicate legacy JSON field", input)
	}
}
