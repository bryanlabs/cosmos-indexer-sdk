package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replay034/observeapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

func TestSparseFlagRequiresCoinRecordsBeforeSideEffects(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"run", run}, {"export-zero", exportZero}} {
		t.Run(command.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "absent")
			err := command.run([]string{"-sparse-account-checks", "-home", home})
			if err == nil || err.Error() != "sparse-account-checks requires coin-records" {
				t.Fatal("sparse mode did not require recording")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatal("invalid sparse flags created a checkpoint")
			}
		})
	}
}

func TestRunSparseFlagDefaultAndExplicitFalse(t *testing.T) {
	for _, args := range [][]string{nil, {"-sparse-account-checks=false"}} {
		if err := run(args); err == nil || err.Error() != "genesis, home and to are required" {
			t.Fatal("full default unexpectedly required recording")
		}
	}
}

// Exercise the real export CLI against one offline checkpoint, comparing both
// private artifacts byte-for-byte. Never print fixture contents or SDK errors.
func TestExportSparseFlagExactArtifactsAndCheckpointIsolation(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "checkpoint")
	if os.Mkdir(home, 0o700) != nil {
		t.Fatal("create checkpoint directory")
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("create checkpoint database")
	}
	key := secp256k1.GenPrivKeySecp256k1([]byte("private sparse CLI fixture"))
	addr := sdk.AccAddress(key.PubKey().Address())
	state := observeapp.NewDefaultGenesisState()
	state.Accounts = []observeapp.GenesisAccount{{Address: addr, Coins: sdk.NewCoins(sdk.NewInt64Coin("stake", 10000000))}}
	v := staking.NewValidator(sdk.ValAddress(addr), key.PubKey(), staking.Description{})
	v.Tokens = sdk.NewInt(1000000)
	v.DelegatorShares = sdk.NewDec(1000000)
	state.StakingData.Validators = []staking.Validator{v}
	state.StakingData.Delegations = []staking.Delegation{{DelegatorAddress: addr, ValidatorAddress: sdk.ValAddress(addr), Shares: sdk.NewDec(1000000)}}
	state.StakingData.Pool.NotBondedTokens = sdk.NewInt(11000000)
	raw, err := observeapp.MakeCodec().MarshalJSON(state)
	if err != nil {
		t.Fatal("encode fixture genesis")
	}
	app := observeapp.NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	app.InitChain(abci.RequestInitChain{AppStateBytes: raw, ChainId: "sparse-cli-test"})
	app.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: 1, ChainID: "sparse-cli-test", Time: time.Unix(1500000001, 0)}})
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	hash := append([]byte(nil), app.Commit().Data...)
	db.Close()
	var fullArtifact, fullRecords []byte
	for i, mode := range []string{"", "-sparse-account-checks=false", "-sparse-account-checks=true"} {
		output := filepath.Join(base, []string{"default", "full", "sparse"}[i])
		records := output + ".records"
		args := []string{"-home", home, "-out", output, "-coin-records", records}
		if mode != "" {
			args = append(args, mode)
		}
		if exportZero(args) != nil {
			t.Fatal("CLI export failed")
		}
		artifact, err := os.ReadFile(output)
		if err != nil {
			t.Fatal("read export artifact")
		}
		rows, err := os.ReadFile(records)
		if err != nil || len(rows) == 0 {
			t.Fatal("read export records")
		}
		if i == 0 {
			fullArtifact, fullRecords = artifact, rows
		} else if !bytes.Equal(fullArtifact, artifact) || !bytes.Equal(fullRecords, rows) {
			t.Fatal("CLI FULL/SPARSE artifacts differ")
		}
	}
	db, err = dbm.NewGoLevelDBWithOpts("application", home, nil)
	if err != nil {
		t.Fatal("reopen checkpoint")
	}
	defer db.Close()
	app = observeapp.NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	if app.LastBlockHeight() != 1 || !bytes.Equal(hash, app.LastCommitID().Hash) {
		t.Fatal("CLI export changed source checkpoint")
	}
}
