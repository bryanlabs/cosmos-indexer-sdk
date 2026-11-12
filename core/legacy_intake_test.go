package core

import (
	"bytes"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/legacy/store"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	tmjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

type intakeMsg interface{ marker() }
type intakeSend struct {
	From, To []byte
	Amount   []intakeCoin
}

func (intakeSend) marker() {}

type intakeCoin struct{ Denom, Amount string }
type intakeFee struct {
	Amount []intakeCoin
	Gas    uint64
}
type intakeSignature struct {
	PubKey    []byte
	Signature []byte
}
type intakeTx struct {
	Msgs       []intakeMsg
	Fee        intakeFee
	Signatures []intakeSignature
	Memo       string
}

func intakeRaw(t *testing.T, chain string) tm.Tx {
	t.Helper()
	cdc := amino.NewCodec()
	cdc.RegisterInterface((*intakeMsg)(nil), nil)
	cdc.RegisterConcrete(intakeSend{}, "cosmos-sdk/MsgSend", nil)
	name := "auth/StdTx"
	if chain == "cosmoshub-3" {
		name = "cosmos-sdk/StdTx"
	}
	cdc.RegisterConcrete(intakeTx{}, name, nil)
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{intakeSend{a, b, []intakeCoin{{"uatom", "53"}}}}, Fee: intakeFee{Amount: []intakeCoin{{"uatom", "7"}}, Gas: 90000}})
	require.NoError(t, err)
	return raw
}
func TestLegacyOfflineAdapterFeesExecutionAndDuplicateInclusions(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		t.Run(chain, func(t *testing.T) {
			raw := intakeRaw(t, chain)
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: 9}, Data: tm.Data{Txs: tm.Txs{raw, raw}}}}
			json := `{"height":"9","results":{"DeliverTx":[{"log":"[]"},{"code":10}]}}`
			if chain == "cosmoshub-3" {
				json = `{"height":"9","results":{"deliver_tx":[{"code":0},{"code":10}]}}`
			}
			var results rpc.CustomBlockResults
			require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
			items, err := DecodeLegacyBlock(input, &results)
			require.NoError(t, err)
			require.Len(t, items, 2)
			require.NotEqual(t, items[0].Identity, items[1].Identity)
			require.Equal(t, items[0].Hash, items[1].Hash)
			require.Equal(t, raw, tm.Tx(items[0].Tx.Raw))
			require.Equal(t, "succeeded", items[0].Execution)
			require.Equal(t, "failed", items[1].Execution)
			require.Equal(t, "charged", items[0].Fee.Charge)
			require.Equal(t, "unknown", items[1].Fee.Charge)
			require.Equal(t, "7", items[0].Fee.Declared[0].Amount.String())
			require.Equal(t, "7", items[1].Fee.Declared[0].Amount.String())
			require.Equal(t, bytes.Repeat([]byte{1}, 20), []byte(items[1].Fee.Payer))
			for _, item := range items {
				require.False(t, item.ClassificationReady)
				require.NotEmpty(t, item.Issues)
				require.Equal(t, "cosmos-sdk/MsgSend", item.Tx.Messages[0].AminoName)
			}
			rows, _, err := ProcessRPCBlockByHeightTXs(&config.IndexConfig{}, nil, &client.ChainClient{}, nil, nil, input, &results, nil)
			require.ErrorContains(t, err, "legacy serving intake blocked")
			require.Empty(t, rows)
		})
	}
}
func TestLegacyFailedFeeChargedOnlyWithExecutionMarker(t *testing.T) {
	for chain, json := range map[string]string{
		"cosmoshub-2": `{"height":"9","results":{"DeliverTx":[{"tags":[{"key":"YWN0aW9u","value":"c2VuZA=="}]},{"code":10,"tags":[{"key":"YWN0aW9u","value":"c2VuZA=="}]},{"code":12},{"code":12,"log":"{\"codespace\":\"sdk\",\"code\":12,\"message\":\"out of gas in location: txSize; gasWanted: 1, gasUsed: 2\"}"}]}}`,
		"cosmoshub-3": `{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"},{"code":10,"log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"},{"code":12,"log":"{\"code\":12}"},{"code":12,"log":"{\"codespace\":\"sdk\",\"code\":12,\"message\":\"out of gas in location: ante verify: secp256k1; gasWanted: 1, gasUsed: 2\"}"}]}}`,
	} {
		t.Run(chain, func(t *testing.T) {
			raw := intakeRaw(t, chain)
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: 9}, Data: tm.Data{Txs: tm.Txs{raw, raw, raw, raw}}}}
			var results rpc.CustomBlockResults
			require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
			items, err := DecodeLegacyBlock(input, &results)
			require.NoError(t, err)
			require.Len(t, items, 4)
			require.Empty(t, items[3].Messages)
			require.Equal(t, "not charged", items[3].Fee.Charge)
			require.Len(t, items[0].Messages, 1)
			require.True(t, items[0].Messages[0].Success)
			require.Equal(t, "charged", items[0].Fee.Charge)
			require.Len(t, items[1].Messages, 1)
			require.False(t, items[1].Messages[0].Success)
			require.Equal(t, "charged", items[1].Fee.Charge)
			require.Empty(t, items[2].Messages)
			require.Equal(t, "unknown", items[2].Fee.Charge)
			for _, issue := range items[0].Issues {
				require.NotContains(t, issue, "unattributed")
			}
			require.Contains(t, items[2].Issues, "failed legacy transaction left no execution marker; fee commitment needs gas or state evidence")
		})
	}
}

func TestLegacyStoreBlockBuildsHubThreeRowsOrHoldsTheHeight(t *testing.T) {
	raw := intakeRaw(t, "cosmoshub-3")
	when := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: when}, Data: tm.Data{Txs: tm.Txs{raw, raw}}}}
	decode := func(json string) []LegacyInclusion {
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
		items, err := DecodeLegacyBlock(input, &results)
		require.NoError(t, err)
		return items
	}
	block, err := LegacyStoreBlock(input, decode(`{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"sender\",\"value\":\"payer\"},{\"key\":\"module\",\"value\":\"bank\"},{\"key\":\"action\",\"value\":\"send\"}]},{\"type\":\"transfer\",\"attributes\":[{\"key\":\"recipient\",\"value\":\"someone\"},{\"key\":\"amount\",\"value\":\"53uatom\"}]}]}]"},{"code":10,"log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"}]}}`))
	require.NoError(t, err)
	require.Equal(t, "cosmoshub-3", block.Chain)
	require.Equal(t, int64(9), block.Height)
	require.Equal(t, when, block.Time)
	require.Len(t, block.Inclusions, 2)
	from := sdk.AccAddress(bytes.Repeat([]byte{1}, 20)).String()
	to := sdk.AccAddress(bytes.Repeat([]byte{2}, 20)).String()
	first := block.Inclusions[0]
	require.Equal(t, block.Inclusions[1].Hash, first.Hash, "both inclusions of the same bytes are kept")
	require.Equal(t, []string{from}, first.Detail.Signers)
	require.Equal(t, from, first.Detail.FeePayer)
	require.Equal(t, []store.Coin{{Denom: "uatom", Amount: "7"}}, first.Detail.Fee)
	require.Equal(t, []store.Message{{TypeURL: "/cosmos.bank.v1beta1.MsgSend", Rows: []store.Row{{Category: "transfer", Amount: "53", Denom: "uatom", From: from, To: to}}}}, first.Detail.Messages)
	failed := block.Inclusions[1]
	require.Equal(t, uint32(10), failed.Code)
	require.Equal(t, []store.Coin{{Denom: "uatom", Amount: "7"}}, failed.Detail.Fee, "a failure after execution began still paid its fee")
	require.Equal(t, []store.Message{{TypeURL: "/cosmos.bank.v1beta1.MsgSend"}}, failed.Detail.Messages, "a failed transaction moved nothing")

	_, err = LegacyStoreBlock(input, decode(`{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"sender\",\"value\":\"payer\"},{\"key\":\"module\",\"value\":\"bank\"},{\"key\":\"action\",\"value\":\"send\"}]},{\"type\":\"transfer\",\"attributes\":[{\"key\":\"recipient\",\"value\":\"someone\"},{\"key\":\"amount\",\"value\":\"53uatom\"}]}]}]"},{"code":12,"log":"{\"code\":12}"}]}}`))
	require.ErrorContains(t, err, "legacy transaction 1 held: fee charge unknown")
	_, err = LegacyStoreBlock(input, decode(`{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"sender\",\"value\":\"payer\"},{\"key\":\"module\",\"value\":\"bank\"},{\"key\":\"action\",\"value\":\"send\"}]},{\"type\":\"transfer\",\"attributes\":[{\"key\":\"recipient\",\"value\":\"someone\"},{\"key\":\"amount\",\"value\":\"53uatom\"}]}]}]"},{"code":10,"log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"}]}}`)[:1])
	require.ErrorContains(t, err, "do not cover the block")
	older := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-2", Height: 9, Time: when}}}
	_, err = LegacyStoreBlock(older, nil)
	require.ErrorContains(t, err, "only built for cosmoshub-3")
}

func TestLegacyAllServingEntryPointsBlocked(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		cfg := config.IndexConfig{}
		cfg.Probe.ChainID = chain
		// Nil downstream inputs prove rejection happens before parsers or DB work.
		events, err := ProcessRPCBlockResults(cfg, models.Block{}, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, events)
		rows, _, err := ProcessRPCTXs(&cfg, nil, nil, nil, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, rows)
		events, err = ProcessRPCBlockResults(config.IndexConfig{}, models.Block{Chain: models.Chain{ChainID: chain}}, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, events)
	}
	events, err := ProcessRPCBlockResults(config.IndexConfig{}, models.Block{}, &rpc.CustomBlockResults{Legacy: &rpc.LegacyBlockEvidence{}}, nil, nil)
	require.ErrorContains(t, err, "legacy serving intake blocked")
	require.Nil(t, events)
}

func TestLegacyAdapterEvidenceFailsClosed(t *testing.T) {
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9}, Data: tm.Data{Txs: tm.Txs{intakeRaw(t, "cosmoshub-3")}}}}
	fresh := func() rpc.CustomBlockResults {
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal([]byte(`{"height":"9","results":{"deliver_tx":[{"code":0}]}}`), &results))
		return results
	}
	for _, mutate := range []func(*rpc.CustomBlockResults){func(r *rpc.CustomBlockResults) { r.Height = 10 }, func(r *rpc.CustomBlockResults) { r.TxsResults = nil }, func(r *rpc.CustomBlockResults) { r.TxsResults[0] = nil }, func(r *rpc.CustomBlockResults) { r.Legacy = nil }, func(r *rpc.CustomBlockResults) { r.Legacy.RawJSON = nil }, func(r *rpc.CustomBlockResults) { r.Legacy.Dialect = "tendermint-0.30-0.31" }, func(r *rpc.CustomBlockResults) { r.Legacy.CodeEvidence[0] = "" }, func(r *rpc.CustomBlockResults) { r.Legacy.CodeEvidence[0] = "omitted-zero-omitempty" }} {
		results := fresh()
		mutate(&results)
		items, err := DecodeLegacyBlock(input, &results)
		require.Error(t, err)
		require.Empty(t, items)
	}
	require.Error(t, LegacyIntakeAdmission())
}
