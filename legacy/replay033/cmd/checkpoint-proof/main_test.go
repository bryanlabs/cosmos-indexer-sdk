package main

import (
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckNextEveryBinding(t *testing.T) {
	for _, name := range []string{"equal", "app", "parent", "results", "validators", "height", "chain", "zero"} {
		t.Run(name, func(t *testing.T) {
			current := &types.Block{Header: types.Header{Height: 10, ChainID: "cosmoshub-1", NextValidatorsHash: []byte{1}}}
			next := &types.Block{Header: types.Header{Height: 11, ChainID: "cosmoshub-1", ValidatorsHash: []byte{1}, AppHash: []byte{3}, LastResultsHash: []byte{4}, LastBlockID: types.BlockID{Hash: []byte{2}}}}
			app := []byte{3}
			switch name {
			case "app":
				next.AppHash = []byte{5}
			case "parent":
				next.LastBlockID.Hash = []byte{5}
			case "results":
				next.LastResultsHash = []byte{5}
			case "validators":
				next.ValidatorsHash = []byte{5}
			case "height":
				next.Height = 12
			case "chain":
				next.ChainID = "other"
			case "zero":
				app = nil
			}
			e := checkNext(10, app, []byte{2}, []byte{4}, current, next)
			if (e == nil) != (name == "equal") {
				t.Fatal("wrong proof decision")
			}
		})
	}
}
func TestNoCheckpointCreationOrPathDisclosure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "privacy-marker")
	if _, e := prove(t.TempDir(), "cosmoshub-1", p); e == nil || strings.Contains(e.Error(), "privacy-marker") {
		t.Fatal("missing checkpoint was accepted or path disclosed")
	}
	if _, e := os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("source checkpoint created")
	}
}
func TestNoConcurrentWriter(t *testing.T) {
	h := t.TempDir()
	db, e := dbm.NewGoLevelDBWithOpts("application", h, nil)
	if e != nil {
		t.Fatal("fixture DB unavailable")
	}
	defer db.Close()
	fixtureStore(t, db)
	if _, e := prove(t.TempDir(), "cosmoshub-1", h); e == nil || e.Error() != "source checkpoint is not an unlocked offline database" {
		t.Fatal("concurrent writer was not refused at the source lock")
	}
}
func TestSeparateEvidenceSymlink(t *testing.T) {
	p := t.TempDir()
	h := filepath.Join(p, "home")
	a := filepath.Join(p, "alias")
	if os.Mkdir(h, 0700) != nil || os.Symlink(h, a) != nil {
		t.Fatal("fixture failed")
	}
	if outside(h, filepath.Join(h, "proof")) || outside(h, filepath.Join(a, "proof")) {
		t.Fatal("inside checkpoint accepted")
	}
	if !outside(h, filepath.Join(p, "proof")) {
		t.Fatal("independent output refused")
	}
}
func TestUnsupportedChainAndMissingCapture(t *testing.T) {
	if _, e := prove(t.TempDir(), "cosmoshub-2", t.TempDir()); e == nil {
		t.Fatal("wrong release accepted")
	}
	if _, _, _, e := loadBlock(filepath.Join(t.TempDir(), "privacy-marker"), "cosmoshub-1", 1); e == nil || strings.Contains(e.Error(), "privacy-marker") {
		t.Fatal("missing capture accepted or path disclosed")
	}
}
