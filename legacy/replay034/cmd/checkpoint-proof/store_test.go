package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	"github.com/tendermint/tendermint/types"
)

func fixtureStore(t *testing.T, db dbm.DB) []byte {
	t.Helper()
	cms := store.NewCommitMultiStore(db)
	keys := []*st.KVStoreKey{}
	for _, n := range persistentNames() {
		k := st.NewKVStoreKey(n)
		keys = append(keys, k)
		cms.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
	}
	if cms.LoadLatestVersion() != nil {
		t.Fatal("fixture load failed")
	}
	for _, k := range keys {
		cms.GetKVStore(k).Set([]byte("synthetic"), []byte("fixture"))
	}
	return cms.Commit().Hash
}
func TestLoadedStoreRootsNotOnlyMetadata(t *testing.T) {
	db := dbm.NewMemDB()
	hash := fixtureStore(t, db)
	h, got, e := loadedRoot(db)
	if e != nil || h != 1 || !bytes.Equal(hash, got) {
		t.Fatal("valid fixture rejected")
	}
	// Corruption is confined to this synthetic in-memory test fixture.
	prefix := []byte("s/k:acc/")
	it := db.Iterator(prefix, []byte("s/k:acc0"))
	keys := [][]byte{}
	for ; it.Valid(); it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	it.Close()
	for _, k := range keys {
		db.Delete(k)
	}
	if _, _, e = loadedRoot(db); e == nil {
		t.Fatal("missing account tree accepted using metadata hash")
	}
}
func TestMalformedMetadataScrubbed(t *testing.T) {
	db := dbm.NewMemDB()
	fixtureStore(t, db)
	raw := []byte("privacy-marker")
	db.Set([]byte("s/1"), append([]byte{byte(len(raw))}, raw...))
	if _, _, e := loadedRoot(db); e == nil || strings.Contains(e.Error(), "privacy-marker") {
		t.Fatal("malformed metadata accepted or disclosed")
	}
}
func TestNoMissingLockCreation(t *testing.T) {
	h := t.TempDir()
	dir := filepath.Join(h, "application.db")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture mkdir")
	}
	if _, e := cloneOffline(h); e == nil {
		t.Fatal("missing source lock accepted")
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 0 {
		t.Fatal("source file created")
	}
}
func writeCapture(t *testing.T, root string, b *types.Block) *types.BlockMeta {
	t.Helper()
	meta := types.NewBlockMeta(b, b.MakePartSet(65536))
	raw, e := codec.MarshalJSON(ctypes.ResultBlock{Block: b, BlockMeta: meta})
	if e != nil {
		t.Fatal("fixture encode")
	}
	envelope, _ := json.Marshal(struct {
		Result json.RawMessage `json:"result"`
	}{raw})
	dir := filepath.Join(root, "cosmoshub-2", "0")
	if os.MkdirAll(dir, 0700) != nil {
		t.Fatal("fixture dir")
	}
	name := "1-block.raw.gz"
	if b.Height == 2 {
		name = "2-block.raw.gz"
	}
	f, e := os.Create(filepath.Join(dir, name))
	if e != nil {
		t.Fatal("fixture file")
	}
	z := gzip.NewWriter(f)
	z.Write(envelope)
	z.Close()
	f.Close()
	return meta
}
func TestCompleteCheckpointAndActualLockRefusal(t *testing.T) {
	home, cache := t.TempDir(), t.TempDir()
	db, e := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if e != nil {
		t.Fatal("fixture db")
	}
	hash := fixtureStore(t, db)
	db.Close()
	first := types.MakeBlock(1, nil, &types.Commit{}, nil)
	first.ChainID = "cosmoshub-2"
	first.Time = time.Unix(1, 0).UTC()
	first.ProposerAddress = bytes.Repeat([]byte{1}, 20)
	first.ValidatorsHash = bytes.Repeat([]byte{2}, 32)
	first.NextValidatorsHash = first.ValidatorsHash
	meta := writeCapture(t, cache, first)
	last := &types.Commit{BlockID: meta.BlockID, Precommits: []*types.CommitSig{nil}}
	next := types.MakeBlock(2, nil, last, nil)
	next.ChainID = first.ChainID
	next.Time = time.Unix(2, 0).UTC()
	next.LastBlockID = meta.BlockID
	next.ProposerAddress = first.ProposerAddress
	next.ValidatorsHash = first.NextValidatorsHash
	next.NextValidatorsHash = first.NextValidatorsHash
	next.AppHash = hash
	writeCapture(t, cache, next)
	status, _ := json.Marshal(map[string]interface{}{"release": release, "chain": "cosmoshub-2", "last_height_committed": 1, "state_hash_verified_through": 0})
	os.WriteFile(filepath.Join(home, "status.json"), status, 0600)
	os.WriteFile(filepath.Join(home, "tail.json"), []byte(`{"height":1,"results_hash":null}`), 0600)
	if r, e := prove(cache, "cosmoshub-2", home); e != nil || r.Verified != 1 {
		t.Fatal("valid full proof rejected:", e)
	}
	writer, e := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if e != nil {
		t.Fatal("fixture reopen")
	}
	defer writer.Close()
	if _, e := prove(cache, "cosmoshub-2", home); e == nil || e.Error() != "source checkpoint is not an unlocked offline database" {
		t.Fatal("writer did not fail at source lock check")
	}
}
