package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

func TestNativeHistoryAllNineRootsAndNegativeControls(t *testing.T) {
	home := fixtureCheckpoint(t)
	db, err := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("synthetic DB refused")
	}
	defer db.Close()
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	good, err := historyRoots(db, app)
	if err != nil || len(good.StoreInfos) != 9 {
		t.Fatal("native committed all9 roots refused")
	}
	original := append([]byte(nil), db.Get([]byte("s/1"))...)
	for _, name := range []string{"missing", "duplicate", "root", "version", "store-version"} {
		var bad historyCommitInfo
		if cdc.UnmarshalBinaryLengthPrefixed(original, &bad) != nil {
			t.Fatal("synthetic metadata decode")
		}
		switch name {
		case "missing":
			bad.StoreInfos = bad.StoreInfos[1:]
		case "duplicate":
			bad.StoreInfos[0].Name = bad.StoreInfos[1].Name
		case "root":
			bad.StoreInfos[0].Core.CommitID.Hash = []byte{99}
		case "version":
			bad.Version++
		case "store-version":
			bad.StoreInfos[0].Core.CommitID.Version++
		}
		body, e := cdc.MarshalBinaryLengthPrefixed(bad)
		if e != nil {
			t.Fatal("synthetic metadata encode")
		}
		db.Set([]byte("s/1"), body)
		if _, e := historyRoots(db, app); e == nil {
			t.Fatal("corrupted native metadata accepted: " + name)
		}
		db.Set([]byte("s/1"), original)
	}
}
func TestNativeHistoryRequiresStrictNextHeaderAndResultChecks(t *testing.T) {
	for _, flags := range [][]string{{"--native-history", "unused"}, {"--native-history", "unused", "--verify-target-next-header", "--compare-results=false"}} {
		if run(flags) == nil {
			t.Fatal("unchecked native history permitted")
		}
	}
}

func TestEvidenceCompressionIsExactAndClosedBeforeSuccess(t *testing.T) {
	raw := []byte("{\"unknown\":\"AA==\",\"empty\":\"\",\"absent\":null}\n")
	for _, compressed := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "private")
		w, e := createRecordEvidence(path, compressed)
		if e != nil {
			t.Fatal("exclusive output refused")
		}
		if n, e := w.Write(raw); n != len(raw) || e != nil {
			t.Fatal("private write refused")
		}
		if w.Sync() != nil || w.Close() != nil {
			t.Fatal("durable close refused")
		}
		if _, e := w.Write(raw); e == nil {
			t.Fatal("write after close accepted")
		}
		data, e := os.ReadFile(path)
		if e != nil {
			t.Fatal("private read refused")
		}
		if compressed {
			z, e := gzip.NewReader(bytes.NewReader(data))
			if e != nil {
				t.Fatal("gzip reader refused")
			}
			data, e = io.ReadAll(z)
			z.Close()
			if e != nil {
				t.Fatal("complete gzip refused")
			}
		}
		if !bytes.Equal(data, raw) {
			t.Fatal("compression changed original record bytes")
		}
	}
}
