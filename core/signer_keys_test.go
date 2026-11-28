package core

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptoTypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	tx "github.com/cosmos/cosmos-sdk/types/tx"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"
)

// Key types added after this build's SDK (v0.47): bls12_381 in 0.54, mldsa65
// and secp256k1eth in 0.55 (OPS-760 review).
var futureSignerKeyTypes = []string{
	"/cosmos.crypto.mldsa65.PubKey",
	"/cosmos.crypto.secp256k1eth.PubKey",
	"/cosmos.crypto.bls12_381.PubKey",
}

func signerKeyTestClient() *client.ChainClient {
	c := client.MakeCodecConfig()
	bank.RegisterInterfaces(c.InterfaceRegistry)
	cryptocodec.RegisterInterfaces(c.InterfaceRegistry)
	return &client.ChainClient{Codec: c}
}

func futureKey(typeURL string) *codectypes.Any {
	return &codectypes.Any{TypeUrl: typeURL, Value: []byte{10, 3, 'k', 'e', 'y'}}
}

func knownKey(t *testing.T, pub cryptoTypes.PubKey) *codectypes.Any {
	a, err := codectypes.NewAnyWithValue(pub)
	require.NoError(t, err)
	return a
}

func rawSignerKeyTx(t *testing.T, messages []*codectypes.Any, keys []*codectypes.Any, payer string) []byte {
	t.Helper()
	auth := &tx.AuthInfo{Fee: &tx.Fee{Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 17)), Payer: payer}}
	for _, k := range keys {
		auth.SignerInfos = append(auth.SignerInfos, &tx.SignerInfo{PublicKey: k, Sequence: 3})
	}
	bb, err := (&tx.TxBody{Messages: messages}).Marshal()
	require.NoError(t, err)
	aa, err := auth.Marshal()
	require.NoError(t, err)
	raw, err := (&tx.TxRaw{BodyBytes: bb, AuthInfoBytes: aa, Signatures: [][]byte{{1}}}).Marshal()
	require.NoError(t, err)
	return raw
}

func msgSendAny(t *testing.T, from, to sdk.AccAddress) *codectypes.Any {
	a, err := codectypes.NewAnyWithValue(&bank.MsgSend{FromAddress: from.String(), ToAddress: to.String(), Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 5))})
	require.NoError(t, err)
	return a
}

func runSignerKeyBlock(cl *client.ChainClient, txs ...[]byte) ([]dbTypes.TxDBWrapper, error) {
	block := &coretypes.ResultBlock{Block: &tmtypes.Block{Header: tmtypes.Header{Height: 1, Time: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}}}
	results := &rpc.CustomBlockResults{Height: 1}
	logs := sdk.ABCIMessageLogs{{MsgIndex: 0, Events: sdk.StringEvents{{Type: "ops760_probe", Attributes: []sdk.Attribute{{Key: "kept", Value: "1"}}}}}}
	for _, raw := range txs {
		block.Block.Txs = append(block.Block.Txs, raw)
		results.TxsResults = append(results.TxsResults, &abci.ResponseDeliverTx{Log: logs.String()})
	}
	wrappers, _, err := ProcessRPCBlockByHeightTXs(&config.IndexConfig{}, nil, cl, nil, nil, block, results, nil)
	return wrappers, err
}

// The OPS-760 review found that one transaction signed with an SDK 0.55 key
// failed its whole block: "TX cannot be parsed from block 1 ... failed to
// unmarshal auth info". The block must now index, with that transaction
// stored as opaque evidence and its neighbour decoded as usual.
func TestUnreadableSignerKeyDoesNotFailBlock(t *testing.T) {
	sender := sdk.AccAddress(secp256k1.GenPrivKey().PubKey().Address())
	recipient := sdk.AccAddress(secp256k1.GenPrivKey().PubKey().Address())
	neighbourKey := secp256k1.GenPrivKey().PubKey()
	neighbour := sdk.AccAddress(neighbourKey.Address())

	for _, keyType := range futureSignerKeyTypes {
		t.Run(keyType, func(t *testing.T) {
			cl := signerKeyTestClient()
			send := msgSendAny(t, sender, recipient)
			unreadable := rawSignerKeyTx(t, []*codectypes.Any{send}, []*codectypes.Any{futureKey(keyType)}, "")
			readable := rawSignerKeyTx(t, []*codectypes.Any{msgSendAny(t, neighbour, recipient)}, []*codectypes.Any{knownKey(t, neighbourKey)}, "")

			result, err := runSignerKeyBlock(cl, unreadable, readable)
			require.NoError(t, err)
			require.Len(t, result, 2)

			kept := result[0]
			require.Len(t, kept.Messages, 1)
			m := kept.Messages[0]
			require.True(t, m.Message.Opaque, "message must be marked unreadable")
			require.True(t, m.PreserveOpaqueEvidence)
			require.Equal(t, "/cosmos.bank.v1beta1.MsgSend", m.Message.MessageType.MessageType)
			require.Equal(t, send.Value, m.Message.MessageBytes, "wire bytes kept for a later reindex")
			require.Len(t, m.MessageEvents, 1)
			require.Len(t, kept.Tx.SignerAddresses, 1)
			require.Equal(t, sender.String(), kept.Tx.SignerAddresses[0].Address)
			require.Len(t, kept.Tx.Fees, 1)
			require.Equal(t, sender.String(), kept.Tx.Fees[0].PayerAddress.Address)

			other := result[1]
			require.Len(t, other.Messages, 1)
			require.False(t, other.Messages[0].Message.Opaque)
			require.Equal(t, neighbour.String(), other.Tx.SignerAddresses[0].Address)
		})
	}
}

func TestUnreadableMultisigMemberIsOpaque(t *testing.T) {
	cl := signerKeyTestClient()
	sender := sdk.AccAddress(secp256k1.GenPrivKey().PubKey().Address())
	inner, err := codectypes.NewAnyWithValue(secp256k1.GenPrivKey().PubKey())
	require.NoError(t, err)
	// LegacyAminoPubKey{threshold: 1, public_keys: [secp256k1, mldsa65]}.
	ms := []byte{8, 1}
	for _, k := range []*codectypes.Any{inner, futureKey(futureSignerKeyTypes[0])} {
		b, err := k.Marshal()
		require.NoError(t, err)
		require.Less(t, len(b), 128, "one-byte length prefix")
		ms = append(append(append(ms, 18), byte(len(b))), b...) // #nosec G115 -- checked above
	}
	key := &codectypes.Any{TypeUrl: "/cosmos.crypto.multisig.LegacyAminoPubKey", Value: ms}
	result, err := runSignerKeyBlock(cl, rawSignerKeyTx(t, []*codectypes.Any{msgSendAny(t, sender, sender)}, []*codectypes.Any{key}, ""))
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.True(t, result[0].Messages[0].Message.Opaque)
	require.Equal(t, sender.String(), result[0].Tx.Fees[0].PayerAddress.Address)
}

// The fee payer is the first signer. With an unreadable first key and a
// message whose signer cannot be decoded, nothing names it, so the block is
// deferred rather than charging the fee to a guessed address.
func TestUnreadableSignerKeyNeverGuessesFeePayer(t *testing.T) {
	cl := signerKeyTestClient()
	secondKey := secp256k1.GenPrivKey().PubKey()
	second := sdk.AccAddress(secondKey.Address())
	unknown := &codectypes.Any{TypeUrl: testUnknownMessageType, Value: []byte{8, 1}}
	raw := rawSignerKeyTx(t, []*codectypes.Any{unknown, msgSendAny(t, second, second)}, []*codectypes.Any{futureKey(futureSignerKeyTypes[0]), knownKey(t, secondKey)}, "")
	_, err := runSignerKeyBlock(cl, raw)
	require.ErrorContains(t, err, "fee payer unresolved")

	// An explicit payer settles it, and the transaction is kept.
	raw = rawSignerKeyTx(t, []*codectypes.Any{unknown, msgSendAny(t, second, second)}, []*codectypes.Any{futureKey(futureSignerKeyTypes[0]), knownKey(t, secondKey)}, second.String())
	result, err := runSignerKeyBlock(cl, raw)
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Len(t, result[0].Messages, 2)
	for _, m := range result[0].Messages {
		require.True(t, m.Message.Opaque)
	}
	require.Equal(t, second.String(), result[0].Tx.Fees[0].PayerAddress.Address)
}

// A first signer with a readable single key names the fee payer even when a
// later key is unreadable and a message did not decode.
func TestReadableFirstSignerKeyNamesFeePayer(t *testing.T) {
	cl := signerKeyTestClient()
	firstKey := secp256k1.GenPrivKey().PubKey()
	first := sdk.AccAddress(firstKey.Address())
	unknown := &codectypes.Any{TypeUrl: testUnknownMessageType, Value: []byte{8, 1}}
	raw := rawSignerKeyTx(t, []*codectypes.Any{unknown}, []*codectypes.Any{knownKey(t, firstKey), futureKey(futureSignerKeyTypes[1])}, "")
	result, err := runSignerKeyBlock(cl, raw)
	require.NoError(t, err)
	require.Equal(t, first.String(), result[0].Tx.Fees[0].PayerAddress.Address)
	require.True(t, result[0].Messages[0].Message.Opaque)
}

// Corrupt auth info still fails: only an unreadable key type is tolerated.
func TestCorruptAuthInfoStillFails(t *testing.T) {
	cl := signerKeyTestClient()
	bb, err := (&tx.TxBody{}).Marshal()
	require.NoError(t, err)
	raw, err := (&tx.TxRaw{BodyBytes: bb, AuthInfoBytes: []byte{0xff, 0xff, 0xff}}).Marshal()
	require.NoError(t, err)
	_, err = InAppTxDecoder(cl.Codec)(raw)
	require.ErrorContains(t, err, "failed to unmarshal auth info")
}
