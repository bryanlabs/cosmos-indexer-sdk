package main

import (
	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordingHomeAttemptCannotSilentlyResume(t *testing.T) {
	home := t.TempDir()
	if createRecordingAttempt(home) != nil {
		t.Fatal("fresh private attempt refused")
	}
	path := filepath.Join(home, ".native033-recording-attempt")
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("attempt evidence absent")
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private attempt permission")
	}
	if createRecordingAttempt(home) == nil {
		t.Fatal("retry could skip missing committed evidence")
	}
	after, e := os.ReadFile(path)
	if e != nil || string(before) != string(after) {
		t.Fatal("failed retry changed attempt evidence")
	}
}
func TestRecordedNativeCacheDifferencesFailClosed(t *testing.T) {
	reference := abci.ResponseDeliverTx{GasWanted: 7, GasUsed: 5, Data: []byte{9}, Log: "native", Tags: []cmn.KVPair{{Key: []byte("tag"), Value: []byte("native")}}}
	cache := &cachedResults{deliver: []*abci.ResponseDeliverTx{&reference}}
	if requireRecordedResults(cache, abci.ResponseBeginBlock{}, []*abci.ResponseDeliverTx{&reference}, abci.ResponseEndBlock{}) != nil {
		t.Fatal("exact native response refused")
	}
	for _, change := range []func(*abci.ResponseDeliverTx){func(r *abci.ResponseDeliverTx) { r.GasWanted++ }, func(r *abci.ResponseDeliverTx) { r.GasUsed++ }, func(r *abci.ResponseDeliverTx) { r.Code++ }, func(r *abci.ResponseDeliverTx) { r.Data = []byte{1} }, func(r *abci.ResponseDeliverTx) { r.Log = "changed" }, func(r *abci.ResponseDeliverTx) { r.Tags = nil }} {
		r := reference
		change(&r)
		if requireRecordedResults(cache, abci.ResponseBeginBlock{}, []*abci.ResponseDeliverTx{&r}, abci.ResponseEndBlock{}) == nil {
			t.Fatal("changed native response accepted")
		}
	}
	if requireRecordedResults(nil, abci.ResponseBeginBlock{}, nil, abci.ResponseEndBlock{}) == nil {
		t.Fatal("missing native results accepted")
	}
	if requireRecordedResults(cache, abci.ResponseBeginBlock{Tags: reference.Tags}, []*abci.ResponseDeliverTx{&reference}, abci.ResponseEndBlock{}) == nil {
		t.Fatal("changed native begin tags accepted")
	}
	if requireRecordedResults(cache, abci.ResponseBeginBlock{}, []*abci.ResponseDeliverTx{&reference}, abci.ResponseEndBlock{ValidatorUpdates: []abci.ValidatorUpdate{{Power: 1}}}) == nil {
		t.Fatal("changed validator updates accepted")
	}
}
