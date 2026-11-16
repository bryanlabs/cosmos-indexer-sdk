package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	dbm "github.com/tendermint/tendermint/libs/db"
)

// The genesis range may keep its committed tree on disk instead of in memory.
// Every commit root must be identical for the same ordered patches.
func TestGenesisDiskStoreCommitsMatchMemoryStore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "native-home")
	must(os.Mkdir(home, 0700))
	db, e := dbm.NewGoLevelDB("application", home)
	must(e)
	defer db.Close()
	memory, disk := newNative(), nativeDB(db)
	need(disk.cms.LastCommitID().Version == 0 && len(disk.cms.LastCommitID().Hash) == 0, "new disk store not empty")
	key := func(i int) []byte {
		k := make([]byte, 9)
		k[0] = 250
		binary.BigEndian.PutUint64(k[1:], uint64(i))
		return k
	}
	var memoryTime, diskTime time.Duration
	for version := 1; version <= 130; version++ {
		p := blank()
		for _, name := range names {
			for j := 0; j < 40; j++ {
				id := version*100 + j
				p[name] = append(p[name], pair{Key: key(id), Before: raw(nil), Value: raw([]byte{byte(version), byte(j)})})
			}
		}
		if version > 1 {
			old := (version-1)*100 + 3
			p["acc"] = append([]pair{{Key: key(old), Before: raw([]byte{byte(version - 1), 3}), Value: raw(nil)}}, p["acc"]...)
		}
		start := time.Now()
		memory.apply(p)
		a := memory.cms.Commit()
		memoryTime += time.Since(start)
		start = time.Now()
		disk.apply(p)
		b := disk.cms.Commit()
		diskTime += time.Since(start)
		need(a.Version == b.Version && bytes.Equal(a.Hash, b.Hash), "disk and memory commit roots differ")
	}
	sameRaw(memory, disk)
	t.Logf("130 commits: memory store %s, disk store %s", memoryTime, diskTime)
}
