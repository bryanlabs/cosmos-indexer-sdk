package taxapi

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"
)

const testMintAmount = "1000uatom"

func coins(t *testing.T, s string) sdk.Coins {
	t.Helper()
	c, err := sdk.ParseCoinsNormalized(s)
	require.NoError(t, err)
	return c
}

// protoTx builds transaction bytes whose body names the signer and whose
// auth info declares fee and granter.
func protoTx(t *testing.T, memo, fee, granter string) []byte {
	t.Helper()
	body := txtypes.TxBody{Memo: memo + " " + backTestSigner}
	bodyBytes, err := body.Marshal()
	require.NoError(t, err)
	auth := txtypes.AuthInfo{Fee: &txtypes.Fee{Amount: coins(t, fee), GasLimit: 200000, Granter: granter}}
	authBytes, err := auth.Marshal()
	require.NoError(t, err)
	raw := txtypes.TxRaw{BodyBytes: bodyBytes, AuthInfoBytes: authBytes, Signatures: [][]byte{[]byte("sig")}}
	out, err := raw.Marshal()
	require.NoError(t, err)
	return out
}

func TestDecodeTxFee(t *testing.T) {
	fee, granter, ok := decodeTxFee(protoTx(t, "a", "2500uatom", backTestGranter))
	require.True(t, ok)
	require.Equal(t, "2500uatom", fee.String())
	require.Equal(t, backTestGranter, granter)
	_, _, ok = decodeTxFee([]byte("not a transaction"))
	require.False(t, ok)
}

func fcEvent(typ, who, amount, mode string, base64Attrs bool) rpcEvent {
	key := attrReceiver
	if typ == eventCoinSpent {
		key = attrSpender
	}
	attrs := []rpcAttr{{Key: key, Value: who}, {Key: apiAmount, Value: amount}}
	if mode != "" {
		attrs = append(attrs, rpcAttr{Key: "mode", Value: mode})
	}
	if base64Attrs {
		for i, a := range attrs {
			attrs[i] = rpcAttr{Key: b64(a.Key), Value: b64(a.Value)}
		}
	}
	return rpcEvent{Type: typ, Attributes: attrs}
}

func TestCollectedFees(t *testing.T) {
	fc := feeCollectorAddress()
	require.Equal(t, "cosmos17xpfvakm2amg962yls6f84z3kell8c5lserqta", fc)

	// Old results: BeginBlock events, base64 attributes, mint before
	// distribution.
	b := readFeeCollectorBegin([]rpcEvent{
		fcEvent(eventCoinReceived, fc, testMintAmount, "", true),
		fcEvent(eventCoinSpent, backTestSigner, "5uatom", "", true),
		fcEvent(eventCoinSpent, fc, "1150uatom", "", true),
	}, nil)
	got, err := b.collectedFees()
	require.NoError(t, err)
	require.Equal(t, "150uatom", got.String())

	// Newer nodes: finalize events with a mode; EndBlock movements are not
	// the previous block's fees.
	b = readFeeCollectorBegin(nil, []rpcEvent{
		fcEvent(eventCoinReceived, fc, testMintAmount, "BeginBlock", false),
		fcEvent(eventCoinSpent, fc, "1150uatom", "BeginBlock", false),
		fcEvent(eventCoinReceived, fc, "7uatom", "EndBlock", false),
	})
	got, err = b.collectedFees()
	require.NoError(t, err)
	require.Equal(t, "150uatom", got.String())

	// Distribution before mint (Gaia v7 order) is not read as v6 fees.
	b = readFeeCollectorBegin(nil, []rpcEvent{
		fcEvent(eventCoinSpent, fc, "1150uatom", "BeginBlock", false),
		fcEvent(eventCoinReceived, fc, testMintAmount, "BeginBlock", false),
	})
	_, err = b.collectedFees()
	require.ErrorContains(t, err, "before receiving")

	b = readFeeCollectorBegin(nil, []rpcEvent{fcEvent(eventCoinReceived, fc, testMintAmount, "BeginBlock", false)})
	_, err = b.collectedFees()
	require.Error(t, err)
}

func TestResolvePreEventFees(t *testing.T) {
	txs := []nodeTx{{Code: 0}, {Code: 19}, {Code: 32}, {Code: 5}}
	raw := []rawTxInfo{
		{Fee: coins(t, "100uatom"), FeeDecoded: true},
		{Codespace: apiStaking, Fee: coins(t, "50uatom"), FeeDecoded: true},
		{Codespace: codespaceSDK, Fee: coins(t, "30uatom"), FeeDecoded: true},
		{Codespace: codespaceSDK, Fee: coins(t, "40uatom"), FeeDecoded: true},
	}
	charged, reason := resolvePreEventFees(txs, raw, coins(t, "190uatom"))
	require.Equal(t, "", reason)
	require.Equal(t, []bool{false, true, false, true}, charged)

	charged, reason = resolvePreEventFees(txs, raw, coins(t, "150uatom"))
	require.Equal(t, "subset", reason)
	require.Equal(t, []bool{false, true, false, false}, charged)

	// A wrong-sequence failure never pays, so 130 has no answer.
	charged, reason = resolvePreEventFees(txs, raw, coins(t, "130uatom"))
	require.Nil(t, charged)
	require.Equal(t, "no combination matches", reason)

	// Two failed transactions with the same fee, one charged: ambiguous.
	raw[3].Fee = coins(t, "50uatom")
	charged, reason = resolvePreEventFees(txs, raw, coins(t, "150uatom"))
	require.Nil(t, charged)
	require.Equal(t, "ambiguous combination", reason)

	charged, reason = resolvePreEventFees(txs, raw, coins(t, "90uatom"))
	require.Nil(t, charged)
	require.Equal(t, "collected less than the successful fees", reason)

	raw[1].FeeDecoded = false
	charged, reason = resolvePreEventFees(txs, raw, coins(t, "150uatom"))
	require.Nil(t, charged)
	require.Equal(t, "undecodable transaction", reason)
}

func TestSettlePreEventBlock(t *testing.T) {
	fc := feeCollectorAddress()
	next := readFeeCollectorBegin([]rpcEvent{
		fcEvent(eventCoinReceived, fc, testMintAmount, "", false),
		fcEvent(eventCoinSpent, fc, "1150uatom", "", false),
	}, nil)
	b := scannedBlock{
		txs: []nodeTx{{Hash: "AA"}, {Hash: "BB", Code: 19, Rejected: true}, {Hash: "CC", Code: 32, Rejected: true}},
		raw: []rawTxInfo{
			{Fee: coins(t, "100uatom"), FeeDecoded: true},
			{Codespace: apiStaking, Fee: coins(t, "50uatom"), FeeGranter: backTestGranter, FeeDecoded: true},
			{Codespace: codespaceSDK, Fee: coins(t, "30uatom"), FeeDecoded: true},
		},
	}
	st := settlePreEventBlock(9000002, b, next)
	require.Equal(t, 1, st.ResolvedBlocks)
	require.Equal(t, 1, st.FailedCharged)
	require.Equal(t, 1, st.FailedUncharged)
	require.Equal(t, nodeTx{Hash: "BB", Code: 19, Charged: "50uatom", Granter: backTestGranter}, b.txs[1])
	require.Equal(t, nodeTx{Hash: "CC", Code: 32, Rejected: true}, b.txs[2])

	// Unresolved: failed transactions keep no outcome and must be indexed.
	b.txs[1], b.txs[2] = nodeTx{Hash: "BB", Code: 19, Rejected: true}, nodeTx{Hash: "CC", Code: 32, Rejected: true}
	bad := readFeeCollectorBegin([]rpcEvent{
		fcEvent(eventCoinReceived, fc, testMintAmount, "", false),
		fcEvent(eventCoinSpent, fc, "1140uatom", "", false),
	}, nil)
	st = settlePreEventBlock(9000002, b, bad)
	require.Equal(t, 1, st.UnresolvedBlocks)
	require.Equal(t, []int64{9000002}, st.UnresolvedHeights)
	require.True(t, b.txs[1].ChargeUnknown)
	require.False(t, b.txs[1].Rejected)
	require.True(t, b.txs[2].Rejected, "a wrong sequence paid nothing whatever the block collected")
	require.False(t, b.txs[2].ChargeUnknown)
	index := []backTx{
		{Height: 9000002, Indexed: true, Hash: "aa", Messages: true, TxID: 1},
		{Height: 9000002, Indexed: true, Hash: "bb", Code: 19, TxID: 2},
	}
	got := scanBackBlock(9000002, b.txs, index)
	require.Empty(t, got.fees, "an unknown outcome is not recorded")
	require.Zero(t, got.chainTxs, "the wrong-sequence transaction need not be indexed")
	got = scanBackBlock(9000002, b.txs, index[:1])
	require.Equal(t, 2, got.chainTxs, "a failed transaction the index lacks is a hole while its charge is unknown")

	// A block without failed transactions checks the method itself.
	clean := scannedBlock{txs: []nodeTx{{Hash: "AA"}}, raw: []rawTxInfo{{Fee: coins(t, "100uatom"), FeeDecoded: true}}}
	st = settlePreEventBlock(9000001, clean, next)
	require.Equal(t, 1, st.CleanMismatch)
	require.Equal(t, []int64{9000001}, st.MismatchHeights)
}
