package accounttrack

import (
	"testing"

	st "github.com/cosmos/cosmos-sdk/store/types"
)

func TestLateFenceFlushKnownPendingKeys(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		tr, m, k := setup(t)
		root := m.GetKVStore(k)
		var child st.CacheWrap
		if standalone {
			child = root.CacheWrap()
		} else {
			child = m.CacheMultiStore().(st.CacheWrap)
		}
		var v st.KVStore
		if standalone {
			v = child.(st.KVStore)
		} else {
			v = child.(st.MultiStore).GetKVStore(k)
		}
		early := open(t, tr, v)
		v.Set(key(1), []byte{2})
		closeFence(t, early)
		// A new fence must capture the real parent before-image at flush even
		// though it did not exist when the originating Set happened.
		parent := open(t, tr, root)
		sameChild := open(t, tr, v)
		child.Write()
		b, a := closeFence(t, parent)
		image(t, b, key(1), nil)
		image(t, a, key(1), []byte{2})
		b, a = closeFence(t, sameChild)
		image(t, b, key(1), []byte{2})
		image(t, a, key(1), []byte{2})
	}
}

func TestUnknownDisabledPendingFlushFailsClosedButStillWrites(t *testing.T) {
	for _, nested := range []bool{false, true} {
		tr, m, k := setup(t)
		root := m.GetKVStore(k)
		outer := m.CacheMultiStore()
		child := outer
		if nested {
			child = outer.CacheMultiStore()
		}
		child.GetKVStore(k).Set(key(1), []byte{3})
		if nested {
			child.Write()
		}
		parent := open(t, tr, root)
		outer.Write()
		if tr.Error() != ErrTracking || !root.Has(key(1)) {
			t.Fatal("unknown flush evidence or execution")
		}
		if _, _, e := parent.CloseFence([][]byte{key(1)}); e != ErrTracking {
			t.Fatal("unsafe proof accepted")
		}
	}
	// The same guard applies to standalone account caches.
	tr, m, k := setup(t)
	v := m.GetKVStore(k)
	c := v.CacheWrap()
	c.(st.KVStore).Set(key(1), []byte{4})
	f := open(t, tr, v)
	c.Write()
	if _, _, e := f.CloseFence(nil); e != ErrTracking {
		t.Fatal("standalone unknown flush")
	}
}

func TestKnownPendingKeysPropagateAcrossDeferredNestedFlush(t *testing.T) {
	tr, m, k := setup(t)
	root := m.GetKVStore(k)
	outer := m.CacheMultiStore()
	child := outer.CacheMultiStore()
	f := open(t, tr, child.GetKVStore(k))
	child.GetKVStore(k).Set(key(1), []byte{7})
	closeFence(t, f)
	child.Write()
	f = open(t, tr, root)
	outer.Write()
	b, a := closeFence(t, f)
	image(t, b, key(1), nil)
	image(t, a, key(1), []byte{7})
}

func TestStaleRootAndCacheViewsRejectedAfterLoadOrCommit(t *testing.T) {
	for _, load := range []bool{false, true} {
		tr, m, k := setup(t)
		old := m.GetKVStore(k)
		oldCache := m.CacheMultiStore().GetKVStore(k)
		lazy := m.CacheMultiStore()
		if load {
			if m.LoadVersion(0) != nil {
				t.Fatal("load")
			}
		} else {
			m.Commit()
		}
		current := m.GetKVStore(k)
		if current == old {
			t.Fatal("epoch identity")
		}
		f := open(t, tr, current)
		closeFence(t, f)
		if _, e := tr.OpenFence(lazy.GetKVStore(k)); e != ErrTracking {
			t.Fatal("stale lazily materialized cache accepted")
		}
		if _, e := tr.OpenFence(old); e != ErrTracking {
			t.Fatal("stale root accepted")
		}
		if _, e := tr.OpenFence(oldCache); e != ErrTracking {
			t.Fatal("stale cache accepted")
		}
	}
}
