package nativekvtrack

import (
	"bytes"
	st "github.com/cosmos/cosmos-sdk/store/types"
	"sort"
)

// Entry retains every byte. A present empty value is not an absent key.
// Images are immutable borrowed values, consumed only by the observer.
type Entry struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// EnableImages selects lossless affected-key snapshots before observation.
// The first image is a complete native iterator anchor, never a projection.
func (t *Tracker) EnableImages() {
	if t.images {
		return
	}
	t.images = true
	t.dirty = make(map[string]uint64)
}

// Image reconstructs the exact iterator view from a complete anchor and ALL
// candidate writes, including discarded child writes. The most recent EXACT
// view can advance from its own image using revisions; switching cache views
// always returns to the complete commit-generation anchor and all candidates.
// A commit rebases that anchor; a load invalidates it. No speculative write is
// forgotten merely because it was observed in a different cache branch.
func (t *Tracker) Image(store st.KVStore) ([]Entry, error) {
	token, err := t.Token(store)
	if err != nil || !t.images {
		t.fail()
		return nil, t.err
	}
	v := token.view
	if t.image == nil {
		t.image, t.imageGeneration = t.scanImage(v), t.generation
	}
	if t.imageGeneration != t.generation {
		t.fail()
		return nil, t.err
	}
	base := t.image
	var since uint64
	if t.lastView == v && t.lastGeneration == t.generation {
		base, since = t.lastImage, t.lastRevision
	}
	keys := make([]string, 0, len(t.dirty))
	for k, revision := range t.dirty {
		if revision > since {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []Entry
	if len(keys) > len(base)/8 {
		// Dense changes use the unchanged FULL native iterator, not a key cap.
		out = t.scanImage(v)
	} else {
		out = make([]Entry, 0, len(base)+len(keys))
		i, j := 0, 0
		for i < len(base) || j < len(keys) {
			if j == len(keys) || (i < len(base) && string(base[i].Key) < keys[j]) {
				out = append(out, base[i])
				i++
				continue
			}
			k := keys[j]
			j++
			if i < len(base) && string(base[i].Key) == k {
				i++
			}
			value := t.read(v, []byte(k))
			if value != nil {
				out = append(out, Entry{[]byte(k), value})
			}
		}
	}
	t.lastView, t.lastImage, t.lastRevision, t.lastGeneration = v, out, t.revision, t.generation
	return out, t.err
}
func (t *Tracker) scanImage(v *accountKV) []Entry {
	entries := make([]Entry, 0)
	it := v.raw.Iterator(nil, nil)
	defer it.Close()
	for ; it.Valid(); it.Next() {
		k := clone(it.Key())
		if len(k) == 0 || (len(entries) > 0 && bytes.Compare(entries[len(entries)-1].Key, k) >= 0) {
			t.fail()
			break
		}
		entries = append(entries, Entry{k, clone(it.Value())})
	}
	return entries
}

// ObserveWrites receives every successful source Set/Delete, including net-zero
// and repeated writes. Native cache flushes have a separate source/view journal.
func (t *Tracker) ObserveWrites(active func() bool, sink func(uint64, uint64, []byte, []byte, []byte, string)) {
	t.writeActive, t.writeSink = active, sink
}
func (t *Tracker) ViewID(store st.KVStore) (uint64, error) {
	token, err := t.Token(store)
	if err != nil {
		return 0, err
	}
	return token.view.id, nil
}
func (t *Tracker) ObserveFlushes(sink func(uint64, uint64)) { t.flushSink = sink }
func (t *Tracker) candidate(k []byte) {
	if !t.images {
		return
	}
	if len(k) == 0 {
		t.fail()
		return
	}
	t.dirty[string(clone(k))] = t.revision
}
func (t *Tracker) commitBoundary(root st.KVStore) {
	if len(t.fences) != 0 {
		t.fail()
	}
	// Capture the actual commit ROOT before native serialization, not whichever
	// speculative view happens to be imaged first in the next generation.
	var committed []Entry
	if t.images {
		var err error
		committed, err = t.Image(root)
		if err != nil {
			t.fail()
		}
	}
	t.touch()
	t.generation++
	if t.images {
		t.image, t.imageGeneration = committed, t.generation
		t.dirty = make(map[string]uint64)
		t.lastView, t.lastImage = nil, nil
	}
}
