package accounttrack

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/store"
	"github.com/cosmos/cosmos-sdk/store/gaskv"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	dbm "github.com/tendermint/tendermint/libs/db"
)

func key(n byte) []byte { k := make([]byte, 21); k[0] = 1; k[20] = n; return k }
func setup(t *testing.T) (*Tracker, st.CommitMultiStore, *st.KVStoreKey) {
	t.Helper()
	tr := NewTracker()
	k := st.NewKVStoreKey("acc")
	if tr.Bind(k) != nil {
		t.Fatal("bind")
	}
	m := tr.WrapCommitMultiStore(store.NewCommitMultiStore(dbm.NewMemDB()))
	m.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
	if m.LoadLatestVersion() != nil {
		t.Fatal("load")
	}
	return tr, m, k
}
func open(t *testing.T, tr *Tracker, v st.KVStore) *Fence {
	t.Helper()
	f, e := tr.OpenFence(v)
	if e != nil {
		t.Fatal("open")
	}
	return f
}
func closeFence(t *testing.T, f *Fence, extra ...[]byte) (Values, Values) {
	t.Helper()
	b, a, e := f.CloseFence(extra)
	if e != nil {
		t.Fatal("close")
	}
	return b, a
}
func image(t *testing.T, m Values, k []byte, want []byte) {
	t.Helper()
	b, ok := m[string(k)]
	if !ok || !bytes.Equal(b, want) || (b == nil) != (want == nil) {
		t.Fatal("image mismatch")
	}
}

func TestAccessorsIdentityAndDirectMutations(t *testing.T) {
	tr, m, k := setup(t)
	v := m.GetKVStore(k)
	if v != m.GetStore(k) || v != m.GetCommitStore(k).(st.KVStore) || v != m.GetCommitKVStore(k) {
		t.Fatal("identity")
	}
	if _, ok := v.(st.CommitKVStore); !ok {
		t.Fatal("commit shape")
	}
	if _, ok := v.(st.Queryable); !ok {
		t.Fatal("query shape")
	}
	if _, ok := v.(st.CacheKVStore); ok {
		t.Fatal("unexpected cache shape")
	}
	accesses := []st.KVStore{v, m.GetStore(k).(st.KVStore), m.GetCommitStore(k).(st.KVStore), m.GetCommitKVStore(k)}
	for i, s := range accesses {
		f := open(t, tr, v)
		a := key(byte(i))
		s.Set(a, []byte{2})
		s.Delete(a)
		s.Set(a, []byte{3})
		b, after := closeFence(t, f)
		if len(b) != 1 {
			t.Fatal("write union")
		}
		image(t, b, a, nil)
		image(t, after, a, []byte{3})
	}
	f := open(t, tr, v)
	v.Set([]byte{2}, []byte{9})
	v.Delete([]byte{2})
	b, _ := closeFence(t, f)
	if len(b) != 0 {
		t.Fatal("counter included")
	}
}

func TestNestedFlushAndDiscard(t *testing.T) {
	for _, mode := range []string{"discard", "flush", "outer-discard"} {
		t.Run(mode, func(t *testing.T) {
			tr, m, k := setup(t)
			root := m.GetKVStore(k)
			a := key(1)
			root.Set(a, []byte{1})
			rf := open(t, tr, root)
			outer := m.CacheMultiStore()
			ov := outer.GetKVStore(k)
			of := open(t, tr, ov)
			child := outer.CacheMultiStore()
			cv := child.GetKVStore(k)
			cf := open(t, tr, cv)
			if cv == ov || ov == root || cv != child.GetStore(k) {
				t.Fatal("view identity")
			}
			cv.Set(a, []byte{2})
			cv.Set(a, []byte{3})
			cb, ca := closeFence(t, cf)
			image(t, cb, a, []byte{1})
			image(t, ca, a, []byte{3})
			if mode != "discard" {
				child.Write()
			}
			ob, oa := closeFence(t, of)
			image(t, ob, a, []byte{1})
			want := []byte{1}
			if mode != "discard" {
				want = []byte{3}
			}
			image(t, oa, a, want)
			if mode == "flush" {
				outer.Write()
			}
			rb, ra := closeFence(t, rf)
			image(t, rb, a, []byte{1})
			want = []byte{1}
			if mode == "flush" {
				want = []byte{3}
			}
			image(t, ra, a, want)
		})
	}
}

func TestEveryCacheAndTraceReturn(t *testing.T) {
	constructors := []func(st.CommitMultiStore) st.CacheMultiStore{
		func(m st.CommitMultiStore) st.CacheMultiStore { return m.CacheMultiStore() },
		func(m st.CommitMultiStore) st.CacheMultiStore { return m.CacheWrap().(st.CacheMultiStore) },
		func(m st.CommitMultiStore) st.CacheMultiStore {
			return m.CacheWrapWithTrace(new(bytes.Buffer), st.TraceContext{"test": true}).(st.CacheMultiStore)
		},
	}
	for i, makeCache := range constructors {
		tr, m, k := setup(t)
		root := m.GetKVStore(k)
		traced := m.SetTracer(new(bytes.Buffer)).SetTracingContext(st.TraceContext{"test": true})
		if traced != m {
			t.Fatal("root setter identity")
		}
		if _, ok := traced.(st.Queryable); !ok {
			t.Fatal("query lost")
		}
		c := makeCache(traced.(st.CommitMultiStore))
		v := c.GetKVStore(k)
		returned := c.SetTracer(new(bytes.Buffer)).SetTracingContext(st.TraceContext{"next": true})
		if returned != c || returned.GetKVStore(k) != v {
			t.Fatal("cache setter identity")
		}
		if _, ok := v.(st.CacheWrap); !ok {
			t.Fatal("cache shape")
		}
		if _, ok := v.(st.Committer); ok {
			t.Fatal("cache committer")
		}
		f := open(t, tr, root)
		// Every nested multi-store return must remain instrumented.
		for _, n := range []st.CacheWrap{c.CacheWrap(), c.CacheWrapWithTrace(new(bytes.Buffer), nil), c.CacheMultiStore().(st.CacheWrap)} {
			nv := n.(st.MultiStore).GetKVStore(k)
			nf := open(t, tr, nv)
			nv.Set(key(byte(i+1)), []byte{4})
			b, _ := closeFence(t, nf)
			if len(b) != 1 {
				t.Fatal("nested interception")
			}
		}
		// Account-only cache construction is also instrumented, with native Write.
		for _, n := range []st.CacheWrap{v.CacheWrap(), v.CacheWrapWithTrace(new(bytes.Buffer), nil)} {
			nv := n.(st.KVStore)
			nf := open(t, tr, nv)
			nv.Set(key(10), []byte{5})
			closeFence(t, nf)
			n.Write()
		}
		c.Write()
		b, a := closeFence(t, f)
		image(t, b, key(10), nil)
		image(t, a, key(10), []byte{5})
	}
}

func TestFirstImagesAndPhantomProofInputs(t *testing.T) {
	tr, m, k := setup(t)
	v := m.CacheMultiStore().GetKVStore(k)
	a := key(1)
	phantom := key(2)
	v.Set(a, []byte{1})
	v.Set(phantom, []byte{7})
	f := open(t, tr, v)
	v.Set(a, []byte{2})
	g := open(t, tr, v)
	v.Delete(a)
	v.Set(a, []byte{3})
	gb, ga := closeFence(t, g)
	image(t, gb, a, []byte{2})
	image(t, ga, a, []byte{3})
	b, after := closeFence(t, f, phantom, key(3))
	image(t, b, a, []byte{1})
	image(t, after, a, []byte{3})
	image(t, b, phantom, []byte{7})
	image(t, after, phantom, []byte{7})
	image(t, b, key(3), nil)
	// Caller reconciliation must see the unexplained write and the independently
	// supplied phantom key, rather than treating an empty dirty set as proof.
	f = open(t, tr, v)
	b, after = closeFence(t, f, phantom)
	if len(b) != 1 || !bytes.Equal(b[string(phantom)], after[string(phantom)]) {
		t.Fatal("phantom omitted")
	}
}

func TestFailClosedAndExecutionContinues(t *testing.T) {
	tr, m, k := setup(t)
	v := m.GetKVStore(k)
	f := open(t, tr, v)
	malformed := []byte{1}
	v.Set(malformed, []byte{4})
	if tr.Error() != ErrTracking || !v.Has(malformed) {
		t.Fatal("malformed handling")
	}
	if _, _, e := f.CloseFence(nil); e != ErrTracking {
		t.Fatal("missing latch")
	}
	tr, m, k = setup(t)
	v = m.GetKVStore(k)
	f = open(t, tr, v)
	if _, _, e := f.CloseFence([][]byte{{2}}); e != ErrTracking {
		t.Fatal("record key validation")
	}
	tr, m, k = setup(t)
	v = m.GetKVStore(k)
	// Disabled mode intentionally does not inspect keys or values.
	v.Set([]byte{1}, []byte{2})
	if tr.Error() != nil {
		t.Fatal("disabled inspection")
	}
	if _, e := tr.OpenFence(gaskv.NewStore(v, sdk.NewGasMeter(100), st.KVGasConfig())); e == nil {
		t.Fatal("gas wrapper accepted")
	}
}

// aliasStore deliberately retains/reuses buffers to prove that observer images
// and candidate keys are owned independently of caller and native buffers.
type aliasStore struct {
	st.KVStore
	data               []byte
	gets, sets, writes int
	panicRead          bool
}

func (s *aliasStore) Get([]byte) []byte {
	s.gets++
	if s.panicRead {
		panic("private")
	}
	return s.data
}

// Only exact-key point lookups are supported. A full or prefix scan fails.
func (s *aliasStore) Iterator(start, end []byte) st.Iterator {
	if len(start) != 21 || !bytes.Equal(end, append(clone(start), 0)) {
		panic("unbounded observer read")
	}
	s.gets++
	if s.panicRead {
		panic("private")
	}
	return &aliasIterator{start: start, end: end, data: s.data}
}

type aliasIterator struct {
	start, end, data []byte
	closed           bool
}

func (i *aliasIterator) Domain() ([]byte, []byte) { return i.start, i.end }
func (i *aliasIterator) Valid() bool              { return !i.closed && i.data != nil }
func (i *aliasIterator) Next()                    { panic("point read advanced") }
func (i *aliasIterator) Key() []byte              { return i.start }
func (i *aliasIterator) Value() []byte            { return i.data }
func (i *aliasIterator) Close()                   { i.closed = true }

func (s *aliasStore) Set(_, b []byte) { s.sets++; s.data = b }
func (s *aliasStore) Delete([]byte)   { s.sets++; s.data = nil }
func (s *aliasStore) Write()          { s.writes++ }
func TestCopiesReadFailureAndWriteOnce(t *testing.T) {
	tr := NewTracker()
	tr.Bind(st.NewKVStoreKey("acc"))
	raw := &aliasStore{data: []byte{1}}
	v := tr.wrapKV(raw)
	f := open(t, tr, v)
	a := key(1)
	old := raw.data
	val := []byte{2}
	v.Set(a, val)
	a[20] = 9
	old[0] = 8
	b, after := closeFence(t, f)
	val[0] = 7
	image(t, b, key(1), []byte{1})
	image(t, after, key(1), []byte{2})
	v.(st.CacheWrap).Write()
	if raw.writes != 1 || raw.sets != 1 {
		t.Fatal("native delegation")
	}
	f = open(t, tr, v)
	raw.panicRead = true
	v.Set(key(3), []byte{4})
	if raw.sets != 2 || tr.Error() != ErrTracking {
		t.Fatal("read panic altered execution")
	}
	if _, _, e := f.CloseFence(nil); e != ErrTracking {
		t.Fatal("read error missing")
	}
	tr = NewTracker()
	raw = &aliasStore{}
	v = tr.wrapKV(raw)
	v.Set(key(1), []byte{1})
	v.Delete(key(1))
	if raw.gets != 0 {
		t.Fatal("disabled reads")
	}
}

func TestObserverPointReadsDoNotPopulateChildExecutionCache(t *testing.T) {
	for _, cachedParent := range []bool{false, true} {
		for _, mode := range []string{"replace", "create", "delete", "close-only"} {
			t.Run(mode, func(t *testing.T) {
				tr, m, k := setup(t)
				var parent st.MultiStore = m
				if cachedParent {
					parent = m.CacheMultiStore()
				}
				pv := parent.GetKVStore(k)
				a := key(1)
				var initial []byte
				if mode != "create" {
					initial = []byte{1}
					pv.Set(a, initial)
				}
				child := parent.CacheMultiStore().GetKVStore(k)
				f := open(t, tr, child)
				want := []byte{2}
				if mode == "close-only" {
					before, after := closeFence(t, f, a)
					image(t, before, a, initial)
					image(t, after, a, initial)
				}
				if mode == "delete" {
					pv.Delete(a)
					want = nil
				} else {
					pv.Set(a, want)
				}
				if !bytes.Equal(child.Get(a), want) {
					t.Fatal("observer read changed child execution after parent mutation")
				}
				if mode != "close-only" {
					before, after := closeFence(t, f)
					image(t, before, a, initial)
					image(t, after, a, want)
				}
			})
		}
	}
}

func TestBoundaryAndForeignViews(t *testing.T) {
	tr, m, k := setup(t)
	v := m.GetKVStore(k)
	f := open(t, tr, v)
	m.Commit()
	if tr.Error() != ErrTracking {
		t.Fatal("commit boundary")
	}
	f.CloseFence(nil)
	tr, m, k = setup(t)
	v = m.GetKVStore(k)
	m.Commit()
	if m.LoadLatestVersion() != nil {
		t.Fatal("reload")
	}
	if v == m.GetKVStore(k) {
		t.Fatal("stale root wrapper")
	}
	other := NewTracker()
	other.Bind(k)
	if _, e := other.OpenFence(m.GetKVStore(k)); e != ErrTracking {
		t.Fatal("foreign accepted")
	}
}
