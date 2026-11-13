package accounttrack

import (
	"io"

	st "github.com/cosmos/cosmos-sdk/store/types"
)

// accountKV embeds native read/iterator behavior. raw is always unmetered;
// execution may use a separate native tracing delegate at the commit root.
type accountKV struct {
	st.KVStore
	raw            st.KVStore
	t              *Tracker
	execution      func() st.KVStore
	generation     uint64
	parent         *accountKV
	cached         bool
	pending        map[string]struct{}
	unknownPending bool
}

func (v *accountKV) accountView() *accountKV { return v }
func (v *accountKV) target() st.KVStore {
	if v.execution != nil {
		return v.execution()
	}
	return v.KVStore
}
func (v *accountKV) Get(k []byte) []byte                     { return v.target().Get(k) }
func (v *accountKV) Has(k []byte) bool                       { return v.target().Has(k) }
func (v *accountKV) Iterator(a, b []byte) st.Iterator        { return v.target().Iterator(a, b) }
func (v *accountKV) ReverseIterator(a, b []byte) st.Iterator { return v.target().ReverseIterator(a, b) }
func (v *accountKV) noteWrite(k []byte) {
	if v.generation != v.t.generation {
		v.t.fail()
	}
	v.t.beforeWrite(k)
	if !v.cached || len(k) == 0 || k[0] != 1 {
		return
	}
	if len(v.t.fences) == 0 {
		v.unknownPending = true
		return
	}
	if v.pending == nil {
		v.pending = make(map[string]struct{})
	}
	v.pending[string(k)] = struct{}{}
}
func (v *accountKV) Set(k, b []byte) { v.noteWrite(k); v.target().Set(k, b) }
func (v *accountKV) Delete(k []byte) { v.noteWrite(k); v.target().Delete(k) }
func (v *accountKV) wrapChild(raw st.CacheWrap) st.CacheWrap {
	child := v.t.wrapKV(raw.(st.KVStore))
	cv := child.(interface{ accountView() *accountKV }).accountView()
	cv.parent = v
	cv.generation = v.generation
	return child.(st.CacheWrap)
}
func (v *accountKV) CacheWrap() st.CacheWrap { return v.wrapChild(v.target().CacheWrap()) }
func (v *accountKV) CacheWrapWithTrace(w io.Writer, c st.TraceContext) st.CacheWrap {
	return v.wrapChild(v.target().CacheWrapWithTrace(w, c))
}

// Native cache Write holds its own mutex. Capture pending keys before entering
// it, never from a parent callback while that mutex is held.
func (v *accountKV) beforeFlush() {
	if v.generation != v.t.generation {
		v.t.fail()
	}
	if v.unknownPending && len(v.t.fences) != 0 {
		v.t.fail()
	}
	for k := range v.pending {
		v.t.beforeWrite([]byte(k))
	}
}
func (v *accountKV) afterFlush() {
	if p := v.parent; p != nil && p.cached {
		p.unknownPending = p.unknownPending || v.unknownPending
		if len(v.pending) != 0 && p.pending == nil {
			p.pending = make(map[string]struct{})
		}
		for k := range v.pending {
			p.pending[k] = struct{}{}
		}
	}
	v.pending = nil
	v.unknownPending = false
}

type cacheKV struct {
	*accountKV
	native st.CacheWrap
}

func (v *cacheKV) Write() { v.beforeFlush(); v.native.Write(); v.afterFlush() }

type commitKV struct {
	*accountKV
	st.Committer
}

func (v *commitKV) Commit() st.CommitID { v.t.boundary(); return v.Committer.Commit() }

type queryKV struct {
	*accountKV
	st.Queryable
}
type queryCommitKV struct {
	*commitKV
	st.Queryable
}
type queryCacheKV struct {
	*cacheKV
	st.Queryable
}

func (t *Tracker) wrapKV(raw st.KVStore) st.KVStore {
	v := &accountKV{KVStore: raw, raw: raw, t: t, generation: t.generation}
	q, query := raw.(st.Queryable)
	if c, ok := raw.(st.CommitKVStore); ok {
		r := &commitKV{v, c}
		if query {
			return &queryCommitKV{r, q}
		}
		return r
	}
	if c, ok := raw.(st.CacheWrap); ok {
		v.cached = true
		r := &cacheKV{v, c}
		if query {
			return &queryCacheKV{r, q}
		}
		return r
	}
	if query {
		return &queryKV{v, q}
	}
	return v
}

// multi memoizes exactly one account wrapper per native multi-store view. Native
// cachemulti tracing setters return value copies sharing the same substore map;
// retaining their result here preserves both tracing configuration and identity.
type multi struct {
	st.MultiStore
	t          *Tracker
	account    st.KVStore
	parent     *multi
	execution  func(st.StoreKey) st.KVStore
	generation uint64
}

func (m *multi) viewGeneration() uint64 {
	if m.execution != nil {
		return m.t.generation
	}
	return m.generation
}

func (m *multi) GetKVStore(k st.StoreKey) st.KVStore {
	if m.t.key == nil || k != m.t.key {
		return m.MultiStore.GetKVStore(k)
	}
	if m.account != nil && m.execution != nil && m.account.(interface{ accountView() *accountKV }).accountView().generation != m.t.generation {
		m.account = nil
	}
	if m.account == nil {
		m.account = m.t.wrapKV(m.MultiStore.GetStore(k).(st.KVStore))
		v := m.account.(interface{ accountView() *accountKV }).accountView()
		v.generation = m.viewGeneration()
		if m.parent != nil {
			v.parent = m.parent.GetKVStore(k).(interface{ accountView() *accountKV }).accountView()
		}
		if m.execution != nil {
			v.execution = func() st.KVStore { return m.execution(k) }
		}
	}
	return m.account
}
func (m *multi) GetStore(k st.StoreKey) st.Store {
	if m.t.key != nil && k == m.t.key {
		return m.GetKVStore(k)
	}
	return m.MultiStore.GetStore(k)
}
func (m *multi) CacheMultiStore() st.CacheMultiStore {
	return m.wrapChild(m.MultiStore.CacheMultiStore())
}
func (m *multi) CacheWrap() st.CacheWrap {
	return m.wrapChild(m.MultiStore.CacheWrap().(st.CacheMultiStore))
}
func (m *multi) CacheWrapWithTrace(w io.Writer, c st.TraceContext) st.CacheWrap {
	return m.wrapChild(m.MultiStore.CacheWrapWithTrace(w, c).(st.CacheMultiStore))
}

func (m *multi) wrapChild(raw st.CacheMultiStore) *cacheMulti {
	child := m.t.wrapMulti(raw)
	child.parent = m
	child.generation = m.viewGeneration()
	return child
}

type cacheMulti struct {
	*multi
	native st.CacheMultiStore
}

func (m *cacheMulti) Write() {
	if m.viewGeneration() != m.t.generation {
		m.t.fail()
	}
	var v *accountKV
	if m.account != nil {
		v = m.account.(interface{ accountView() *accountKV }).accountView()
		v.beforeFlush()
	}
	m.native.Write()
	if v != nil {
		v.afterFlush()
	}
}
func (m *cacheMulti) SetTracer(w io.Writer) st.MultiStore {
	m.MultiStore = m.MultiStore.SetTracer(w)
	m.native = m.MultiStore.(st.CacheMultiStore)
	return m
}
func (m *cacheMulti) SetTracingContext(c st.TraceContext) st.MultiStore {
	m.MultiStore = m.MultiStore.SetTracingContext(c)
	m.native = m.MultiStore.(st.CacheMultiStore)
	return m
}
func (t *Tracker) wrapMulti(raw st.CacheMultiStore) *cacheMulti {
	return &cacheMulti{&multi{MultiStore: raw, t: t, generation: t.generation}, raw}
}

type commitMulti struct {
	*multi
	st.CommitMultiStore
}

// WrapCommitMultiStore installs a permanent decorator. Keep no writable raw
// backing-store aliases. Bind before any account accessor, mount/load normally.
func (t *Tracker) WrapCommitMultiStore(raw st.CommitMultiStore) st.CommitMultiStore {
	m := &commitMulti{multi: &multi{MultiStore: raw, t: t, execution: raw.GetKVStore}, CommitMultiStore: raw}
	if q, ok := raw.(st.Queryable); ok {
		return &queryMulti{m, q}
	}
	return m
}

// Explicit forwarding resolves embedded interface ambiguity.
func (m *commitMulti) GetStoreType() st.StoreType { return m.multi.GetStoreType() }
func (m *commitMulti) TracingEnabled() bool       { return m.multi.TracingEnabled() }
func (m *commitMulti) GetStore(k st.StoreKey) st.Store {
	if m.t.key != nil && k == m.t.key {
		return m.GetKVStore(k)
	}
	return m.CommitMultiStore.GetStore(k)
}
func (m *commitMulti) GetKVStore(k st.StoreKey) st.KVStore {
	if m.t.key == nil || k != m.t.key {
		return m.CommitMultiStore.GetKVStore(k)
	}
	return m.multi.GetKVStore(k)
}
func (m *commitMulti) GetCommitStore(k st.StoreKey) st.CommitStore {
	if m.t.key != nil && k == m.t.key {
		return m.GetKVStore(k).(st.CommitStore)
	}
	return m.CommitMultiStore.GetCommitStore(k)
}
func (m *commitMulti) GetCommitKVStore(k st.StoreKey) st.CommitKVStore {
	if m.t.key != nil && k == m.t.key {
		return m.GetKVStore(k).(st.CommitKVStore)
	}
	return m.CommitMultiStore.GetCommitKVStore(k)
}
func (m *commitMulti) CacheMultiStore() st.CacheMultiStore { return m.multi.CacheMultiStore() }
func (m *commitMulti) CacheWrap() st.CacheWrap             { return m.multi.CacheWrap() }
func (m *commitMulti) CacheWrapWithTrace(w io.Writer, c st.TraceContext) st.CacheWrap {
	return m.multi.CacheWrapWithTrace(w, c)
}
func (m *commitMulti) SetTracer(w io.Writer) st.MultiStore {
	m.multi.MultiStore = m.CommitMultiStore.SetTracer(w)
	return m
}
func (m *commitMulti) SetTracingContext(c st.TraceContext) st.MultiStore {
	m.multi.MultiStore = m.CommitMultiStore.SetTracingContext(c)
	return m
}
func (m *commitMulti) Commit() st.CommitID {
	m.t.boundary()
	m.account = nil
	return m.CommitMultiStore.Commit()
}
func (m *commitMulti) LoadVersion(v int64) error {
	m.t.boundary()
	m.account = nil
	return m.CommitMultiStore.LoadVersion(v)
}
func (m *commitMulti) LoadLatestVersion() error {
	m.t.boundary()
	m.account = nil
	return m.CommitMultiStore.LoadLatestVersion()
}

type queryMulti struct {
	*commitMulti
	st.Queryable
}

func (m *queryMulti) SetTracer(w io.Writer) st.MultiStore { m.commitMulti.SetTracer(w); return m }
func (m *queryMulti) SetTracingContext(c st.TraceContext) st.MultiStore {
	m.commitMulti.SetTracingContext(c)
	return m
}

var (
	_ st.CommitMultiStore = (*commitMulti)(nil)
	_ st.CacheMultiStore  = (*cacheMulti)(nil)
	_ st.CacheWrap        = (*cacheMulti)(nil)
	_ st.CommitKVStore    = (*commitKV)(nil)
	_ st.CacheKVStore     = (*cacheKV)(nil)
	_ st.CacheWrap        = (*cacheKV)(nil)
	_ st.Queryable        = (*queryMulti)(nil)
)
