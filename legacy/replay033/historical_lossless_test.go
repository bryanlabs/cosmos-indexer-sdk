package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This runs only a NEW observed recording on an exclusive byte-copy. The
// independent FULL native/observed historical fixtures are retained unchanged.
func TestHistoricalLosslessRecording(t *testing.T) {
	dir, source, cache, target := os.Getenv("HUB1_LOSSLESS_OUT"), os.Getenv("HUB1_LOSSLESS_SOURCE"), os.Getenv("HUB1_LOSSLESS_CACHE"), os.Getenv("HUB1_LOSSLESS_TARGET")
	if dir == "" || source == "" || cache == "" || target == "" {
		t.Skip("explicit private source/output required")
	}
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("exclusive output required")
	}
	before, e := privateTree(source)
	if e != nil {
		t.Fatal("source manifest refused")
	}
	cacheBefore, e := privateTree(cache)
	if e != nil {
		t.Fatal("cache manifest refused")
	}
	defer func() {
		after, e := privateTree(source)
		cacheAfter, ce := privateTree(cache)
		result := struct {
			SourceBefore, SourceAfter, CacheBefore, CacheAfter map[string]string
			Unchanged                                          bool
		}{before, after, cacheBefore, cacheAfter, e == nil && ce == nil && reflect.DeepEqual(before, after) && reflect.DeepEqual(cacheBefore, cacheAfter)}
		f, err := os.OpenFile(filepath.Join(dir, "preservation.private.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			json.NewEncoder(f).Encode(result)
			f.Sync()
			f.Close()
		}
		if !result.Unchanged {
			t.Error("source preservation failed")
		}
	}()
	home := filepath.Join(dir, "observed-home")
	if copyPrivateTree(source, home) != nil {
		t.Fatal("private clone refused")
	}
	args := []string{"--cache", cache, "--chain", "cosmoshub-1", "--genesis", filepath.Join(cache, "genesis", "genesis.cosmoshub-1.json"), "--home", home, "--to", target, "--readers", "2", "--compare-results=true", "--pruning", "recent", "--verify-target-next-header", "--native-audit", filepath.Join(dir, "observed-audit.ndjson"), "--coin-records", filepath.Join(dir, "observed-movements.ndjson"), "--lossless-recording=true"}
	if run(args) != nil {
		t.Fatal("new actual lossless recording refused")
	}
	if os.Getenv("HUB1_LOSSLESS_EXPORT") == "true" {
		if exportZero([]string{"--home", home, "--out", filepath.Join(dir, "observed-export.json"), "--require-height", target, "--coin-records", filepath.Join(dir, "observed-export-movements.ndjson"), "--lossless-recording=true"}) != nil {
			t.Fatal("new lossless export refused")
		}
	}
}
