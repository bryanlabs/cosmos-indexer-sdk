package accounttrack

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/store/gaskv"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestObserverReadsDoNotChargeExecutionGas(t *testing.T) {
	var baseline sdk.Gas
	for _, enabled := range []bool{false, true} {
		tr, m, k := setup(t)
		v := m.CacheMultiStore().GetKVStore(k)
		meter := sdk.NewGasMeter(1000000)
		execution := gaskv.NewStore(v, meter, st.KVGasConfig())
		var fences []*Fence
		if enabled {
			for i := 0; i < 4; i++ {
				fences = append(fences, open(t, tr, v))
			}
		}
		execution.Set(key(1), []byte{3})
		execution.Get(key(1))
		execution.Has(key(1))
		execution.Delete(key(1))
		for _, f := range fences {
			closeFence(t, f, key(2))
		}
		if !enabled {
			baseline = meter.GasConsumed()
		} else if meter.GasConsumed() != baseline {
			t.Fatal("observer charged consensus gas")
		}
	}
}

type nativeCache interface{ st.CacheMultiStore }
type countedMulti struct {
	nativeCache
	writes int
}

func (m *countedMulti) Write() { m.writes++; m.nativeCache.Write() }
func TestNativeMultiWriteOnce(t *testing.T) {
	tr, m, k := setup(t)
	raw := &countedMulti{nativeCache: m.CacheMultiStore()}
	wrapped := tr.wrapMulti(raw)
	f := open(t, tr, m.GetKVStore(k))
	wrapped.GetKVStore(k).Set(key(1), []byte{3})
	wrapped.Write()
	if raw.writes != 1 {
		t.Fatal("multi write delegation")
	}
	b, a := closeFence(t, f)
	image(t, b, key(1), nil)
	image(t, a, key(1), []byte{3})
}

func TestRootAccountCachePaths(t *testing.T) {
	for _, trace := range []bool{false, true} {
		tr, m, k := setup(t)
		v := m.GetKVStore(k)
		f := open(t, tr, v)
		var c st.CacheWrap
		if trace {
			c = v.CacheWrapWithTrace(new(bytes.Buffer), nil)
		} else {
			c = v.CacheWrap()
		}
		cv := c.(st.KVStore)
		cf := open(t, tr, cv)
		cv.Set(key(1), []byte{4})
		closeFence(t, cf)
		c.Write()
		b, a := closeFence(t, f)
		image(t, b, key(1), nil)
		image(t, a, key(1), []byte{4})
	}
}
