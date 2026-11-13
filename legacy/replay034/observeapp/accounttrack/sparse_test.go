package accounttrack

import (
	"testing"

	st "github.com/cosmos/cosmos-sdk/store/types"
)

func TestSparseUnionHasNoCapAndNoWholeStoreReads(t *testing.T) {
	tr := NewTracker()
	tr.Bind(st.NewKVStoreKey("acc"))
	// aliasStore permits only exact-key iterators. Any scan or Next would panic.
	raw := &aliasStore{data: []byte{1}}
	v := tr.wrapKV(raw)
	f := open(t, tr, v)
	const count = 4096
	for i := 0; i < count; i++ {
		a := key(0)
		a[19] = byte(i >> 8)
		a[20] = byte(i)
		v.Set(a, []byte{2})
		v.Set(a, []byte{3})
	}
	b, a := closeFence(t, f)
	if len(b) != count || len(a) != count || raw.gets != 2*count {
		t.Fatal("sparse work or union mismatch")
	}
}

func TestDeletionRecreationAndEmptyValueOwnership(t *testing.T) {
	tr, m, k := setup(t)
	v := m.CacheMultiStore().GetKVStore(k)
	a := key(1)
	v.Set(a, []byte{1})
	f := open(t, tr, v)
	v.Delete(a)
	v.Set(a, []byte{2})
	v.Delete(a)
	b, after := closeFence(t, f)
	image(t, b, a, []byte{1})
	image(t, after, a, nil)
	raw := &aliasStore{data: []byte{}}
	v = tr.wrapKV(raw)
	f = open(t, tr, v)
	v.Set(a, []byte{3})
	b, _ = closeFence(t, f)
	if b[string(a)] == nil {
		t.Fatal("empty value lost to absence")
	}
}

func TestLoadWithOpenFenceLatchesButDelegates(t *testing.T) {
	tr, m, k := setup(t)
	v := m.GetKVStore(k)
	f := open(t, tr, v)
	if m.LoadVersion(0) != nil {
		t.Fatal("native load")
	}
	if tr.Error() != ErrTracking || v == m.GetKVStore(k) {
		t.Fatal("load boundary")
	}
	f.CloseFence(nil)
	tr, m, k = setup(t)
	f = open(t, tr, m.GetKVStore(k))
	closeFence(t, f)
	if _, _, e := f.CloseFence(nil); e != ErrTracking {
		t.Fatal("double close")
	}
}
