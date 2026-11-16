package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	distr "github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	dbm "github.com/tendermint/tendermint/libs/db"
)

// This is exactly the persistent store set mounted by pinned Gaia NewGaiaApp.
// Transient stores never enter commit-info. No application constructor or
// cmn.Exit path is used when inspecting untrusted persisted metadata.
func persistentNames() []string {
	return []string{"main", auth.StoreKey, staking.StoreKey, mint.StoreKey, distr.StoreKey, slashing.StoreKey, gov.StoreKey, auth.FeeStoreKey, params.StoreKey}
}

type commitCore struct{ CommitID st.CommitID }
type storeInfo struct {
	Name string
	Core commitCore
}
type commitInfo struct {
	Version    int64
	StoreInfos []storeInfo
}

func loadedRoot(db dbm.DB) (height int64, hash []byte, err error) {
	defer func() {
		if recover() != nil {
			height = 0
			hash = nil
			err = errors.New("checkpoint store metadata or loaded tree invalid")
		}
	}()
	if codec.UnmarshalBinaryLengthPrefixed(db.Get([]byte("s/latest")), &height) != nil || height < 1 {
		return 0, nil, errors.New("committed checkpoint metadata invalid")
	}
	var info commitInfo
	if codec.UnmarshalBinaryLengthPrefixed(db.Get([]byte(fmt.Sprintf("s/%d", height))), &info) != nil || info.Version != height {
		return 0, nil, errors.New("checkpoint commit info invalid")
	}
	names := persistentNames()
	if len(info.StoreInfos) != len(names) {
		return 0, nil, errors.New("checkpoint persistent store set differs")
	}
	expected := map[string]st.CommitID{}
	for _, i := range info.StoreInfos {
		if _, ok := expected[i.Name]; ok {
			return 0, nil, errors.New("checkpoint repeats a persistent store")
		}
		expected[i.Name] = i.Core.CommitID
	}
	cms := store.NewCommitMultiStore(db)
	keys := make([]*st.KVStoreKey, 0, len(names))
	for _, name := range names {
		if _, ok := expected[name]; !ok {
			return 0, nil, errors.New("checkpoint persistent store missing")
		}
		k := st.NewKVStoreKey(name)
		keys = append(keys, k)
		cms.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
	}
	if cms.LoadVersion(height) != nil {
		return 0, nil, errors.New("checkpoint persistent stores cannot load")
	}
	for _, key := range keys {
		actual := cms.GetCommitStore(key).LastCommitID()
		want := expected[key.Name()]
		if actual.Version != want.Version || !bytes.Equal(actual.Hash, want.Hash) {
			return 0, nil, errors.New("loaded persistent store differs from committed root")
		}
	}
	return height, cms.LastCommitID().Hash, nil
}

// The source lock is opened without O_CREATE and held shared while all source
// bytes are copied. A real writer's exclusive flock refuses this operation.
// LevelDB only opens the private retained clone, so even a malformed source
// cannot acquire a new LOCK/LOG file or other mutation through its opener.
func cloneOffline(home string) (copyHome string, err error) {
	dir := filepath.Join(home, "application.db")
	lock := filepath.Join(dir, "LOCK")
	s, e := os.Lstat(lock)
	if e != nil || !s.Mode().IsRegular() {
		return "", errors.New("source checkpoint lock must already exist")
	}
	f, e := os.Open(lock)
	if e != nil {
		return "", errors.New("source checkpoint lock cannot open read-only")
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		return "", errors.New("source checkpoint is not an unlocked offline database")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	files, e := os.ReadDir(dir)
	if e != nil {
		return "", errors.New("source checkpoint cannot list")
	}
	copyHome, e = os.MkdirTemp("", "cosmos-checkpoint-proof-")
	if e != nil {
		return "", errors.New("private checkpoint copy cannot create")
	}
	dest := filepath.Join(copyHome, "application.db")
	if os.Mkdir(dest, 0700) != nil {
		return "", errors.New("private checkpoint copy cannot create")
	}
	for _, entry := range files {
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() {
			return "", errors.New("source checkpoint contains non-regular entry")
		}
		in, e := os.Open(filepath.Join(dir, entry.Name()))
		if e != nil {
			return "", errors.New("source checkpoint file cannot read")
		}
		out, e := os.OpenFile(filepath.Join(dest, entry.Name()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			in.Close()
			return "", errors.New("private checkpoint file cannot create")
		}
		_, e = io.Copy(out, in)
		in.Close()
		closeErr := out.Close()
		if e != nil || closeErr != nil {
			return "", errors.New("private checkpoint copy failed")
		}
	}
	// Replay bookkeeping lives outside application.db and is read independently.
	// The clone is retained even on failure; no evidence cleanup is implicit.
	return copyHome, nil
}
