package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func checkpointFixture(t *testing.T) (*native, *native, nativeCheckpointState) {
	t.Helper()
	phase, committed := newNative(), newNative()
	for h := int64(1); h <= 4; h++ {
		for _, name := range names {
			kv := committed.cms.GetKVStore(committed.keys[name])
			kv.Set([]byte{byte(h)}, []byte{byte(h + 10)})
			if h > 2 {
				kv.Delete([]byte{byte(h - 2)})
			}
		}
		committed.cms.Commit()
	}
	for _, name := range names {
		kv := phase.cms.GetKVStore(phase.keys[name])
		kv.Set([]byte("working-only"), []byte{})
	}
	for _, key := range phase.trans {
		phase.cms.GetKVStore(key).Set([]byte("transient"), []byte("retained"))
	}
	id := committed.cms.LastCommitID()
	s := nativeCheckpointState{Schema: "native033-resume-v1", Height: 4, First: 1, Through: 8, Phases: 17, Transactions: 9, Prior: id.Hash, OpeningChecked: true, AssetFactRows: 2, AssetBlocks: 1}
	return phase, committed, s
}
func TestNativeCheckpointExactResume(t *testing.T) {
	phase, committed, s := checkpointFixture(t)
	root := t.TempDir()
	facts, e := os.OpenFile(filepath.Join(root, "facts-live"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer facts.Close()
	if _, e = facts.Write([]byte("private synthetic facts\n")); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "checkpoint")
	ack := saveNativeCheckpoint(path, s, phase, committed, facts)
	if ack.Kind != "checkpoint" || ack.Height != 4 {
		t.Fatal("incorrect ack")
	}
	out := filepath.Join(root, "resume")
	if e = os.Mkdir(out, 0700); e != nil {
		t.Fatal(e)
	}
	restoredPhase, restoredCommit := newNative(), newNative()
	got := loadNativeCheckpoint(path, restoredPhase, &restoredCommit, filepath.Join(out, "receipt"))
	defer restoredCommit.db.Close()
	sameRaw(phase, restoredPhase)
	sameRaw(committed, restoredCommit)
	if got.Phases != s.Phases || got.Transactions != s.Transactions || got.AssetFactRows != s.AssetFactRows || got.AssetBlocks != s.AssetBlocks || !bytes.Equal(got.Prior, s.Prior) {
		t.Fatal("counter restore mismatch")
	}
	for _, name := range names {
		if !equal(committed.cms.GetCommitKVStore(committed.keys[name]).LastCommitID(), restoredCommit.cms.GetCommitKVStore(restoredCommit.keys[name]).LastCommitID()) {
			t.Fatal("substore version differs")
		}
	}
	for name, key := range phase.trans {
		if !bytes.Equal(phase.cms.GetKVStore(key).Get([]byte("transient")), restoredPhase.cms.GetKVStore(restoredPhase.trans[name]).Get([]byte("transient"))) {
			t.Fatal("transient state differs")
		}
	}
	restoredFacts, e := os.ReadFile(filepath.Join(out, "native-stake-reductions.private.jsonl"))
	if e != nil || string(restoredFacts) != "private synthetic facts\n" {
		t.Fatal("facts restore mismatch")
	}
	// A restored outer output may contain an uncheckpointed tail. Only a
	// matching checkpoint prefix may be retained and truncated.
	factsPath := filepath.Join(out, "native-stake-reductions.private.jsonl")
	f, e := os.OpenFile(factsPath, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte("uncheckpointed tail\n")); e != nil {
		t.Fatal(e)
	}
	f.Close()
	p2, c2 := newNative(), newNative()
	loadNativeCheckpoint(path, p2, &c2, filepath.Join(out, "receipt"))
	c2.db.Close()
	if checkpointDigest(factsPath) != got.Files["facts.jsonl"] {
		t.Fatal("facts tail was not removed")
	}
	for h := 5; h <= 8; h++ {
		for _, n := range []*native{committed, restoredCommit} {
			for _, name := range names {
				n.cms.GetKVStore(n.keys[name]).Set([]byte{byte(h)}, []byte{byte(h + 20)})
			}
		}
		if !equal(committed.cms.Commit(), restoredCommit.cms.Commit()) {
			t.Fatal("resumed IAVL commitment differs")
		}
	}
}
func TestNativeCheckpointRejectCorruption(t *testing.T) {
	for _, member := range []string{"native.json", "phase.raw.jsonl", "committed.db.jsonl"} {
		t.Run(member, func(t *testing.T) {
			phase, committed, s := checkpointFixture(t)
			s.AssetFactRows, s.AssetBlocks = 0, 0
			root := t.TempDir()
			path := filepath.Join(root, "checkpoint")
			saveNativeCheckpoint(path, s, phase, committed, nil)
			f, e := os.OpenFile(filepath.Join(path, member), os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.Write([]byte(" "))
			if e != nil {
				t.Fatal(e)
			}
			f.Close()
			defer func() {
				if _, ok := recover().(refusal); !ok {
					t.Error("corrupt snapshot was not refused")
				}
			}()
			p, c := newNative(), newNative()
			loadNativeCheckpoint(path, p, &c, filepath.Join(root, "receipt"))
		})
	}
}
func TestNativeCheckpointImmutableDestination(t *testing.T) {
	phase, committed, s := checkpointFixture(t)
	s.AssetFactRows, s.AssetBlocks = 0, 0
	root := t.TempDir()
	path := filepath.Join(root, "checkpoint")
	saveNativeCheckpoint(path, s, phase, committed, nil)
	before := checkpointDigest(filepath.Join(path, "native.json"))
	defer func() {
		if _, ok := recover().(refusal); !ok {
			t.Error("existing snapshot overwritten")
		}
		if before != checkpointDigest(filepath.Join(path, "native.json")) {
			t.Error("snapshot changed")
		}
	}()
	saveNativeCheckpoint(path, s, phase, committed, nil)
}
func TestNativeCheckpointAckJSON(t *testing.T) {
	b, e := json.Marshal(nativeCheckpointAck{Kind: "checkpoint", Height: 7})
	if e != nil {
		t.Fatal(e)
	}
	var got struct {
		Kind   string `json:"kind"`
		Height int64  `json:"height"`
	}
	if e = json.Unmarshal(b, &got); e != nil || got.Kind != "checkpoint" || got.Height != 7 {
		t.Fatal("protocol differs")
	}
}
