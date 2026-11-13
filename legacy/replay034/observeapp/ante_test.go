package observeapp

import (
	"bytes"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
)

func TestFailedAnteDiscardsAlreadyWrittenPayerFee(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	payer := sdk.AccAddress(key.PubKey().Address())
	missingKey := secp256k1.GenPrivKeySecp256k1([]byte("absent second fixture signer"))
	missing := sdk.AccAddress(missingKey.PubKey().Address())
	destination := sdk.AccAddress(bytes.Repeat([]byte{8}, 20))
	msgs := []sdk.Msg{bank.NewMsgSend(payer, destination, coins(1)), bank.NewMsgSend(missing, destination, coins(1))}
	fee := auth.NewStdFee(200000, coins(7))
	sig, err := key.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, msgs, ""))
	must(t, err == nil, "sign ante fixture")
	tx := auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: key.PubKey(), Signature: sig}, {PubKey: missingKey.PubKey(), Signature: []byte{1}}}, "")
	raw, err := app.cdc.MarshalBinaryLengthPrefixed(tx)
	must(t, err == nil, "encode ante fixture")
	before := app.snapshot(app.NewContext(false, abci.Header{}))
	res := app.DeliverTx(raw)
	must(t, res.Code != 0, "missing second signer aborts ante")
	must(t, equal(before, app.snapshot(app.NewContext(false, abci.Header{}))), "ante cache fee rolled back")
	must(t, app.RecorderError() == nil, "partial ante accounting")
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	attempted := false
	for _, r := range rows(t, &out) {
		if r.Kind == "movement" {
			must(t, r.Label == "ante_fee" && r.Status == "aborted", "cached ante fee only attempted")
			attempted = true
		}
	}
	must(t, attempted, "observe cached direct fee before ante abort")
}
