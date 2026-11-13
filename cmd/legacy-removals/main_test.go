package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

// stored builds the bytes the chain kept for an accumulated commission.
func stored(pairs ...string) []byte {
	var list []byte
	for i := 0; i < len(pairs); i += 2 {
		coin := append([]byte{0x0a, byte(len(pairs[i]))}, pairs[i]...)
		coin = append(append(coin, 0x12, byte(len(pairs[i+1]))), pairs[i+1]...)
		list = append(append(list, 0x0a, byte(len(coin))), coin...)
	}
	return append([]byte{byte(len(list))}, list...)
}

func TestWholeCommissionKeepsOnlyWholeCoins(t *testing.T) {
	for amount, want := range map[string]string{"8999999999999999999": "8", "999999999999999999": "0", "12000000000000000000": "12"} {
		whole, err := wholeCommission(stored("uatom", amount))
		require.NoError(t, err)
		require.Equal(t, want, whole.String())
	}
	whole, err := wholeCommission(nil)
	require.NoError(t, err)
	require.Zero(t, whole.Sign())
	for _, bad := range [][]byte{stored("stake", "8000000000000000000"), stored("uatom", "1", "uatom", "2"), stored("uatom", "x"), {5, 1}, bytes.Repeat([]byte{0xff}, 4)} {
		_, err := wholeCommission(bad)
		require.Error(t, err)
	}
}

// An archive is believed about a missing record only when it has the height,
// had the record the block before, and says what the commission was.
func TestVerdictNeedsAnArchiveThatHasTheHeight(t *testing.T) {
	validator := sdk.ValAddress(bytes.Repeat([]byte{2}, 20))
	record, commission, power := "21"+hex.EncodeToString(validator), "07"+hex.EncodeToString(validator), "12"
	run := func(state map[string][]byte, refuse bool) (string, error) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call struct {
				Params struct{ Path, Data, Height string }
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&call))
			if refuse {
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)
				return
			}
			value := base64.StdEncoding.EncodeToString(state[call.Params.Height+" "+call.Params.Path+" "+call.Params.Data])
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"value":%q,"height":%q}}}`, value, call.Params.Height)
		}))
		defer server.Close()
		a := &archive{name: "1", url: server.URL, client: server.Client(), evidence: t.TempDir()}
		return a.verdict(context.Background(), "9-0-0", validator, 9)
	}
	staking, distribution := "/store/staking/key ", "/store/distribution/key "
	got, err := run(map[string][]byte{"9 " + staking + record: {1}}, false)
	require.NoError(t, err)
	require.Equal(t, "not removed", got)
	gone := map[string][]byte{"9 " + staking + power: {1}, "8 " + staking + record: {1}, "8 " + distribution + commission: stored("uatom", "8500000000000000000")}
	got, err = run(gone, false)
	require.NoError(t, err)
	require.Equal(t, "removed with commission 8uatom", got)
	delete(gone, "8 "+distribution+commission)
	got, err = run(gone, false)
	require.NoError(t, err)
	require.Equal(t, "removed with no commission", got)
	// No total power after the block: the archive does not have the height.
	_, err = run(map[string][]byte{"8 " + staking + record: {1}}, false)
	require.ErrorIs(t, err, errNotServed)
	_, err = run(nil, true)
	require.ErrorIs(t, err, errNotServed)
	// The record was never there: the candidate is wrong, not a removal.
	_, err = run(map[string][]byte{"9 " + staking + power: {1}}, false)
	require.ErrorContains(t, err, "no record before the block either")
}

// The state around a block speaks for the last message that leaves a
// validator, and for none when the block also creates that validator.
func TestDecidingNamesTheLastLeaveOfEachValidatorInABlock(t *testing.T) {
	a, b, c := sdk.ValAddress(bytes.Repeat([]byte{1}, 20)).String(), sdk.ValAddress(bytes.Repeat([]byte{2}, 20)).String(), sdk.ValAddress(bytes.Repeat([]byte{3}, 20)).String()
	leave := func(validator string) legacy.Message {
		return legacy.Message{Derived: &stakingtypes.MsgUndelegate{ValidatorAddress: validator}}
	}
	move := func(from, to string) legacy.Message {
		return legacy.Message{Derived: &stakingtypes.MsgBeginRedelegate{ValidatorSrcAddress: from, ValidatorDstAddress: to}}
	}
	create := func(validator string) legacy.Message {
		return legacy.Message{Derived: &stakingtypes.MsgCreateValidator{ValidatorAddress: validator}}
	}
	tx := func(code uint32, messages ...legacy.Message) core.LegacyInclusion {
		return core.LegacyInclusion{Tx: &legacy.Tx{Messages: messages}, Result: &abci.ResponseDeliverTx{Code: code}}
	}
	speaks, created := deciding([]core.LegacyInclusion{
		tx(0, leave(a), move(b, a)),
		tx(0, leave(a), leave(c)),
		// A failed transaction left nothing and created nothing.
		tx(5, leave(a), leave(b), create(a)),
		{},
		tx(0, create(c)),
	})
	require.Equal(t, map[[2]int64]string{{1, 0}: a, {0, 1}: b}, speaks)
	require.Equal(t, map[string]bool{c: true}, created)
}
