package observeapp

import (
	"bytes"
	"encoding/json"
	"testing"

	upstream "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/bank"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	"reflect"
)

func TestLosslessImagesMatchCompleteOracleAcrossAllPrefixesAndCacheBranches(t *testing.T) {
	app, ctx, _, _, _ := nativeFixture(t)
	app.SetLosslessRecording(true)
	nativeNow(t, app, ctx)
	for _, s := range app.nativeStores() {
		kv := ctx.MultiStore().GetKVStore(s.key)
		for prefix := 0; prefix < 256; prefix++ {
			kv.Set([]byte{byte(prefix), 0xae, 0x92}, []byte{byte(prefix)})
		}
		kv.Set([]byte{0xff, 0xae, 0x92}, []byte{})
	}
	before := nativeNow(t, app, ctx)
	cms := ctx.MultiStore().CacheMultiStore()
	childctx := ctx.WithMultiStore(cms)
	for _, s := range app.nativeStores() {
		child := cms.GetKVStore(s.key)
		child.Get([]byte{0x01, 0xae, 0x92})
		ctx.MultiStore().GetKVStore(s.key).Set([]byte{0x01, 0xae, 0x92}, []byte{99})
		child.Set([]byte{0x02, 0xae, 0x92}, []byte{88})
		child.Delete([]byte{0x03, 0xae, 0x92})
	}
	nativeNow(t, app, childctx)
	nativeNow(t, app, ctx)
	cms.Write()
	nativeNow(t, app, ctx)
	for _, s := range app.nativeStores() {
		direct := ctx.MultiStore().GetKVStore(s.key).CacheWrap().(st.CacheKVStore)
		direct.Set([]byte{0x04, 0xae, 0x92}, []byte{})
		direct.Write()
	}
	nativeNow(t, app, ctx)
	for i := range before {
		must(t, bytes.Equal(before[i].digest, app.nativeDigest(before[i].values)), "immutable complete before image")
	}
}

func losslessFenceScenario(t *testing.T, enabled bool) []byte {
	app, ctx, _, _, _ := nativeFixture(t)
	app.SetLosslessRecording(enabled)
	app.nativeCheckpoint(ctx, "anchor")
	outer := app.nativeFence(ctx)
	cms := ctx.MultiStore().CacheMultiStore()
	childctx := ctx.WithMultiStore(cms)
	attempted := app.nativeFence(childctx)
	for _, s := range app.nativeStores() {
		kv := cms.GetKVStore(s.key)
		kv.Set([]byte{0xfa, 0xae}, []byte{1, 2, 3})
		kv.Set([]byte{0xfa, 0xaf}, []byte{})
	}
	attempted()
	// Child is deliberately discarded, while its entire attempted state remains.
	outer()
	app.nativeCheckpoint(ctx, "final")
	must(t, app.RecorderError() == nil, "lossless discarded-child recording")
	b, err := json.Marshal(app.observer.pending)
	must(t, err == nil, "encode complete records")
	return b
}
func TestLosslessNativeRecordsExactlyEqualFullIncludingNonzeroDiscardedWrites(t *testing.T) {
	must(t, bytes.Equal(losslessFenceScenario(t, false), losslessFenceScenario(t, true)), "all native records byte-identical to complete oracle")
}

func TestLosslessSparseAccountingIncludesRecordOnlyAllCoinsAndMetadata(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		app, ctx, del, _, _ := nativeFixture(t)
		app.SetLosslessRecording(enabled)
		f := app.openAccounting(ctx)
		a := app.accountKeeper.GetAccount(ctx, del)
		a.SetSequence(a.GetSequence() + 1)
		app.accountKeeper.SetAccount(ctx, a)
		b, c := app.closeAccounting(f, []Record{{Kind: "movement", Address: del.String(), Denom: "stake", Amount: "0"}})
		must(t, equal(b, c) && app.RecorderError() == nil, "metadata-only native writes do not invent movements")
		f = app.openAccounting(ctx)
		b, c = app.closeAccounting(f, []Record{{Kind: "movement", Address: del.String(), Denom: "stake", Amount: "1"}})
		app.reconcile(b, c, []Record{{Kind: "movement", Address: del.String(), Denom: "stake", Amount: "1"}})
		must(t, app.RecorderError() != nil, "phantom ledger-only movement rejected")
	}
}

func TestLosslessPartialNativeFlushEvidenceRefusesCommit(t *testing.T) {
	for _, skip := range []string{"ante", "message"} {
		_, _, key, raw := fixture(t, false)
		app := NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true)
		app.SetLosslessRecording(true)
		app.InitChain(abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"})
		var out bytes.Buffer
		app.SetRecorder(&out)
		app.nativeTrackers[8].ObserveFlushes(func(view, parent uint64) {
			if view == app.observer.anteViews[8] && skip != "ante" {
				app.observer.anteFlushed[8] = true
			}
			if view == app.observer.messageViews[8] && skip != "message" {
				app.observer.messageFlushed[8] = true
			}
		})
		begin(app, 1)
		payer := sdk.AccAddress(key.PubKey().Address())
		receiver := sdk.AccAddress(bytes.Repeat([]byte{8}, 20))
		app.DeliverTx(signed(t, app, key, 0, bank.NewMsgSend(payer, receiver, coins(100))))
		must(t, app.RecorderError() != nil, "partial native "+skip+" flush evidence refused")
		must(t, len(app.Commit().Data) == 0 && app.LastBlockHeight() == 0, "unobserved cache flush cannot commit")
	}
}

func TestLosslessModeCannotChangeDuringPhase(t *testing.T) {
	app, _, _, _, _ := nativeFixture(t)
	app.observer.active = true
	app.SetLosslessRecording(true)
	must(t, app.RecorderError() != nil, "active mode switch refused")
}

func TestLosslessNativeFlushDistinguishesRollbackFromPostWriteFailure(t *testing.T) {
	for _, late := range []bool{false, true} {
		_, _, key, raw := fixture(t, false)
		app := NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true)
		app.SetLosslessRecording(true)
		native := upstream.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true)
		req := abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"}
		if late {
			req.ConsensusParams = &abci.ConsensusParams{BlockSize: &abci.BlockSizeParams{MaxGas: 25000}}
		}
		app.InitChain(req)
		native.InitChain(req)
		var out bytes.Buffer
		app.SetRecorder(&out)
		begin(app, 1)
		native.BeginBlock(abci.RequestBeginBlock{Header: app.NewContext(false, abci.Header{}).BlockHeader()})
		payer := sdk.AccAddress(key.PubKey().Address())
		receiver := sdk.AccAddress(bytes.Repeat([]byte{7}, 20))
		msgs := []sdk.Msg{bank.NewMsgSend(payer, receiver, coins(100))}
		if !late {
			msgs = append(msgs, bank.NewMsgSend(payer, receiver, coins(20000000)))
		}
		tx := signed(t, app, key, 0, msgs...)
		result, want := app.DeliverTx(tx), native.DeliverTx(tx)
		must(t, result.Code != 0 && reflect.DeepEqual(result, want), "entire failed native result remains exact")
		must(t, app.RecorderError() == nil, "native flush evidence resolves committed-vs-attempted semantics")
		writes, aborted := 0, 0
		for _, r := range app.observer.pending {
			if r.Kind == "native_write" && r.Phase == "message" {
				writes++
				if r.Status == "aborted" {
					aborted++
				}
			}
		}
		must(t, writes > 0, "nonzero native source message writes captured")
		must(t, (late && aborted == 0) || (!late && aborted == writes), "raw source writes have actual native cache disposition")
		must(t, reflect.DeepEqual(app.EndBlock(abci.RequestEndBlock{Height: 1}), native.EndBlock(abci.RequestEndBlock{Height: 1})), "native end remains exact")
		must(t, bytes.Equal(app.Commit().Data, native.Commit().Data) && app.RecorderError() == nil, "committed native root exact through rejected transaction")
		begin(app, 2)
		nativeNow(t, app, app.NewContext(false, abci.Header{}))
		app.EndBlock(abci.RequestEndBlock{Height: 2})
		app.Commit()
		must(t, app.RecorderError() == nil, "complete next-generation image after native commit")
	}
}
