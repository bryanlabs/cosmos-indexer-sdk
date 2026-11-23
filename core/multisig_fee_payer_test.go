package core

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/multisig"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptoTypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"
)

// multisigSend is a 2-of-3 multisig account sending 5uatom with a 1000uatom
// fee, built from fixed keys so its processed rows are the same on every run.
func multisigSend(t *testing.T) (*client.ChainClient, *txtypes.TxBody, *txtypes.AuthInfo, []cryptoTypes.PubKey, sdk.AccAddress) {
	c := client.MakeCodecConfig()
	cryptocodec.RegisterInterfaces(c.InterfaceRegistry)
	bank.RegisterInterfaces(c.InterfaceRegistry)
	var members []cryptoTypes.PubKey
	for _, secret := range []string{"member-0", "member-1", "member-2"} {
		members = append(members, secp256k1.GenPrivKeyFromSecret([]byte(secret)).PubKey())
	}
	msig := multisig.NewLegacyAminoPubKey(2, members)
	account := sdk.AccAddress(msig.Address())
	to := sdk.AccAddress(secp256k1.GenPrivKeyFromSecret([]byte("recipient")).PubKey().Address())
	send, err := codectypes.NewAnyWithValue(&bank.MsgSend{FromAddress: account.String(), ToAddress: to.String(), Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 5))})
	require.NoError(t, err)
	pk, err := codectypes.NewAnyWithValue(msig)
	require.NoError(t, err)
	body := &txtypes.TxBody{Messages: []*codectypes.Any{send}}
	auth := &txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{PublicKey: pk}},
		Fee:         &txtypes.Fee{Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 1000))},
	}
	return &client.ChainClient{Codec: c}, body, auth, members, account
}

func processMultisigSend(t *testing.T, path string) []dbTypes.TxDBWrapper {
	cl, body, auth, _, _ := multisigSend(t)
	now := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	if path == testRPCPath {
		wrappers, _, err := ProcessRPCTXs(&config.IndexConfig{}, nil, cl, nil, nil, &txtypes.GetTxsEventResponse{Txs: []*txtypes.Tx{{Body: body, AuthInfo: auth}}, TxResponses: []*sdk.TxResponse{{Height: 1, TxHash: "ABC", Timestamp: now.Format(time.RFC3339)}}}, nil)
		require.NoError(t, err)
		return wrappers
	}
	bb, err := body.Marshal()
	require.NoError(t, err)
	aa, err := auth.Marshal()
	require.NoError(t, err)
	raw, err := (&txtypes.TxRaw{BodyBytes: bb, AuthInfoBytes: aa}).Marshal()
	require.NoError(t, err)
	wrappers, _, err := ProcessRPCBlockByHeightTXs(&config.IndexConfig{}, nil, cl, nil, nil, &ctypes.ResultBlock{Block: &tm.Block{Header: tm.Header{Height: 1, Time: now}, Data: tm.Data{Txs: tm.Txs{raw}}}}, &rpc.CustomBlockResults{Height: 1, TxsResults: []*abci.ResponseDeliverTx{{Log: "[]"}}}, nil)
	require.NoError(t, err)
	return wrappers
}

// Everything a multisig tx writes besides its fee rows (the tx, its signer
// list and order, its messages) must match the output from before the fee
// payer fix. The golden files were written on the parent commit; set
// UPDATE_GOLDEN=1 to rewrite them after a deliberate change.
func TestMultisigNonFeeRowsUnchanged(t *testing.T) {
	for _, path := range []string{"block", testRPCPath} {
		t.Run(path, func(t *testing.T) {
			wrappers := processMultisigSend(t, path)
			require.Len(t, wrappers, 1)
			wrappers[0].Tx.Fees = nil
			got, err := json.MarshalIndent(wrappers, "", "  ")
			require.NoError(t, err)
			golden := "testdata/multisig_nonfee_" + path + ".golden"
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o600))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "missing golden file %s (run with UPDATE_GOLDEN=1)", golden)
			require.Equal(t, string(want), string(got)+"\n")
		})
	}
}

// A multisig account pays its own fee. ProcessSigners lists the member keys
// before the account, so ProcessFees used to charge member key 0.
func TestMultisigFeePayerIsTheMultisigAccount(t *testing.T) {
	cl, _, auth, members, account := multisigSend(t)
	for _, path := range []string{"block", testRPCPath} {
		wrappers := processMultisigSend(t, path)
		require.Len(t, wrappers[0].Tx.Fees, 1, path)
		require.Equal(t, account.String(), wrappers[0].Tx.Fees[0].PayerAddress.Address, path)
	}

	signers, err := ProcessSigners(cl, auth, []sdk.AccAddress{account})
	require.NoError(t, err)
	var want []models.Address
	for _, m := range members {
		want = append(want, models.Address{Address: sdk.AccAddress(m.Address()).String()})
	}
	require.Equal(t, append(want, models.Address{Address: account.String()}), signers)
	feeSigners, err := FeeSigners(cl, auth, signers)
	require.NoError(t, err)
	require.Equal(t, append([]models.Address{{Address: account.String()}}, want...), feeSigners)

	// A message the codec cannot decode names no signer, so the account is
	// missing from the stored list; the fee still goes to it.
	feeSigners, err = FeeSigners(cl, auth, want)
	require.NoError(t, err)
	require.Equal(t, account.String(), feeSigners[0].Address)
	require.Len(t, want, len(members))

	single := secp256k1.GenPrivKeyFromSecret([]byte("single")).PubKey()
	spk, err := codectypes.NewAnyWithValue(single)
	require.NoError(t, err)
	auth.SignerInfos = []*txtypes.SignerInfo{{PublicKey: spk}}
	signers, err = ProcessSigners(cl, auth, []sdk.AccAddress{sdk.AccAddress(single.Address())})
	require.NoError(t, err)
	feeSigners, err = FeeSigners(cl, auth, signers)
	require.NoError(t, err)
	require.Equal(t, signers, feeSigners)
	fees, err := ProcessFees(nil, *auth, feeSigners)
	require.NoError(t, err)
	require.Equal(t, sdk.AccAddress(single.Address()).String(), fees[0].PayerAddress.Address)
}
