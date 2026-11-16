package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"github.com/syndtr/goleveldb/leveldb/opt"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/types"
)

func fixtureCheckpoint(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	db, err := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("fixture database unavailable")
	}
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	state := gaia.NewDefaultGenesisState()
	address := sdk.AccAddress(bytes.Repeat([]byte{1}, 20))
	account := gaia.NewDefaultGenesisAccount(address)
	account.Coins = sdk.Coins{sdk.NewInt64Coin(state.StakingData.Params.BondDenom, 3000000)}
	state.Accounts = []gaia.GenesisAccount{account}
	validator := staking.NewValidator(sdk.ValAddress(address), ed25519.PubKeyEd25519{1}, staking.Description{Moniker: "fixture"})
	validator.Tokens = sdk.NewInt(2000000)
	validator.DelegatorShares = sdk.NewDec(2000000)
	state.StakingData.Validators = append(state.StakingData.Validators, validator)
	state.StakingData.Delegations = append(state.StakingData.Delegations, staking.Delegation{DelegatorAddress: address, ValidatorAddress: validator.OperatorAddress, Shares: validator.DelegatorShares})
	state.StakingData.Pool.NotBondedTokens = sdk.NewInt(5000000)
	body, err := gaia.MakeCodec().MarshalJSON(state)
	if err != nil {
		t.Fatal("fixture genesis encoding failed")
	}
	app.InitChain(abci.RequestInitChain{ChainId: "boundary-fixture", Time: time.Unix(1, 0), AppStateBytes: body})
	app.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{ChainID: "boundary-fixture", Height: 1, Time: time.Unix(2, 0)}})
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	if app.LastBlockHeight() != 1 {
		t.Fatal("fixture did not commit exact height")
	}
	db.Close()
	return home
}

func checkpointHashes(t *testing.T, home string) map[string][32]byte {
	t.Helper()
	hashes := map[string][32]byte{}
	err := filepath.WalkDir(filepath.Join(home, "application.db"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[path] = sha256.Sum256(body)
		return nil
	})
	if err != nil {
		t.Fatal("checkpoint hash capture failed")
	}
	return hashes
}

func exactJSON(t *testing.T, body []byte) interface{} {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		t.Fatal("complete JSON decoding failed")
	}
	return value
}

func TestNativeHubOneExportFullTreeAndSourceBytes(t *testing.T) {
	home := fixtureCheckpoint(t)
	before := checkpointHashes(t, home)
	output := filepath.Join(t.TempDir(), "native.json")
	if err := exportZero([]string{"--home", home, "--out", output, "--require-height", "1"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal("export artifact unavailable")
	}
	var result struct {
		Release       string          `json:"release"`
		SourceHeight  int64           `json:"source_height"`
		SourceAppHash string          `json:"source_app_hash"`
		ForZeroHeight bool            `json:"for_zero_height"`
		AppState      json.RawMessage `json:"app_state"`
		Validators    json.RawMessage `json:"validators"`
	}
	if json.Unmarshal(body, &result) != nil || result.Release != release || result.SourceHeight != 1 || !result.ForZeroHeight {
		t.Fatal("export provenance differs")
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("artifact permissions not private")
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", home, &opt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal("native reference checkpoint unavailable")
	}
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	if result.SourceAppHash != hex.EncodeToString(app.LastCommitID().Hash) {
		t.Fatal("native export source root differs from loaded checkpoint")
	}
	nativeState, nativeValidators, err := app.ExportAppStateAndValidators(true, nil)
	if err != nil {
		t.Fatal("native reference export failed")
	}
	if len(nativeValidators) != 1 {
		t.Fatal("nonempty native validator fixture required")
	}
	validatorJSON, err := gaia.MakeCodec().MarshalJSON(nativeValidators)
	if err != nil {
		t.Fatal("native validators encoding failed")
	}
	legacyJSON, err := json.Marshal(nativeValidators)
	if err != nil || reflect.DeepEqual(exactJSON(t, legacyJSON), exactJSON(t, validatorJSON)) {
		t.Fatal("standard JSON validator negative control did not differ")
	}
	var legacyDecoded []types.GenesisValidator
	if gaia.MakeCodec().UnmarshalJSON(legacyJSON, &legacyDecoded) == nil {
		t.Fatal("standard JSON validator negative control unexpectedly decoded")
	}
	var decoded []types.GenesisValidator
	if gaia.MakeCodec().UnmarshalJSON(result.Validators, &decoded) != nil || len(decoded) != 1 || !decoded[0].PubKey.Equals(nativeValidators[0].PubKey) {
		t.Fatal("native amino validator public key was lost")
	}
	if !reflect.DeepEqual(exactJSON(t, result.AppState), exactJSON(t, nativeState)) || !reflect.DeepEqual(exactJSON(t, result.Validators), exactJSON(t, validatorJSON)) {
		t.Fatal("full native export tree or validator array differs")
	}
	db.Close()
	if !reflect.DeepEqual(before, checkpointHashes(t, home)) {
		t.Fatal("original checkpoint database bytes changed")
	}
}

func TestNativeHubOneExportRequiresExactHeightAndExclusiveOutput(t *testing.T) {
	home := fixtureCheckpoint(t)
	output := filepath.Join(t.TempDir(), "native.json")
	if exportZero([]string{"--home", home, "--out", output, "--require-height", "2"}) == nil {
		t.Fatal("wrong source height accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("wrong-height invocation created artifact")
	}
	if err := os.WriteFile(output, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exportZero([]string{"--home", home, "--out", output, "--require-height", "1"}) == nil {
		t.Fatal("existing artifact accepted")
	}
	body, _ := os.ReadFile(output)
	if string(body) != "preserved" {
		t.Fatal("existing artifact changed")
	}
}

func TestNativeHubOneExportRefusesWriterAndCheckpointOutputs(t *testing.T) {
	home := fixtureCheckpoint(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{filepath.Join(home, "artifact"), filepath.Join(alias, "artifact")} {
		if exportZero([]string{"--home", home, "--out", out, "--require-height", "1"}) == nil {
			t.Fatal("checkpoint output path accepted")
		}
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("fixture writer unavailable")
	}
	defer db.Close()
	if exportZero([]string{"--home", home, "--out", filepath.Join(t.TempDir(), "native.json"), "--require-height", "1"}) == nil {
		t.Fatal("concurrent writer lock bypassed")
	}
}

func TestNativeHubOneExportDoesNotCreateMissingLock(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "application.db")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if exportZero([]string{"--home", home, "--out", filepath.Join(t.TempDir(), "native.json")}) == nil {
		t.Fatal("missing existing source lock accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("malformed source acquired new database files")
	}
}

func TestNativeHubOneExportDoesNotCreateMissingCheckpoint(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	err := exportZero([]string{"--home", home, "--out", home + ".json"})
	if err == nil || strings.Contains(err.Error(), home) {
		t.Fatal("missing checkpoint accepted or private path disclosed")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("missing source checkpoint created")
	}
}
