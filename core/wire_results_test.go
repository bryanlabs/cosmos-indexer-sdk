package core

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	any "github.com/cosmos/cosmos-sdk/codec/types"
	tx "github.com/cosmos/cosmos-sdk/types/tx"
	transfer "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	"github.com/stretchr/testify/require"
)

func TestTxResultsFailClosed(t *testing.T) {
	block := &ctypes.ResultBlock{Block: &tm.Block{Data: tm.Data{Txs: tm.Txs{{1}}}}}
	require.Error(t, ValidateBlockTxResults(block, nil))
	require.Error(t, ValidateBlockTxResults(block, &rpc.CustomBlockResults{}))
	require.Error(t, ValidateBlockTxResults(block, &rpc.CustomBlockResults{TxsResults: []*abci.ResponseDeliverTx{nil}}))
	results := &rpc.CustomBlockResults{TxsResults: []*abci.ResponseDeliverTx{{}}, FinalizeBlockEvents: []abci.Event{{Type: "upgrade"}}}
	_, err := NormalizeCustomBlockResults(results)
	require.Error(t, err)
	require.NoError(t, ValidateBlockTxResults(block, results), "tx extraction must not depend on event modes")
	results.Height = 1
	require.Error(t, ValidateBlockTxResults(block, results), "wrong-height results must fail closed")
	_, err = NormalizeCustomBlockResults(nil)
	require.Error(t, err)
}

func TestKnownTransferOriginalWireBytes(t *testing.T) {
	c := client.MakeCodecConfig()
	transfer.RegisterInterfaces(c.InterfaceRegistry)
	// Older transfer wire omits the zero-valued nested timeout height (field 6).
	// Modern SDK marshaling inserts 32 00, even though it has no semantic effect.
	original := []byte{10, 8, 't', 'r', 'a', 'n', 's', 'f', 'e', 'r', 18, 9, 'c', 'h', 'a', 'n', 'n', 'e', 'l', '-', '0'}
	sender := "cosmos1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqnrql8a"
	original = append(original, 34, byte(len(sender)))
	original = append(original, []byte(sender)...)
	var msg transfer.MsgTransfer
	require.NoError(t, msg.Unmarshal(original))
	normalized, err := msg.Marshal()
	require.NoError(t, err)
	require.NotEqual(t, original, normalized)
	body, err := (&tx.TxBody{Messages: []*any.Any{{TypeUrl: "/ibc.applications.transfer.v1.MsgTransfer", Value: original}}}).Marshal()
	require.NoError(t, err)
	auth, err := (&tx.AuthInfo{Fee: &tx.Fee{}}).Marshal()
	require.NoError(t, err)
	raw, err := (&tx.TxRaw{BodyBytes: body, AuthInfoBytes: auth}).Marshal()
	require.NoError(t, err)
	cfg := &config.IndexConfig{}
	wrappers, _, err := ProcessRPCBlockByHeightTXs(cfg, nil, &client.ChainClient{Codec: c}, nil, nil, &ctypes.ResultBlock{Block: &tm.Block{Header: tm.Header{Height: 1, Time: time.Now()}, Data: tm.Data{Txs: tm.Txs{raw}}}}, &rpc.CustomBlockResults{Height: 1, TxsResults: []*abci.ResponseDeliverTx{{Log: "[]"}}}, nil)
	require.NoError(t, err)
	require.Len(t, wrappers, 1)
	require.Equal(t, original, wrappers[0].Messages[0].Message.MessageBytes)
}
