package observeapp

import (
	"bytes"
	"testing"
	"time"

	upstream "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

func TestAntePanicAndPreAnteRejectionHaveExplicitAbortedScope(t *testing.T) {
	for _, preAnte := range []bool{false, true} {
		app, _, key, genesis := fixture(t, false)
		original := upstream.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0)
		original.InitChain(abci.RequestInitChain{AppStateBytes: genesis, ChainId: "observe-test"})
		var out bytes.Buffer
		app.SetRecorder(&out)
		req := abci.RequestBeginBlock{Header: abci.Header{Height: 1, ChainID: "observe-test", Time: time.Unix(1500000001, 0)}}
		app.BeginBlock(req)
		original.BeginBlock(req)
		a := sdk.AccAddress(key.PubKey().Address())
		b := sdk.AccAddress(bytes.Repeat([]byte{9}, 20))
		amount := coins(1)
		gas := uint64(1)
		if preAnte {
			amount = sdk.Coins{}
			gas = 200000
		}
		msgs := []sdk.Msg{bank.NewMsgSend(a, b, amount)}
		fee := auth.NewStdFee(gas, coins(7))
		sig, e := key.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, msgs, ""))
		must(t, e == nil, "sign rejection fixture")
		raw, e := app.cdc.MarshalBinaryLengthPrefixed(auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: key.PubKey(), Signature: sig}}, ""))
		must(t, e == nil, "encode rejection fixture")
		x, y := app.DeliverTx(raw), original.DeliverTx(raw)
		must(t, x.Code != 0 && x.Code == y.Code && x.GasUsed == y.GasUsed, "rejection code and gas equivalence")
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		original.EndBlock(abci.RequestEndBlock{Height: 1})
		must(t, app.RecorderError() == nil, "aborted scope reconciliation")
		must(t, bytes.Equal(app.Commit().Data, original.Commit().Data), "rejection original hash")
		count := 0
		for _, r := range rows(t, &out) {
			if r.Kind == "accounting" && r.Phase == "ante" {
				count++
				must(t, r.Status == "aborted", "ante failure is aborted")
				if preAnte {
					must(t, r.Label == "ante_not_invoked", "explicit pre-ante rejection")
				}
			}
			if r.Kind == "movement" {
				must(t, r.Status == "aborted", "failed ante never committed")
			}
		}
		must(t, count == 1, "exact ante failure marker")
	}
}
