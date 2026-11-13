package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/opt"
	dbm "github.com/tendermint/tendermint/libs/db"
)

func TestPrivateEvidenceExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	f, err := createPrivateEvidence(path)
	if err != nil {
		t.Fatal("create failed")
	}
	if _, err := f.WriteString("sentinel"); err != nil {
		t.Fatal("write failed")
	}
	if err := f.Close(); err != nil {
		t.Fatal("close failed")
	}
	if _, err := createPrivateEvidence(path); err == nil {
		t.Fatal("existing evidence was reopened")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "sentinel" {
		t.Fatal("existing evidence changed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("evidence permissions are not private")
	}
}

func TestReadOnlyCheckpointRejectsConcurrentWriter(t *testing.T) {
	home := t.TempDir()
	db, err := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("cannot create fixture database")
	}
	defer db.Close()
	reader, err := dbm.NewGoLevelDBWithOpts("application", home, &opt.Options{ReadOnly: true})
	if err == nil {
		reader.Close()
		t.Fatal("read-only checkpoint bypassed running writer lock")
	}
}

func TestExportEvidenceOutsideCheckpoint(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "checkpoint")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal("cannot create checkpoint directory")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal("cannot create fixture alias")
	}
	if outsideCheckpoint(home, filepath.Join(home, "output")) || outsideCheckpoint(home, filepath.Join(alias, "output")) {
		t.Fatal("accepted output within checkpoint")
	}
	if !outsideCheckpoint(home, filepath.Join(base, "output")) {
		t.Fatal("rejected independent output")
	}
}

func TestExportRequiresDistinctArtifacts(t *testing.T) {
	if err := exportZero([]string{"-home", ".", "-out", "same", "-coin-records", "same"}); err == nil {
		t.Fatal("accepted overlapping artifacts")
	}
}

func TestExportDoesNotCreateCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	err := exportZero([]string{"-home", path, "-out", path + ".out", "-coin-records", path + ".records"})
	if err == nil {
		t.Fatal("accepted absent checkpoint")
	}
	if strings.Contains(err.Error(), path) {
		t.Fatal("error disclosed checkpoint path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("checkpoint was created")
	}
}
