// Package nativekvtrack observes EVERY raw native key without decoding values
// or changing execution. Like SDK v0.33 stores, a Tracker is serial-use.
package nativekvtrack

import (
	"bytes"
	"errors"

	st "github.com/cosmos/cosmos-sdk/store/types"
)

// ErrTracking deliberately contains no keys, values, or underlying error text.
var ErrTracking = errors.New("native KV tracking failed")

// Values owns its keys and values. A present key with nil value means absence.
type Values map[string][]byte

// Tracker is permanently attached to store decorators. No fences means no
// observer reads, key validation, or copying on the mutation path.
type Tracker struct {
	key        st.StoreKey
	fences     map[*Fence]struct{}
	err        error
	generation uint64
	revision   uint64
}

func NewTracker() *Tracker { return &Tracker{} }

// Bind requires the exact mounted key capability, before accessing account
// stores. Rebinding is unsupported. Error is a permanent private failure latch.
func (t *Tracker) Bind(key st.StoreKey) error {
	if key == nil || (t.key != nil && t.key != key) {
		t.fail()
		return t.err
	}
	t.key = key
	return t.err
}

// Token identifies BOTH the exact native cache view and EVERY mutation/flush
// since the last complete snapshot. Reads cannot advance the mutation epoch.
// A token is never evidence of equal values across different cache views.
type Token struct {
	view                 *accountKV
	revision, generation uint64
}

func (t *Tracker) Token(store st.KVStore) (Token, error) {
	v, ok := store.(interface{ accountView() *accountKV })
	if !ok || v.accountView() == nil || v.accountView().t != t || v.accountView().generation != t.generation || t.err != nil {
		t.fail()
		return Token{}, t.err
	}
	return Token{v.accountView(), t.revision, t.generation}, nil
}
func (t *Tracker) touch() {
	t.revision++
	if t.revision == 0 {
		t.fail()
	}
}
func (t *Tracker) Error() error { return t.err }
func (t *Tracker) fail()        { t.err = ErrTracking }
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	r := make([]byte, len(b))
	copy(r, b)
	return r
}

// Fence pins an exact unmetered account view, not a Context.KVStore gas wrapper.
// Close it before discarding that view. A parent's fence may remain open while
// children are created, written, flushed, or discarded.
type Fence struct {
	t      *Tracker
	view   *accountKV
	before Values
	closed bool
}

// OpenFence only accepts this tracker's decorated raw store, rejecting gas
// wrappers, unrelated stores, and stores belonging to a different tracker.
func (t *Tracker) OpenFence(view st.KVStore) (*Fence, error) {
	v, ok := view.(interface{ accountView() *accountKV })
	if !ok || v.accountView() == nil || v.accountView().t != t || v.accountView().generation != t.generation || t.key == nil || t.err != nil {
		t.fail()
		return nil, t.err
	}
	f := &Fence{t: t, view: v.accountView(), before: make(Values)}
	if t.fences == nil {
		t.fences = make(map[*Fence]struct{})
	}
	t.fences[f] = struct{}{}
	return f, nil
}

// Observe ALL native keys, including counters and unknown/new prefixes.
// Native KV stores reject empty keys; never silently omit an invalid mutation.
func accountKey(k []byte) (observed, valid bool) {
	return len(k) > 0, len(k) > 0
}
func (t *Tracker) read(v *accountKV, k []byte) (out []byte) {
	defer func() {
		if recover() != nil {
			t.fail()
			out = nil
		}
	}()
	// cachekv.Get memoizes parent reads and can change a child's later native
	// reads after a parent write. Like the full oracle, observe via an iterator,
	// bounded to this exact key, without populating the execution read cache.
	it := v.raw.Iterator(k, append(clone(k), 0))
	defer it.Close()
	if !it.Valid() {
		return nil
	}
	if !bytes.Equal(it.Key(), k) {
		t.fail()
		return nil
	}
	return clone(it.Value())
}
func (t *Tracker) beforeWrite(k []byte) {
	if len(t.fences) == 0 {
		return
	}
	account, valid := accountKey(k)
	if !valid {
		t.fail()
		return
	}
	if !account {
		return
	}
	for f := range t.fences {
		if _, ok := f.before[string(k)]; !ok {
			f.before[string(clone(k))] = t.read(f.view, k)
		}
	}
}

// CloseFence returns owned raw before/after images for the union of candidate
// writes and extra record-only raw account keys. Extra keys are mandatory proof
// inputs from the independent movement ledger, including phantom records.
// No-write extra keys have identical before/after images. This does not infer
// transfers or reconcile coins. Any error makes all returned evidence unusable.
func (f *Fence) CloseFence(extra [][]byte) (Values, Values, error) {
	if f == nil {
		return nil, nil, ErrTracking
	}
	t := f.t
	if f.closed {
		t.fail()
		return nil, nil, t.err
	}
	f.closed = true
	delete(t.fences, f)
	for _, k := range extra {
		account, valid := accountKey(k)
		if !account || !valid {
			t.fail()
			continue
		}
		if _, ok := f.before[string(k)]; !ok {
			f.before[string(clone(k))] = t.read(f.view, k)
		}
	}
	after := make(Values, len(f.before))
	for k := range f.before {
		after[k] = t.read(f.view, []byte(k))
	}
	before := f.before
	f.before = nil
	return before, after, t.err
}

// A commit/load cannot silently carry a fence across an invalidated view.
func (t *Tracker) boundary() {
	t.touch()
	if len(t.fences) != 0 {
		t.fail()
	}
	t.generation++
}
