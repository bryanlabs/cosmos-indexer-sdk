package main

import (
	"encoding/json"
	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/syndtr/goleveldb/leveldb/opt"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeFullAuditEveryStoreAndWrongHeightRefusal(t *testing.T) {
	home := fixtureCheckpoint(t)
	before := checkpointHashes(t, home)
	db, err := dbm.NewGoLevelDBWithOpts("application", home, &opt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal("fixture read-only unavailable")
	}
	defer db.Close()
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	f, err := createPrivateEvidence(filepath.Join(t.TempDir(), "audit.ndjson"))
	if err != nil {
		t.Fatal("private audit unavailable")
	}
	defer f.Close()
	c := cached{height: 1, block: &tmtypes.Block{Header: abciHeaderFixture()}}
	if err := auditNative(f, app, c, abci.ResponseBeginBlock{}, nil, abci.ResponseEndBlock{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal("native audit read failed")
	}
	var evidence struct {
		Stores []struct {
			Name       string               `json:"name"`
			Partitions []abci.ResponseQuery `json:"partitions"`
		} `json:"all_persistent_stores"`
	}
	if json.Unmarshal(b, &evidence) != nil || len(evidence.Stores) != 9 {
		t.Fatal("all native stores must be preserved")
	}
	nonempty := 0
	seen := map[string]bool{}
	for _, s := range evidence.Stores {
		if seen[s.Name] || len(s.Partitions) != 256 {
			t.Fatal("complete prefix coverage required")
		}
		seen[s.Name] = true
		populated := false
		for p, res := range s.Partitions {
			if res.Height != 1 || res.Code != 0 || len(res.Key) != 1 || res.Key[0] != byte(p) {
				t.Fatal("native prefix provenance differs")
			}
			if len(res.Value) > 2 {
				populated = true
			}
		}
		if populated {
			nonempty++
		}
	}
	if nonempty < 7 {
		t.Fatal("native all-store fixture must be meaningful")
	}
	c.height = 2
	if auditNative(f, app, c, abci.ResponseBeginBlock{}, nil, abci.ResponseEndBlock{}) == nil {
		t.Fatal("wrong native height accepted")
	}
	if !reflect.DeepEqual(before, checkpointHashes(t, home)) {
		t.Fatal("read-only native audit changed checkpoint bytes")
	}
}
func abciHeaderFixture() tmtypes.Header {
	return tmtypes.Header{Height: 1, ChainID: "boundary-fixture"}
}
func TestObservedExportSourceBytesAndExclusiveEvidence(t *testing.T) {
	home := fixtureCheckpoint(t)
	before := checkpointHashes(t, home)
	dir := t.TempDir()
	args := []string{"--home", home, "--out", filepath.Join(dir, "export.json"), "--require-height", "1", "--coin-records", filepath.Join(dir, "records.ndjson")}
	if exportZero(args) != nil {
		t.Fatal("observed native export failed")
	}
	body, err := os.ReadFile(filepath.Join(dir, "records.ndjson"))
	if err != nil || len(body) == 0 {
		t.Fatal("nonempty observed evidence required")
	}
	if exportZero(args) == nil {
		t.Fatal("exclusive evidence must refuse reuse")
	}
	if !reflect.DeepEqual(before, checkpointHashes(t, home)) {
		t.Fatal("observed export modified native DB bytes")
	}
}
