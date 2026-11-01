package core

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	tx "github.com/cosmos/cosmos-sdk/types/tx"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"
)

func TestOpaqueEvidenceDecodingPaths(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, path := range []string{"block", "rpc"} {
			t.Run(path+map[bool]string{true: "/mixed", false: "/unknown-only"}[mixed], func(t *testing.T) {
				c := client.MakeCodecConfig()
				bank.RegisterInterfaces(c.InterfaceRegistry)
				cl := &client.ChainClient{Codec: c}
				messages := []*codectypes.Any{{TypeUrl: "/future.MsgUnknown", Value: []byte{8, 1, 18, 2, 'x', 'y'}}}
				if mixed {
					known, err := codectypes.NewAnyWithValue(&bank.MsgSend{})
					require.NoError(t, err)
					messages = append(messages, known)
				}
				body := &tx.TxBody{Messages: messages}
				auth := &tx.AuthInfo{Fee: &tx.Fee{Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 17)), Payer: "cosmos1payer"}}
				logs := sdk.ABCIMessageLogs{{MsgIndex: 0, Events: sdk.StringEvents{{Type: "future", Attributes: []sdk.Attribute{{Key: "second", Value: "2"}, {Key: "first", Value: "1"}}}, {Type: "another"}}}}
				if mixed {
					logs = append(logs, sdk.ABCIMessageLog{MsgIndex: 1})
				}
				now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
				cfg := &config.IndexConfig{}
				if path == "rpc" {
					result, _, err := ProcessRPCTXs(cfg, nil, cl, nil, nil, &tx.GetTxsEventResponse{Txs: []*tx.Tx{{Body: body, AuthInfo: auth}}, TxResponses: []*sdk.TxResponse{{Height: 1, TxHash: "ABC", Timestamp: now.Format(time.RFC3339), Logs: logs}}}, nil)
					require.NoError(t, err)
					require.Len(t, result, 1)
					require.Len(t, result[0].Messages, len(messages))
					m := result[0].Messages[0]
					require.True(t, m.PreserveOpaqueEvidence)
					require.Equal(t, messages[0].Value, m.Message.MessageBytes)
					require.Equal(t, "/future.MsgUnknown", m.Message.MessageType.MessageType)
					require.Equal(t, 0, m.Message.MessageIndex)
					require.Len(t, m.MessageEvents, 2)
					require.Equal(t, "second", m.MessageEvents[0].Attributes[0].MessageEventAttributeKey.Key)
					require.Len(t, result[0].Tx.Fees, 1)
				} else {
					bb, err := body.Marshal()
					require.NoError(t, err)
					aa, err := auth.Marshal()
					require.NoError(t, err)
					raw, err := (&tx.TxRaw{BodyBytes: bb, AuthInfoBytes: aa}).Marshal()
					require.NoError(t, err)
					result, _, err := ProcessRPCBlockByHeightTXs(cfg, nil, cl, nil, nil, &coretypes.ResultBlock{Block: &tmtypes.Block{Header: tmtypes.Header{Height: 1, Time: now}, Data: tmtypes.Data{Txs: tmtypes.Txs{raw}}}}, &rpc.CustomBlockResults{TxsResults: []*abci.ResponseDeliverTx{{Log: logs.String()}}}, nil)
					require.NoError(t, err)
					require.Len(t, result, 1)
					require.Len(t, result[0].Messages, len(messages))
					m := result[0].Messages[0]
					require.True(t, m.PreserveOpaqueEvidence)
					require.Equal(t, messages[0].Value, m.Message.MessageBytes)
					require.Equal(t, "/future.MsgUnknown", m.Message.MessageType.MessageType)
					require.Equal(t, 0, m.Message.MessageIndex)
					require.Len(t, m.MessageEvents, 2)
					require.Equal(t, uint64(1), m.MessageEvents[1].MessageEvent.Index)
					require.Len(t, result[0].Tx.Fees, 1)
				}
			})
		}
	}
}
