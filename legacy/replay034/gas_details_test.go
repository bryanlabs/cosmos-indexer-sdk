package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/types"
)

func TestGasDetailsKeepExactFieldsWithoutChangingNativeResult(t *testing.T) {
	txs := types.Txs{types.Tx("private transaction bytes")}
	native := []*abci.ResponseDeliverTx{{Code: 7, GasWanted: 200000, GasUsed: 114933, Log: "private log"}}
	cached := &cachedResults{deliver: []*abci.ResponseDeliverTx{{Code: 7, GasWanted: 200000, GasUsed: 114833}}}
	before, _ := json.Marshal(native)
	var output bytes.Buffer
	if err := writeGasDifferences(&output, 479755, txs, cached, native); err != nil {
		t.Fatal(err)
	}
	var detail gasDifferenceDetail
	if err := json.Unmarshal(output.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Height != 479755 || detail.TxIndex != 0 || detail.TxSHA256 != hex.EncodeToString(txs[0].Hash()) || detail.NativeCode != 7 || detail.CachedCode != 7 || detail.NativeGasWanted != 200000 || detail.CachedGasWanted != 200000 || detail.NativeGasUsed != 114933 || detail.CachedGasUsed != 114833 {
		t.Fatal("exact diagnostic fields differ")
	}
	after, _ := json.Marshal(native)
	if !bytes.Equal(before, after) || strings.Contains(output.String(), "private") {
		t.Fatal("diagnostic changed result or exposed private raw fields")
	}
}

func TestGasDetailsRecordsWantedOnlyDifferenceAndEveryIndex(t *testing.T) {
	txs := types.Txs{types.Tx("a"), types.Tx("b"), types.Tx("c")}
	native := []*abci.ResponseDeliverTx{{GasWanted: 1}, {GasUsed: 2}, {GasWanted: 3}}
	cached := &cachedResults{deliver: []*abci.ResponseDeliverTx{{}, {}, {GasWanted: 3}}}
	var output bytes.Buffer
	if err := writeGasDifferences(&output, 1, txs, cached, native); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for index := 0; index < 2; index++ {
		var detail gasDifferenceDetail
		if err := decoder.Decode(&detail); err != nil || detail.TxIndex != index {
			t.Fatal("missing or reordered gas discrepancy")
		}
	}
	var extra gasDifferenceDetail
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("equal gas emitted as a discrepancy")
	}
}

type failingGasWriter struct{}

func (failingGasWriter) Write([]byte) (int, error) { return 0, errors.New("private failure") }

func TestGasDetailsRejectsIncompleteResultsAndFailedWriter(t *testing.T) {
	txs := types.Txs{types.Tx("a")}
	for _, cached := range []*cachedResults{nil, {}, {deliver: []*abci.ResponseDeliverTx{nil}}} {
		if err := writeGasDifferences(io.Discard, 1, txs, cached, []*abci.ResponseDeliverTx{{}}); err == nil {
			t.Fatal("incomplete result accepted")
		}
	}
	if err := writeGasDifferences(io.Discard, 1, txs, &cachedResults{deliver: []*abci.ResponseDeliverTx{{}}}, []*abci.ResponseDeliverTx{nil}); err == nil {
		t.Fatal("nil native result accepted")
	}
	if err := writeGasDifferences(failingGasWriter{}, 1, txs, &cachedResults{deliver: []*abci.ResponseDeliverTx{{}}}, []*abci.ResponseDeliverTx{{GasUsed: 1}}); err == nil || strings.Contains(err.Error(), "private failure") {
		t.Fatal("writer failure ignored or raw error exposed")
	}
}

func TestGasDetailsDestinationRejectsReplayAndOutputAliases(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, filepath.Join(home, "result-differences.txt"), filepath.Join(alias, "status.json")} {
		if validateGasDestination(path, home, "", "") == nil {
			t.Fatal("replay-home alias accepted as evidence")
		}
	}
	path := filepath.Join(dir, "evidence")
	if validateGasDestination(path, home, filepath.Join(dir, ".", "evidence"), "") == nil || validateGasDestination(path, home, "", path) == nil {
		t.Fatal("colliding output accepted")
	}
	if err := validateGasDestination(path, home, filepath.Join(dir, "profile"), filepath.Join(dir, "movements")); err != nil {
		t.Fatal("disjoint evidence refused")
	}
}

func TestGasDetailsExclusiveCreateRejectsDanglingFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "forbidden.json")
	path := filepath.Join(dir, "evidence-symlink")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--gas-difference-details", path, "--genesis", "unused", "--home", home, "--to", "1"}); err == nil {
		t.Fatal("exclusive creation followed a dangling final symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("dangling target was created")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("original symlink was not preserved")
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("database accessed through invalid evidence destination")
	}
}

func TestGasDetailsFlagsRefuseBeforeDatabaseAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence")
	if err := run([]string{"--gas-difference-details", path, "--compare-results=false"}); err == nil {
		t.Fatal("gas evidence without full result comparisons accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid flags created evidence")
	}
	if err := run([]string{"--gas-difference-details", path}); err == nil {
		t.Fatal("missing mandatory arguments accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing mandatory arguments created evidence")
	}
	if err := os.WriteFile(path, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--gas-difference-details", path, "--genesis", "unused", "--home", filepath.Join(t.TempDir(), "home"), "--to", "1"}); err == nil {
		t.Fatal("existing evidence path accepted")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "preserved" {
		t.Fatal("existing evidence changed")
	}
}
