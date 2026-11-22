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

// Results of failed transactions as SDK 0.42 and 0.44 logged them.
const (
	logMsgFailed  = "failed to execute message; message index: 0: 5uatom is smaller than 40uatom: insufficient funds"
	logWrongSeq   = "account sequence mismatch, expected 7, got 6: incorrect account sequence"
	logOutOfGas   = "out of gas in location: WriteFlat; gasWanted: 300, gasUsed: 1200: out of gas"
	logAnteNoFund = "0uatom is smaller than 40uatom: insufficient funds: insufficient funds"
)

// failedResult is a failed transaction's result as blockScan keeps it.
func failedResult(code uint32, codespace, log string, gasWanted int64, fee string) *failedTxResult {
	return &failedTxResult{Code: code, Codespace: codespace, Log: log, GasWanted: gasWanted, TxBytes: 200, Signers: 1, Fee: fee, FeeRead: true}
}

// Before Gaia v7 a failed transaction's fee is settled from where its result
// says it failed: a message failure paid the declared fee, a rejection before
// the messages paid nothing, and an out-of-gas panic the gas limit cannot
// place is left unknown.
func TestSettlePreEventBlock(t *testing.T) {
	fresh := func() scannedBlock {
		return scannedBlock{
			txs: []nodeTx{
				{Hash: "AA"},
				{Hash: "BB", Code: 5, Rejected: true},
				{Hash: "CC", Code: 32, Rejected: true},
				{Hash: "DD", Code: 5, Rejected: true},
				{Hash: "EE", Code: 11, Rejected: true},
				{Hash: "FF", Code: 11, Rejected: true},
			},
			raw: []rawTxInfo{
				{},
				{Codespace: codespaceSDK, FeeGranter: backTestGranter, FeeDecoded: true, Failed: failedResult(5, codespaceSDK, logMsgFailed, 200000, "50uatom")},
				{Codespace: codespaceSDK, Failed: failedResult(32, codespaceSDK, logWrongSeq, 200000, "30uatom")},
				{Codespace: codespaceSDK, Failed: failedResult(5, codespaceSDK, logAnteNoFund, 200000, "40uatom")},
				{Codespace: codespaceSDK, Failed: failedResult(11, codespaceSDK, logOutOfGas, 300000, "20uatom")},
				{Codespace: codespaceSDK, Failed: failedResult(11, codespaceSDK, logOutOfGas, 300, "20uatom")},
			},
		}
	}
	// Below Gaia v6 the result settles the fee the same way down to
	// cosmoshub-4's first block, and a granter in the bytes paid nothing.
	for _, h := range []int64{9000002, hubCoinEventsFrom - 2, hubGenesisHeight} {
		granter := backTestGranter
		if h < hubCoinEventsFrom {
			granter = ""
		}
		b := fresh()
		st := settlePreEventBlock(h, b)
		require.Equal(t, 1, st.BlocksWithFailed)
		require.Equal(t, 1, st.UnresolvedBlocks, "the out-of-gas panic below the ante limit is unknown")
		require.Equal(t, []int64{h}, st.UnresolvedHeights)
		require.Equal(t, map[string]int{preEventLogUnsettled: 1}, st.UnresolvedReasons)
		require.Equal(t, 2, st.FailedCharged)
		require.Equal(t, 2, st.FailedUncharged)
		require.Equal(t, 1, st.FailedUnknown)
		require.Equal(t, nodeTx{Hash: "BB", Code: 5, Charged: "50uatom", ChargeKnown: true, Granter: granter}, b.txs[1], "a message failure paid its fee")
		require.Equal(t, nodeTx{Hash: "CC", Code: 32, Rejected: true, ChargeKnown: true}, b.txs[2], "a wrong sequence paid nothing")
		require.Equal(t, nodeTx{Hash: "DD", Code: 5, Rejected: true, ChargeKnown: true}, b.txs[3], "a fee above the balance paid nothing")
		require.Equal(t, nodeTx{Hash: "EE", Code: 11, Charged: "20uatom", ChargeKnown: true}, b.txs[4], "out of gas past the ante handler paid")
		require.Equal(t, nodeTx{Hash: "FF", Code: 11}, b.txs[5], "out of gas below the ante limit is unknown")

		index := []backTx{
			{Height: h, Indexed: true, Hash: "aa", Messages: true, TxID: 1},
			{Height: h, Indexed: true, Hash: "bb", Code: 5, TxID: 2},
			{Height: h, Indexed: true, Hash: "ee", Code: 11, TxID: 3},
			{Height: h, Indexed: true, Hash: "ff", Code: 11, TxID: 4},
		}
		got := scanBackBlock(h, b.txs, index)
		require.Equal(t, []FailedTxFee{{TxID: 2, Charged: "50uatom"}, {TxID: 3, Charged: "20uatom"}}, got.fees, "an unknown outcome is not recorded")
		if granter == "" {
			require.Empty(t, got.grants, "no fee grant before Gaia v6")
		} else {
			require.Equal(t, []FeeGrantUse{{TxID: 2, Granter: granter}}, got.grants)
		}
		require.Zero(t, got.chainTxs, "the rejected transactions need not be indexed")
		got = scanBackBlock(h, b.txs, index[:3])
		require.Equal(t, 4, got.chainTxs, "a failed transaction the index lacks is a hole while its charge is unknown")
	}

	// A block whose failed transactions all settle is resolved.
	b := fresh()
	b.txs, b.raw = b.txs[:5], b.raw[:5]
	st := settlePreEventBlock(9000001, b)
	require.Equal(t, 1, st.ResolvedBlocks)
	require.Zero(t, st.UnresolvedBlocks)

	// A block without failed transactions has nothing to settle.
	st = settlePreEventBlock(9000000, scannedBlock{txs: []nodeTx{{Hash: "AA"}}, raw: []rawTxInfo{{}}})
	require.Equal(t, preEventStats{Blocks: 1}, st)
}
