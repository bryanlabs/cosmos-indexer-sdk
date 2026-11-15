package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	sdk "github.com/cosmos/cosmos-sdk/types"
	dbm "github.com/tendermint/tendermint/libs/db"
)

// Commit DB bytes preserve IAVL node versions, pruning metadata and multistore
// commit info. The working phase has never committed and is saved separately,
// not assumed equal to the committed state (aborted phases can differ).
type nativeCheckpointState struct {
	Schema                                       string
	Height, First, Through, Phases, Transactions int64
	AssetFactRows, AssetBlocks                   int64
	Prior                                        []byte
	OpeningChecked                               bool
	SourceAnchor                                 history
	ValidatorHistoryPin, EvidenceCapturePin      string
	Commit                                       sdk.CommitID
	Files                                        map[string]string
	FactsPresent                                 bool
	FactsOffset                                  int64
}
type nativeCheckpointAck struct {
	Kind           string `json:"kind"`
	CheckpointPath string `json:"checkpoint_path"`
	Height         int64  `json:"height"`
	NativeSHA256   string `json:"native_sha256"`
}
type checkpointPair struct {
	Store      string
	Key, Value []byte
}

func checkpointDigest(path string) string {
	f, e := os.Open(path)
	must(e)
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	must(e)
	return hex.EncodeToString(h.Sum(nil))
}
func checkpointFile(path string, write func(*os.File)) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(e)
	defer f.Close()
	write(f)
	must(f.Sync())
	must(f.Close())
}
func syncCheckpointDir(path string) {
	f, e := os.Open(path)
	must(e)
	defer f.Close()
	must(f.Sync())
}
func saveNativeCheckpoint(path string, s nativeCheckpointState, phase, committed *native, facts *os.File) nativeCheckpointAck {
	need(filepath.IsAbs(path), "absolute native checkpoint path required")
	_, e := os.Lstat(path)
	need(os.IsNotExist(e), "native checkpoint destination already exists")
	parent := filepath.Dir(path)
	temp, e := os.MkdirTemp(parent, ".native-checkpoint-")
	must(e)
	s.Commit = committed.cms.LastCommitID()
	need(s.Commit.Version == s.Height && bytes.Equal(s.Commit.Hash, s.Prior), "native checkpoint commitment differs")
	s.Files = map[string]string{}
	checkpointFile(filepath.Join(temp, "committed.db.jsonl"), func(f *os.File) {
		enc := json.NewEncoder(f)
		it := committed.db.Iterator(nil, nil)
		defer it.Close()
		for ; it.Valid(); it.Next() {
			must(enc.Encode(checkpointPair{Key: it.Key(), Value: it.Value()}))
		}
	})
	checkpointFile(filepath.Join(temp, "phase.raw.jsonl"), func(f *os.File) {
		enc := json.NewEncoder(f)
		dump := func(name string, key sdk.StoreKey) {
			it := phase.cms.GetKVStore(key).Iterator(nil, nil)
			defer it.Close()
			for ; it.Valid(); it.Next() {
				must(enc.Encode(checkpointPair{Store: name, Key: it.Key(), Value: it.Value()}))
			}
		}
		for _, name := range names {
			dump(name, phase.keys[name])
		}
		for name, key := range phase.trans {
			dump("transient:"+name, key)
		}
	})
	for _, name := range []string{"committed.db.jsonl", "phase.raw.jsonl"} {
		s.Files[name] = checkpointDigest(filepath.Join(temp, name))
	}
	if facts != nil {
		must(facts.Sync())
		info, e := facts.Stat()
		must(e)
		s.FactsPresent = true
		s.FactsOffset = info.Size()
		checkpointFile(filepath.Join(temp, "facts.jsonl"), func(f *os.File) {
			in, e := os.Open(facts.Name())
			must(e)
			defer in.Close()
			n, e := io.Copy(f, in)
			must(e)
			need(n == s.FactsOffset, "native facts changed at checkpoint")
		})
		s.Files["facts.jsonl"] = checkpointDigest(filepath.Join(temp, "facts.jsonl"))
	}
	checkpointFile(filepath.Join(temp, "native.json"), func(f *os.File) { must(json.NewEncoder(f).Encode(s)) })
	ack := nativeCheckpointAck{Kind: "checkpoint", CheckpointPath: path, Height: s.Height, NativeSHA256: checkpointDigest(filepath.Join(temp, "native.json"))}
	checkpointFile(filepath.Join(temp, "checkpoint.complete.json"), func(f *os.File) { must(json.NewEncoder(f).Encode(ack)) })
	syncCheckpointDir(temp)
	must(os.Rename(temp, path))
	syncCheckpointDir(parent)
	return ack
}
func decodeCheckpointFile(path string, out interface{}) {
	f, e := os.Open(path)
	must(e)
	defer f.Close()
	d := json.NewDecoder(f)
	must(d.Decode(out))
	var extra interface{}
	need(d.Decode(&extra) == io.EOF, "trailing checkpoint metadata")
}
func loadNativeCheckpoint(path string, phase *native, committed **native, receipt string) nativeCheckpointState {
	var ack nativeCheckpointAck
	decodeCheckpointFile(filepath.Join(path, "checkpoint.complete.json"), &ack)
	need(ack.Kind == "checkpoint" && ack.NativeSHA256 == checkpointDigest(filepath.Join(path, "native.json")), "native checkpoint manifest hash differs")
	var s nativeCheckpointState
	decodeCheckpointFile(filepath.Join(path, "native.json"), &s)
	need(s.Schema == "native033-resume-v1" && s.Height == ack.Height && s.Height >= s.First && s.Height <= s.Through && s.OpeningChecked, "invalid native checkpoint boundary")
	expected := 2
	if s.FactsPresent {
		expected++
	}
	need(len(s.Files) == expected, "native checkpoint member coverage differs")
	for _, name := range []string{"committed.db.jsonl", "phase.raw.jsonl"} {
		need(s.Files[name] != "" && s.Files[name] == checkpointDigest(filepath.Join(path, name)), "native checkpoint state hash differs")
	}
	if s.FactsPresent {
		need(s.Files["facts.jsonl"] != "" && s.Files["facts.jsonl"] == checkpointDigest(filepath.Join(path, "facts.jsonl")), "native checkpoint facts hash differs")
	}
	// Only a fresh private DB is opened. Never open the immutable checkpoint DB.
	home, e := os.MkdirTemp(filepath.Dir(receipt), ".native-resume-db-")
	must(e)
	db, e := dbm.NewGoLevelDB("application", home)
	must(e)
	load := func(name string, put func(checkpointPair)) {
		f, e := os.Open(filepath.Join(path, name))
		must(e)
		defer f.Close()
		dec := json.NewDecoder(f)
		for {
			var p checkpointPair
			e := dec.Decode(&p)
			if e == io.EOF {
				break
			}
			must(e)
			put(p)
		}
	}
	load("committed.db.jsonl", func(p checkpointPair) { need(p.Store == "", "invalid committed DB member"); db.Set(p.Key, p.Value) })
	*committed = nativeDB(db)
	need(equal((*committed).cms.LastCommitID(), s.Commit) && s.Commit.Version == s.Height && bytes.Equal(s.Commit.Hash, s.Prior), "restored native commitment differs")
	load("phase.raw.jsonl", func(p checkpointPair) {
		var key sdk.StoreKey
		if k, ok := phase.keys[p.Store]; ok {
			key = k
		} else {
			for name, k := range phase.trans {
				if p.Store == "transient:"+name {
					key = k
				}
			}
		}
		need(key != nil && len(p.Key) > 0, "unknown working checkpoint store")
		value := p.Value
		if value == nil {
			value = []byte{}
		}
		phase.cms.GetKVStore(key).Set(p.Key, value)
	})
	if s.FactsPresent {
		target := filepath.Join(filepath.Dir(receipt), "native-stake-reductions.private.jsonl")
		info, e := os.Lstat(target)
		if os.IsNotExist(e) {
			checkpointFile(target, func(f *os.File) {
				in, e := os.Open(filepath.Join(path, "facts.jsonl"))
				must(e)
				defer in.Close()
				n, e := io.Copy(f, in)
				must(e)
				need(n == s.FactsOffset, "restored native facts offset differs")
			})
		} else {
			must(e)
			need(info.Mode().IsRegular() && info.Size() >= s.FactsOffset, "existing native facts are not a complete regular prefix")
			f, e := os.OpenFile(target, os.O_RDWR, 0600)
			must(e)
			defer f.Close()
			digest := sha256.New()
			n, e := io.Copy(digest, io.LimitReader(f, s.FactsOffset))
			must(e)
			need(n == s.FactsOffset && hex.EncodeToString(digest.Sum(nil)) == s.Files["facts.jsonl"], "existing native facts prefix differs")
			must(f.Truncate(s.FactsOffset))
			must(f.Sync())
		}
		syncCheckpointDir(filepath.Dir(receipt))
	} else {
		need(s.FactsOffset == 0 && s.AssetFactRows == 0 && s.AssetBlocks == 0, "missing native facts state")
	}
	return s
}
