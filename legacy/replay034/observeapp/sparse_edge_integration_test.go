package observeapp

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	bam "github.com/cosmos/cosmos-sdk/baseapp"
	upstream "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

func TestSparseCMSOverrideRejectedAndModeChangeLatched(t *testing.T) {
	func() {
		refused := false
		defer func() {
			refused = recover() != nil
			must(t, refused, "unsafe CMS option must be rejected")
		}()
		NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0, func(b *bam.BaseApp) {
			b.SetCMS(store.NewCommitMultiStore(dbm.NewMemDB()))
		})
	}()
	app := NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0, bam.SetPruning(st.PruneNothing))
	must(t, app != nil && !app.observer.sparse, "configuration option preserves full default")
	app, _, _, _ = fixture(t, false)
	app.SetSparseAccounting(true)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	app.SetSparseAccounting(false)
	app.Commit()
	must(t, app.RecorderError() != nil && app.LastBlockHeight() == 0 && out.Len() == 0, "active mode change fails before commit")
}

func TestSparseAllDenominationsAndAccountLifecycle(t *testing.T) {
	p := newSparsePair(t, false)
	p.record()
	p.begin(t, 1)
	p.end(t, 1)
	address := sdk.AccAddress(bytes.Repeat([]byte{41}, 20))
	for _, app := range []*GaiaApp{p.full, p.sparse} {
		ctx := app.NewContext(false, abci.Header{})
		expect := func(denom, amount string) {
			app.observer.pending = append(app.observer.pending, Record{Kind: "movement", Address: address.String(), Denom: denom, Amount: amount})
		}
		app.phase(ctx, "synthetic_create", func() {
			a := app.accountKeeper.NewAccountWithAddress(ctx, address)
			must(t, a.SetCoins(sdk.NewCoins(sdk.NewInt64Coin("other", 5), sdk.NewInt64Coin("stake", 3))) == nil, "set synthetic denominations")
			app.accountKeeper.SetAccount(ctx, a)
			expect("other", "5")
			expect("stake", "3")
		})
		app.phase(ctx, "synthetic_remove_denom", func() {
			a := app.accountKeeper.GetAccount(ctx, address)
			must(t, a.SetCoins(coins(3)) == nil, "remove entire denomination")
			app.accountKeeper.SetAccount(ctx, a)
			expect("other", "-5")
		})
		app.phase(ctx, "synthetic_delete", func() {
			app.accountKeeper.RemoveAccount(ctx, app.accountKeeper.GetAccount(ctx, address))
			expect("stake", "-3")
		})
		app.phase(ctx, "synthetic_recreate", func() {
			app.accountKeeper.SetAccount(ctx, app.accountKeeper.NewAccountWithAddress(ctx, address))
		})
	}
	p.commit(t, false)
	must(t, p.sparse.observer.fullSnapshots == 0, "all-denomination lifecycle stays sparse")
}

func TestSparsePartialAnteWriteIsAttemptedOnly(t *testing.T) {
	p := newSparsePair(t, false)
	p.record()
	p.begin(t, 1)
	payer := sdk.AccAddress(p.key.PubKey().Address())
	second := secp256k1.GenPrivKeySecp256k1([]byte("missing sparse second signer"))
	receiver := sdk.AccAddress(bytes.Repeat([]byte{42}, 20))
	msgs := []sdk.Msg{bank.NewMsgSend(payer, receiver, coins(1)), bank.NewMsgSend(sdk.AccAddress(second.PubKey().Address()), receiver, coins(1))}
	fee := auth.NewStdFee(200000, coins(7))
	sig, err := p.key.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, msgs, ""))
	must(t, err == nil, "sign partial ante fixture")
	raw, err := p.full.cdc.MarshalBinaryLengthPrefixed(auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: p.key.PubKey(), Signature: sig}, {PubKey: second.PubKey(), Signature: []byte{1}}}, ""))
	must(t, err == nil, "encode partial ante fixture")
	must(t, p.deliver(t, raw).Code != 0, "second signer aborts after payer write")
	p.end(t, 1)
	p.commit(t, false)
	found := false
	for _, r := range rows(t, &p.sparseOut) {
		if r.Kind == "movement" {
			must(t, r.Label == "ante_fee" && r.Status == "aborted", "partial ante fee is attempted only")
			found = true
		}
	}
	must(t, found, "partial payer write was observed")
}

func TestSparseNestedCacheAcceptanceMatrix(t *testing.T) {
	for _, mode := range []string{"discard", "flush", "inner_flush_outer_discard"} {
		t.Run(mode, func(t *testing.T) {
			p := newSparsePair(t, false)
			p.record()
			p.begin(t, 1)
			p.end(t, 1)
			for _, app := range []*GaiaApp{p.full, p.sparse} {
				ctx := app.NewContext(false, abci.Header{})
				owner := sdk.AccAddress(p.key.PubKey().Address())
				app.phase(ctx, "synthetic_nested", func() {
					child, flush := ctx.CacheContext()
					if mode == "inner_flush_outer_discard" {
						inner, innerFlush := child.CacheContext()
						a := app.accountKeeper.GetAccount(inner, owner)
						must(t, a.SetCoins(coins(1)) == nil, "prepare discarded inner mutation")
						app.accountKeeper.SetAccount(inner, a)
						innerFlush()
						return
					}
					a := app.accountKeeper.GetAccount(child, owner)
					must(t, a.SetCoins(coins(1)) == nil, "prepare child mutation")
					app.accountKeeper.SetAccount(child, a)
					if mode == "flush" {
						flush()
					}
				})
			}
			p.commit(t, mode == "flush")
		})
	}
}

func TestSparseEntireRuntimeResponseMatchesPinnedGaia(t *testing.T) {
	for _, recording := range []bool{false, true} {
		app, _, key, genesis := fixture(t, false)
		app.SetSparseAccounting(true)
		original := upstream.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0)
		original.InitChain(abci.RequestInitChain{AppStateBytes: genesis, ChainId: "observe-test"})
		var out bytes.Buffer
		if recording {
			app.SetRecorder(&out)
		}
		req := abci.RequestBeginBlock{Header: abci.Header{Height: 1, ChainID: "observe-test", Time: time.Unix(1500000001, 0)}}
		must(t, reflect.DeepEqual(app.BeginBlock(req), original.BeginBlock(req)), "entire pinned begin response")
		payer := sdk.AccAddress(key.PubKey().Address())
		receiver := sdk.AccAddress(bytes.Repeat([]byte{43}, 20))
		for i := 0; i < 2; i++ {
			msgs := []sdk.Msg{bank.NewMsgSend(payer, receiver, coins(100))}
			if i == 1 {
				msgs = append(msgs, bank.NewMsgSend(payer, receiver, coins(20000000)))
			}
			raw := signed(t, app, key, uint64(i), msgs...)
			must(t, reflect.DeepEqual(app.DeliverTx(raw), original.DeliverTx(raw)), "entire pinned transaction response including gas and tags")
		}
		end := abci.RequestEndBlock{Height: 1}
		must(t, reflect.DeepEqual(app.EndBlock(end), original.EndBlock(end)), "entire pinned end response")
		must(t, reflect.DeepEqual(app.Commit(), original.Commit()), "entire pinned commit response")
		must(t, app.RecorderError() == nil && reflect.DeepEqual(app.LastCommitID(), original.LastCommitID()), "pinned persistent root and private accounting")
	}
}
