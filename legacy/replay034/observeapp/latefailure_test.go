package observeapp

import (
	"bytes"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	"testing"
)

func TestZeroNetMessagesWithPostWriteGasFailureStillStop(t *testing.T) {
	app, _, first, _ := fixture(t, false, 1)
	second := secp256k1.GenPrivKeySecp256k1([]byte("second zero net fixture signer"))
	a, b := sdk.AccAddress(first.PubKey().Address()), sdk.AccAddress(second.PubKey().Address())
	// Establish a second empty account before the observed block baseline.
	ctx := app.NewContext(false, abci.Header{})
	account := app.accountKeeper.NewAccountWithAddress(ctx, b)
	app.accountKeeper.SetAccount(ctx, account)
	msgs := []sdk.Msg{bank.NewMsgSend(a, b, coins(100)), bank.NewMsgSend(b, a, coins(100))}
	fee := auth.NewStdFee(200000, coins(7))
	one, err := first.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, msgs, ""))
	must(t, err == nil, "sign first zero net fixture")
	two, err := second.Sign(auth.StdSignBytes("observe-test", account.GetAccountNumber(), 0, fee, msgs, ""))
	must(t, err == nil, "sign second zero net fixture")
	tx := auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: first.PubKey(), Signature: one}, {PubKey: second.PubKey(), Signature: two}}, "")
	raw, err := app.cdc.MarshalBinaryLengthPrefixed(tx)
	must(t, err == nil, "encode zero net fixture")
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	before := app.snapshot(app.NewContext(false, abci.Header{}))
	res := app.DeliverTx(raw)
	must(t, res.Code != 0 && app.observer.handlers == 2 && !app.observer.handlerFailure, "both messages succeeded before late failure")
	expected := apply(before, []Record{{Kind: "movement", Address: a.String(), Denom: "stake", Amount: "-7"}})
	must(t, equal(expected, app.snapshot(app.NewContext(false, abci.Header{}))), "message movements exactly cancel")
	must(t, app.RecorderError() != nil, "zero net late failure must fail closed")
	app.Commit()
	must(t, out.Len() == 0 && app.LastBlockHeight() == 0, "no false aborted or committed evidence escapes")
}
