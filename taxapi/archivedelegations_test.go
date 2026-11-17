package taxapi

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

func TestAnchorReadsStoreDelegationsWhenBondDenomIsLost(t *testing.T) {
	const height = 9041674
	del, err := bech32.ConvertAndEncode("cosmos", []byte("delegator-address-20b"))
	require.NoError(t, err)
	other, err := bech32.ConvertAndEncode("cosmos", []byte("someone-else-address"))
	require.NoError(t, err)
	valA, err := bech32.ConvertAndEncode("cosmosvaloper", []byte("validator-a-address!"))
	require.NoError(t, err)
	valB, err := bech32.ConvertAndEncode("cosmosvaloper", []byte("validator-b-address!"))
	require.NoError(t, err)

	// Validator A has been slashed, so its shares are worth less than one
	// token each and the delegation query truncates.
	vals := []stakingtypes.Validator{
		{OperatorAddress: valA, Tokens: math.NewInt(999_999_999), DelegatorShares: math.LegacyMustNewDecFromStr("1000000000.123456789012345678")},
		{OperatorAddress: valB, Tokens: math.NewInt(5_000_000), DelegatorShares: math.LegacyMustNewDecFromStr("5000000")},
	}
	dels := []stakingtypes.Delegation{
		{DelegatorAddress: del, ValidatorAddress: valA, Shares: math.LegacyMustNewDecFromStr("123456.789")},
		{DelegatorAddress: del, ValidatorAddress: valB, Shares: math.LegacyMustNewDecFromStr("42")},
	}
	// stored is the store's delegation records by validator.
	stored := map[string]stakingtypes.Delegation{valA: dels[0], valB: dels[1]}
	answerAt := "9041674"
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "9041674", r.Header.Get(heightHeader))
		switch {
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/v1beta1/balances/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"balances": []any{}, incPageKey: map[string]any{}})
		case strings.HasPrefix(r.URL.Path, "/cosmos/staking/v1beta1/delegations/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":13, "message":"invalid denom: ", "details":[]}`))
		case strings.HasSuffix(r.URL.Path, "/validators"):
			var out []any
			for _, v := range vals {
				out = append(out, map[string]any{"operator_address": v.OperatorAddress, "tokens": v.Tokens.String(), "delegator_shares": v.DelegatorShares.String()})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"validators": out, incPageKey: map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/unbonding_delegations"):
			_ = json.NewEncoder(w).Encode(map[string]any{incUnbondingKey: []any{}, incPageKey: map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer rest.Close()
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/abci_query", r.URL.Path)
		require.Equal(t, `"/store/staking/key"`, r.URL.Query().Get("path"))
		require.Equal(t, "9041674", r.URL.Query().Get(rpcHeightParam))
		_, d, err := bech32.DecodeAndConvert(del)
		require.NoError(t, err)
		value := ""
		for v, rec := range stored {
			_, vb, err := bech32.DecodeAndConvert(v)
			require.NoError(t, err)
			if r.URL.Query().Get("data") == fmt.Sprintf("0x31%02x%s%02x%s", len(d), hex.EncodeToString(d), len(vb), hex.EncodeToString(vb)) {
				bz, err := rec.Marshal()
				require.NoError(t, err)
				value = base64.StdEncoding.EncodeToString(bz)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"response": map[string]any{incCodeKey: 0, "value": value, rpcHeightParam: answerAt}}})
	}))
	defer rpc.Close()

	node, err := NewArchiveLedgerNode(t.TempDir(), rest.URL)
	require.NoError(t, err)
	_, err = node.anchor(del, height)
	require.ErrorContains(t, err, "no archive RPC")
	require.NoError(t, node.UseArchiveRPC(rpc.URL))

	a, err := node.anchor(del, height)
	require.NoError(t, err)
	for i, d := range dels {
		want := vals[i].TokensFromShares(d.Shares).TruncateInt().BigInt()
		require.Zero(t, want.Cmp(a.Delegations[d.ValidatorAddress]), d.ValidatorAddress)
		require.Equal(t, d.Shares.String(), a.Shares[d.ValidatorAddress].StringFixed(18))
	}
	require.Equal(t, "123456", a.Delegations[valA].String())
	require.Equal(t, "42", a.Delegations[valB].String())

	answerAt = "9041675"
	_, err = node.anchor(del, height)
	require.ErrorContains(t, err, "not 9041674")

	answerAt = "9041674"
	stored[valB] = stakingtypes.Delegation{DelegatorAddress: other, ValidatorAddress: valB, Shares: math.LegacyOneDec()}
	_, err = node.anchor(del, height)
	require.ErrorContains(t, err, "names another delegator")

	delete(stored, valB)
	_, err = node.anchor(del, height)
	require.ErrorContains(t, err, "no delegation record")
}
