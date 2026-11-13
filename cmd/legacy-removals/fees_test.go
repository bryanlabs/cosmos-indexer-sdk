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
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

// The sequence moved once for every settled transaction and once for every
// open failure that paid. Open failures are decided only all together.
func TestFeeVerdictNeedsASequenceTheBlockExplains(t *testing.T) {
	for _, c := range []struct {
		before, after uint64
		settled, open int
		want          string
	}{
		{19, 19, 0, 1, "not charged"},
		{19, 20, 0, 1, "charged"},
		{19, 20, 1, 1, "not charged"},
		{19, 21, 1, 1, "charged"},
		{19, 21, 0, 2, "charged"},
		{19, 19, 0, 2, "not charged"},
		{0, 3, 3, 1, "not charged"},
	} {
		got, err := feeVerdict(c.before, c.after, c.settled, c.open)
		require.NoError(t, err)
		require.Equal(t, c.want, got)
	}
	for _, c := range []struct {
		before, after uint64
		settled, open int
	}{{20, 19, 0, 1}, {19, 19, 1, 1}, {19, 22, 1, 1}, {19, 20, 0, 2}, {19, 20, 1, 0}, {19, 21, 0, 3}} {
		_, err := feeVerdict(c.before, c.after, c.settled, c.open)
		require.Error(t, err)
	}
}

// Only the transactions the account signed count, by what is known of each.
func TestFeeCountsFollowsOneAccountThroughABlock(t *testing.T) {
	payer, other := sdk.AccAddress(bytes.Repeat([]byte{1}, 20)), sdk.AccAddress(bytes.Repeat([]byte{2}, 20))
	tx := func(charge string, signers ...sdk.AccAddress) core.LegacyInclusion {
		return core.LegacyInclusion{Tx: &legacy.Tx{Signers: signers, SignersComplete: true}, Fee: core.LegacyFeeEvidence{Charge: charge}}
	}
	settled, open, err := feeCounts([]core.LegacyInclusion{
		tx("charged", payer),
		tx("unknown", payer),
		tx("charged", other),
		tx("unknown", other),
		tx("not charged", payer),
		tx("charged", other, payer),
		tx("unknown", other, payer),
	}, payer)
	require.NoError(t, err)
	require.Equal(t, 2, settled)
	require.Equal(t, []int{1, 6}, open)
	_, _, err = feeCounts([]core.LegacyInclusion{{Tx: &legacy.Tx{Signers: []sdk.AccAddress{payer}}}}, payer)
	require.Error(t, err, "signers that are not all known cannot be counted")
	_, _, err = feeCounts([]core.LegacyInclusion{{}}, payer)
	require.Error(t, err)
	_, _, err = feeCounts([]core.LegacyInclusion{tx("perhaps", payer)}, payer)
	require.Error(t, err)
}

// An archive is believed about a sequence only when it returns the record of
// the account asked for at the height asked for.
func TestSequenceReadsTheAccountRecordOfTheHeight(t *testing.T) {
	type record struct {
		Address       []byte
		Coins         []struct{ Denom, Amount string }
		PubKey        []byte
		AccountNumber uint64
		Sequence      uint64
	}
	cdc := amino.NewCodec()
	cdc.RegisterConcrete(record{}, "cosmos-sdk/Account", nil)
	payer, other := sdk.AccAddress(bytes.Repeat([]byte{1}, 20)), sdk.AccAddress(bytes.Repeat([]byte{2}, 20))
	key := "/store/acc/key 01" + hex.EncodeToString(payer)
	state := map[string][]byte{
		"8 " + key:  cdc.MustMarshalBinaryBare(record{Address: payer, AccountNumber: 5, Sequence: 19}),
		"9 " + key:  cdc.MustMarshalBinaryBare(record{Address: payer, AccountNumber: 5, Sequence: 20}),
		"10 " + key: cdc.MustMarshalBinaryBare(record{Address: other, AccountNumber: 6, Sequence: 20}),
	}
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Params struct{ Path, Data, Height string }
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&call))
		asked = append(asked, call.Params.Height+" "+call.Params.Path+" "+call.Params.Data)
		if call.Params.Height == "12" {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)
			return
		}
		value := base64.StdEncoding.EncodeToString(state[call.Params.Height+" "+call.Params.Path+" "+call.Params.Data])
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"value":%q,"height":%q}}}`, value, call.Params.Height)
	}))
	defer server.Close()
	a := &archive{name: "1", url: server.URL, client: server.Client(), evidence: t.TempDir()}
	ctx := context.Background()
	got, err := a.sequence(ctx, "fee-9-1-sequence-before", payer, 8)
	require.NoError(t, err)
	require.Equal(t, uint64(19), got)
	got, err = a.sequence(ctx, "fee-9-1-sequence-after", payer, 9)
	require.NoError(t, err)
	require.Equal(t, uint64(20), got)
	require.Equal(t, []string{"8 " + key, "9 " + key}, asked)
	_, err = a.sequence(ctx, "x", payer, 10)
	require.ErrorContains(t, err, "not the one asked for")
	_, err = a.sequence(ctx, "x", payer, 11)
	require.ErrorContains(t, err, "no account record")
	_, err = a.sequence(ctx, "x", payer, 12)
	require.ErrorIs(t, err, errNotServed)
}
