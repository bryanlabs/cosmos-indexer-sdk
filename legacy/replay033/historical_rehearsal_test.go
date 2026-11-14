package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func privateTree(path string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fs.ErrInvalid
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, f)
		f.Close()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		out[relative] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	return out, err
}
func copyPrivateTree(source, dest string) error {
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		to := filepath.Join(dest, rel)
		if e.IsDir() {
			return os.Mkdir(to, 0700)
		}
		if !e.Type().IsRegular() {
			return fs.ErrInvalid
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

// A new exclusive local directory, two independently copied immutable native
// checkpoints, no SQL, cluster resources or production paths. Scope is fixed.
func TestHistoricalImmutableRehearsal(t *testing.T) {
	dir, inputs := os.Getenv("HUB1_PRIVATE_REHEARSAL"), os.Getenv("HUB1_PRIVATE_INPUTS")
	if dir == "" || inputs == "" {
		t.Skip("explicit private historical rehearsal paths not supplied")
	}
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("rehearsal path must be exclusive")
	}
	source := filepath.Join(inputs, "checkpoints", "400000")
	cache := filepath.Join(inputs, "cache")
	before, e := privateTree(source)
	if e != nil {
		t.Fatal("immutable checkpoint manifest failed")
	}
	cacheBefore, e := privateTree(cache)
	if e != nil {
		t.Fatal("immutable cache manifest failed")
	}
	defer func() {
		after, e := privateTree(source)
		cacheAfter, ce := privateTree(cache)
		result := struct {
			SourceBefore, SourceAfter, CacheBefore, CacheAfter map[string]string
			SourceUnchanged, CacheUnchanged                    bool
		}{before, after, cacheBefore, cacheAfter, e == nil && reflect.DeepEqual(before, after), ce == nil && reflect.DeepEqual(cacheBefore, cacheAfter)}
		f, e := os.OpenFile(filepath.Join(dir, "preservation.private.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			json.NewEncoder(f).Encode(result)
			f.Sync()
			f.Close()
		}
		if !result.SourceUnchanged || !result.CacheUnchanged {
			t.Error("immutable local sources changed")
		}
	}()
	for _, mode := range []string{"native", "observed"} {
		home := filepath.Join(dir, mode+"-home")
		if copyPrivateTree(source, home) != nil {
			t.Fatal("exclusive native clone failed")
		}
		args := []string{"--cache", cache, "--chain", "cosmoshub-1", "--genesis", filepath.Join(cache, "genesis", "genesis.cosmoshub-1.json"), "--home", home, "--to", "400026", "--readers", "2", "--compare-results=true", "--pruning", "recent", "--verify-target-next-header", "--native-audit", filepath.Join(dir, mode+"-audit.ndjson")}
		if mode == "observed" {
			args = append(args, "--coin-records", filepath.Join(dir, "observed-movements.ndjson"))
		}
		if run(args) != nil {
			t.Fatal("bounded real native replay failed, preserve this attempt")
		}
		exportArgs := []string{"--home", home, "--out", filepath.Join(dir, mode+"-export.json"), "--require-height", "400026"}
		if mode == "observed" {
			exportArgs = append(exportArgs, "--coin-records", filepath.Join(dir, "observed-export-movements.ndjson"))
		}
		if exportZero(exportArgs) != nil {
			t.Fatal("bounded real native export failed, preserve this attempt")
		}
	}
}
