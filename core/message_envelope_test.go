package core

import (
	"errors"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	tx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type envelopeParser struct{ received bool }

func (p *envelopeParser) Identifier() string { return "envelope-test" }
func (p *envelopeParser) ParseMessage(message sdk.Msg, _ *txlog.LogMessage, _ config.IndexConfig) (*any, error) {
	envelope, ok := message.(*authz.MsgExec)
	if !ok || len(envelope.Msgs) != 2 || envelope.Msgs[0].TypeUrl != testUnknownMessageType {
		return nil, errors.New("outer envelope or inner evidence lost")
	}
	p.received = true
	return nil, errors.New("classification incomplete: unknown inner")
}

func (p *envelopeParser) IndexMessage(*any, *gorm.DB, models.Message, []parsers.MessageEventWithAttributes, config.IndexConfig) error {
	return nil
}

func TestKnownAuthzEnvelopeReachesParserWithUnknownChild(t *testing.T) {
	for _, path := range []string{"block", testRPCPath} {
		t.Run(path, func(t *testing.T) {
			codec := client.MakeCodecConfig()
			authz.RegisterInterfaces(codec.InterfaceRegistry)
			bank.RegisterInterfaces(codec.InterfaceRegistry)
			grantee := sdk.AccAddress([]byte("12345678901234567890")).String()
			known, err := codectypes.NewAnyWithValue(&bank.MsgSend{})
			require.NoError(t, err)
			envelope := &authz.MsgExec{Grantee: grantee, Msgs: []*codectypes.Any{{TypeUrl: testUnknownMessageType, Value: []byte{8, 1}}, known}}
			original, err := envelope.Marshal()
			require.NoError(t, err)
			body := &tx.TxBody{Messages: []*codectypes.Any{{TypeUrl: "/cosmos.authz.v1beta1.MsgExec", Value: original}}}
			auth := &tx.AuthInfo{Fee: &tx.Fee{Payer: grantee}}
			parser := &envelopeParser{}
			registry := map[string][]parsers.MessageParser{"/cosmos.authz.v1beta1.MsgExec": {parser}}
			cfg := &config.IndexConfig{}
			chain := &client.ChainClient{Codec: codec}
			now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			if path == testRPCPath {
				wrappers, _, err := ProcessRPCTXs(cfg, nil, chain, nil, nil, &tx.GetTxsEventResponse{Txs: []*tx.Tx{{Body: body, AuthInfo: auth}}, TxResponses: []*sdk.TxResponse{{Height: 1, TxHash: "ABC", Timestamp: now.Format(time.RFC3339)}}}, registry)
				require.NoError(t, err)
				require.True(t, parser.received)
				require.False(t, wrappers[0].Messages[0].PreserveOpaqueEvidence)
				require.Equal(t, original, wrappers[0].Messages[0].Message.MessageBytes)
				require.ErrorContains(t, wrappers[0].Messages[0].MessageParsedDatasets[0].Error, "classification incomplete")
				return
			}
			bb, err := body.Marshal()
			require.NoError(t, err)
			aa, err := auth.Marshal()
			require.NoError(t, err)
			raw, err := (&tx.TxRaw{BodyBytes: bb, AuthInfoBytes: aa}).Marshal()
			require.NoError(t, err)
			wrappers, _, err := ProcessRPCBlockByHeightTXs(cfg, nil, chain, nil, nil, &ctypes.ResultBlock{Block: &tm.Block{Header: tm.Header{Height: 1, Time: now}, Data: tm.Data{Txs: tm.Txs{raw}}}}, &rpc.CustomBlockResults{Height: 1, TxsResults: []*abci.ResponseDeliverTx{{Log: "[]"}}}, registry)
			require.NoError(t, err)
			require.True(t, parser.received)
			require.False(t, wrappers[0].Messages[0].PreserveOpaqueEvidence)
			require.Equal(t, original, wrappers[0].Messages[0].Message.MessageBytes)
			require.ErrorContains(t, wrappers[0].Messages[0].MessageParsedDatasets[0].Error, "classification incomplete")
		})
	}
}
