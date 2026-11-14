package observeapp

import (
	"bytes"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"reflect"
	"testing"
)

func completeNativeOracle(t *testing.T, app *GaiaApp, ctx sdk.Context, native nativeSnapshot) {
	t.Helper()
	for i, s := range app.nativeStores() {
		expected := make([]nativeKV, 0)
		it := quiet(ctx).KVStore(s.key).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			expected = append(expected, nativeKV{nativeCopy(it.Key()), nativeCopy(it.Value())})
		}
		it.Close()
		if !reflect.DeepEqual(expected, native[i].values) {
			t.Fatal("complete raw native snapshot differs from independent live iterator")
		}
	}
}
func nativeNow(t *testing.T, app *GaiaApp, ctx sdk.Context) nativeSnapshot {
	t.Helper()
	out, ok := app.nativeSnapshot(ctx, app.nativeStores())
	if !ok {
		t.Fatal("native complete snapshot refused")
	}
	completeNativeOracle(t, app, ctx, out)
	return out
}
func TestNativeMemoKeepsEveryPrefixEmptyValueAndImmutableBeforeState(t *testing.T) {
	app, ctx, _, _, _ := nativeFixture(t)
	initial := nativeNow(t, app, ctx)
	same := nativeNow(t, app, ctx)
	for i := range initial {
		must(t, initial[i] == same[i], "same exact view/epoch reuses FULL state")
	}
	before := make([][]byte, len(initial))
	for i := range initial {
		before[i] = append([]byte(nil), initial[i].digest...)
	}
	for _, s := range app.nativeStores() {
		store := ctx.MultiStore().GetKVStore(s.key)
		for prefix := 0; prefix < 256; prefix++ {
			store.Set([]byte{byte(prefix), 0xa7, 0x91}, []byte{byte(prefix)})
		}
		store.Set([]byte{0xff, 0xa7, 0x91}, []byte{})
	}
	changed := nativeNow(t, app, ctx)
	for i := range initial {
		must(t, changed[i] != initial[i] && bytes.Equal(before[i], initial[i].digest), "writes invalidate while BEFORE remains immutable")
		must(t, bytes.Equal(initial[i].digest, app.nativeDigest(initial[i].values)), "retained BEFORE bytes unchanged")
	}
	for _, s := range app.nativeStores() {
		ctx.MultiStore().GetKVStore(s.key).Delete([]byte{0x00, 0xa7, 0x91})
	}
	nativeNow(t, app, ctx)
	must(t, app.RecorderError() == nil, "all unknown native prefixes observed without dropping bytes")
}
func TestNativeMemoCacheForkReadMemoParentWritesAndBothFlushAPIs(t *testing.T) {
	app, ctx, _, _, _ := nativeFixture(t)
	key := []byte{0xfe, 0xab, 0xcd}
	parent := ctx.MultiStore().GetKVStore(app.keySlashing)
	parent.Set(key, []byte{1})
	original := nativeNow(t, app, ctx)
	cms := ctx.MultiStore().CacheMultiStore()
	childctx := ctx.WithMultiStore(cms)
	child := cms.GetKVStore(app.keySlashing)
	child.Get(key)
	nativeNow(t, app, childctx)
	parent.Set(key, []byte{2})
	nativeNow(t, app, childctx)
	child.Set(key, []byte{3})
	nativeNow(t, app, childctx)
	nativeNow(t, app, ctx)
	must(t, bytes.Equal(parent.Get(key), []byte{2}), "discarded child cannot leak")
	cms.Write()
	nativeNow(t, app, ctx)
	must(t, bytes.Equal(parent.Get(key), []byte{3}), "cache-multi Write invalidates parent snapshot")
	direct := parent.CacheWrap().(st.CacheKVStore)
	direct.Set(key, []byte{4})
	nativeNow(t, app, ctx)
	direct.Write()
	after := nativeNow(t, app, ctx)
	must(t, bytes.Equal(parent.Get(key), []byte{4}) && original[3] != after[3], "direct cache Write invalidates complete parent view")
}
func TestNativeMemoObservationDoesNotConsumeExecutionGas(t *testing.T) {
	app, ctx, _, _, _ := nativeFixture(t)
	gas, block := sdk.NewGasMeter(100000000), sdk.NewGasMeter(100000000)
	ctx = ctx.WithGasMeter(gas).WithBlockGasMeter(block)
	app.nativeSnapshot(ctx, app.nativeStores())
	app.nativeSnapshot(ctx, app.nativeStores())
	must(t, gas.GasConsumed() == 0 && block.GasConsumed() == 0, "complete snapshot never uses execution gas meters")
}
