package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
)

type terminalFixtureApp struct {
	replayApp
	height int64
	commit sdk.CommitID
	mutate func(*abci.ResponseQuery)
}

func (a terminalFixtureApp) LastBlockHeight() int64     { return a.height }
func (a terminalFixtureApp) LastCommitID() sdk.CommitID { return a.commit }
func (a terminalFixtureApp) Query(q abci.RequestQuery) abci.ResponseQuery {
	r := abci.ResponseQuery{Height: q.Height, Key: q.Data, Value: []byte(q.Path)}
	if a.mutate != nil {
		a.mutate(&r)
	}
	return r
}

func TestClosedTerminalExactRawWitnessAndNegatives(t *testing.T) {
	vh := filepath.Join(t.TempDir(), "validators")
	if os.WriteFile(vh, []byte("complete history"), 0600) != nil {
		t.Fatal("fixture write")
	}
	hash, _ := terminalFileHash(vh)
	app := terminalFixtureApp{height: terminalHeight, commit: sdk.CommitID{Version: terminalHeight, Hash: []byte("actual terminal root")}}
	stores, e := auditNativeStores(app, terminalHeight)
	if e != nil {
		t.Fatal(e)
	}
	ref := &closedTerminalReference{Commit: app.commit, Stores: stores, Results: []byte("actual results"), BlockHash: []byte("actual header"), ValidatorHistorySHA256: hash}
	check := func(a terminalFixtureApp, h int64, b, r []byte) error {
		return verifyClosedTerminal(ref, a, h, b, r, vh)
	}
	if e = check(app, terminalHeight, ref.BlockHash, ref.Results); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"missing_last_height", "wrong_root", "wrong_commit_height", "wrong_result", "wrong_header", "raw_key", "unknown_prefix", "missing_store_bytes", "query_height", "query_code", "validator_history"} {
		t.Run(name, func(t *testing.T) {
			a := app
			h, b, r := terminalHeight, ref.BlockHash, ref.Results
			switch name {
			case "missing_last_height":
				h--
			case "wrong_root":
				a.commit.Hash = []byte("wrong")
			case "wrong_commit_height":
				a.commit.Version--
			case "wrong_result":
				r = []byte("wrong")
			case "wrong_header":
				b = []byte("wrong")
			case "raw_key":
				a.mutate = func(q *abci.ResponseQuery) { q.Key = []byte{0} }
			case "unknown_prefix":
				a.mutate = func(q *abci.ResponseQuery) {
					if bytes.Equal(q.Key, []byte{255}) {
						q.Value = []byte("lost unknown value")
					}
				}
			case "missing_store_bytes":
				a.mutate = func(q *abci.ResponseQuery) { q.Value = nil }
			case "query_height":
				a.mutate = func(q *abci.ResponseQuery) { q.Height-- }
			case "query_code":
				a.mutate = func(q *abci.ResponseQuery) { q.Code = 1 }
			case "validator_history":
				if os.WriteFile(vh, []byte("changed"), 0600) != nil {
					t.Fatal("fixture write")
				}
				defer os.WriteFile(vh, []byte("complete history"), 0600)
			}
			if check(a, h, b, r) == nil {
				t.Fatal("changed witness accepted")
			}
		})
	}
}

func TestClosedTerminalRejectsUnpinnedOriginalBytesBeforeDBOpen(t *testing.T) {
	home := t.TempDir()
	if os.Mkdir(filepath.Join(home, "application.db"), 0700) != nil {
		t.Fatal("fixture")
	}
	lock := filepath.Join(home, "application.db", "LOCK")
	if os.WriteFile(lock, nil, 0600) != nil {
		t.Fatal("fixture")
	}
	before, _ := terminalManifest(home)
	if _, e := cloneTerminalReference(home, filepath.Join(t.TempDir(), "copy")); e == nil {
		t.Fatal("arbitrary original bytes accepted")
	}
	after, _ := terminalManifest(home)
	if before != after {
		t.Fatal("rejected source changed")
	}
	if os.Symlink(lock, filepath.Join(home, "alias")) != nil {
		t.Fatal("fixture")
	}
	if _, e := terminalManifest(home); e == nil {
		t.Fatal("source symlink accepted")
	}
}

func TestClosedTerminalCLIRejectsIncompleteOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--terminal-proof", "unused"},
		{"--closed-terminal-reference", "unused", "--to", "500014"},
		{"--closed-terminal-reference", "unused", "--to", "500015", "--chain", "cosmoshub-2"},
		{"--closed-terminal-reference", "unused", "--to", "500015", "--compare-results=false"},
		{"--closed-terminal-reference", "unused", "--to", "500015", "--verify-target-next-header"},
	} {
		if run(args) == nil {
			t.Fatal("terminal scope waiver accepted")
		}
	}
	if exportZero([]string{"--home", t.TempDir(), "--out", filepath.Join(t.TempDir(), "export"), "--require-height", "500015"}) == nil {
		t.Fatal("terminal export without witness accepted")
	}
}

func TestClosedTerminalCompletionDoesNotClaimNextHeaderOrIncome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof")
	f, e := createRecordEvidence(path, false)
	if e != nil {
		t.Fatal(e)
	}
	if completeClosedTerminal(f, &closedTerminalReference{}, 500001) != nil || f.Close() != nil {
		t.Fatal("proof write")
	}
	raw, _ := os.ReadFile(path)
	var row map[string]interface{}
	if json.Unmarshal(raw, &row) != nil {
		t.Fatal("proof decode")
	}
	if row["next_header_proof"] != false || row["financial_admission"] != false || row["height"] != float64(500015) || row["first_height"] != float64(500001) || row["proof_mode"] != "closed_pinned_native_reference" {
		t.Fatal("false consensus/income/coverage claim")
	}
	if _, ok := row["unexecuted_next_header"]; ok {
		t.Fatal("fabricated next header")
	}
}
