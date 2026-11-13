package accounttrack

import (
	"testing"

	st "github.com/cosmos/cosmos-sdk/store/types"
)

func TestLazyCacheCreationEpochIsNotFirstAccessorEpoch(t *testing.T) {
	for _, load := range []bool{false, true} {
		for _, kind := range []string{"lazy", "lazy-descendant", "stale-kv-child", "materialized", "root"} {
			tr, m, k := setup(t)
			oldRoot := m.GetKVStore(k)
			lazy := m.CacheMultiStore()
			var old st.KVStore
			if kind == "materialized" {
				old = lazy.GetKVStore(k)
			}
			if load {
				if m.LoadVersion(0) != nil {
					t.Fatal("load")
				}
			} else {
				m.Commit()
			}
			switch kind {
			case "lazy":
				old = lazy.GetKVStore(k)
			case "lazy-descendant":
				old = lazy.CacheMultiStore().GetKVStore(k)
			case "stale-kv-child":
				old = oldRoot.CacheWrap().(st.KVStore)
			case "root":
				old = oldRoot
			}
			if tr.Error() != nil {
				t.Fatal("premature error")
			}
			if _, e := tr.OpenFence(old); e != ErrTracking {
				t.Fatal("stale view accepted")
			}
		}
	}
}
