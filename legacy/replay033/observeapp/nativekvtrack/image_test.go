package nativekvtrack

import (
	"bytes"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"reflect"
	"testing"
)

func TestImagesAcrossCommitAndLoadBoundaries(t *testing.T) {
	tracker := NewTracker()
	key := st.NewKVStoreKey("all-native")
	if tracker.Bind(key) != nil {
		t.Fatal("bind")
	}
	cms := tracker.WrapCommitMultiStore(store.NewCommitMultiStore(dbm.NewMemDB()))
	cms.MountStoreWithDB(key, st.StoreTypeIAVL, nil)
	cms.SetPruning(st.NewPruningOptions(100, 0))
	if cms.LoadLatestVersion() != nil {
		t.Fatal("load")
	}
	tracker.EnableImages()
	check := func() {
		view := cms.GetKVStore(key)
		image, err := tracker.Image(view)
		if err != nil {
			t.Fatal("image")
		}
		oracle := make([]Entry, 0)
		it := view.Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			oracle = append(oracle, Entry{clone(it.Key()), clone(it.Value())})
		}
		it.Close()
		if !reflect.DeepEqual(image, oracle) {
			t.Fatal("complete root image differs")
		}
	}
	kv := cms.GetKVStore(key)
	kv.Set([]byte{0xff, 1}, []byte{})
	for i := 0; i < 32; i++ {
		kv.Set([]byte{0x7a, byte(i)}, []byte{byte(i)})
	}
	check()
	// A sparse mutation after the most recent image must be included by the
	// commit-time incremental merge, before ANY next-generation Image call.
	kv.Set([]byte{0x7a, 0}, []byte{0x55})
	cms.Commit()
	expected := make([]Entry, 0)
	iterator := cms.GetKVStore(key).Iterator(nil, nil)
	for ; iterator.Valid(); iterator.Next() {
		expected = append(expected, Entry{clone(iterator.Key()), clone(iterator.Value())})
	}
	iterator.Close()
	if !reflect.DeepEqual(tracker.image, expected) {
		t.Fatal("commit anchor does not equal complete native root before any new-generation image")
	}
	// A speculative write before the first post-commit image must survive
	// imaging the clean parent first, and then switching to that dirty child.
	child := cms.CacheMultiStore().GetKVStore(key)
	child.Set([]byte{0xfe, 9}, []byte{8})
	check()
	image, e := tracker.Image(child)
	if e != nil {
		t.Fatal("post-commit child image")
	}
	found := false
	for _, pair := range image {
		if bytes.Equal(pair.Key, []byte{0xfe, 9}) && bytes.Equal(pair.Value, []byte{8}) {
			found = true
		}
	}
	if !found {
		t.Fatal("first new-generation snapshot forgot a speculative write")
	}
	check()
	kv = cms.GetKVStore(key)
	kv.Set([]byte{0xff, 1}, []byte{2})
	kv.Set([]byte{0, 1}, []byte{3})
	check()
	cms.Commit()
	check()
	if cms.LoadVersion(1) != nil {
		t.Fatal("historical native load")
	}
	check()
	if !bytes.Equal(cms.GetKVStore(key).Get([]byte{0xff, 1}), []byte{}) || cms.GetKVStore(key).Has([]byte{0, 1}) {
		t.Fatal("load did not restore complete bytes")
	}
}
