package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replay033/observeapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmstate "github.com/tendermint/tendermint/state"
	tm "github.com/tendermint/tendermint/types"
)

// TestWriteResumeFixture creates only synthetic private evidence in an exclusive
// scratch directory. No retained source, live process or original DB is opened.
func TestWriteResumeFixture(t *testing.T) {
	root := os.Getenv("HUB1_RESUME_FIXTURE")
	if root == "" {
		t.Skip("set HUB1_RESUME_FIXTURE to a NEW scratch directory")
	}
	mustFixture := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal("synthetic fixture operation failed:", err)
		}
	}
	mustFixture(os.Mkdir(root, 0700))
	cache := filepath.Join(root, "cache")
	mustFixture(os.MkdirAll(filepath.Join(cache, "genesis"), 0700))
	db := dbm.NewMemDB()
	defer db.Close()
	app := observeapp.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	app.SetLosslessRecording(true)
	state := observeapp.NewDefaultGenesisState()
	address := sdk.AccAddress(bytes.Repeat([]byte{1}, 20))
	state.Accounts = []observeapp.GenesisAccount{{Address: address, Coins: sdk.Coins{sdk.NewInt64Coin("stake", 3000000000000)}}}
	pub := ed25519.PubKeyEd25519{1}
	val := staking.NewValidator(sdk.ValAddress(address), pub, staking.Description{})
	val.Tokens, val.DelegatorShares = sdk.NewInt(2000000000000), sdk.NewDec(2000000000000)
	state.StakingData.Validators = append(state.StakingData.Validators, val)
	state.StakingData.Delegations = append(state.StakingData.Delegations, staking.Delegation{DelegatorAddress: address, ValidatorAddress: val.OperatorAddress, Shares: val.DelegatorShares})
	state.StakingData.Pool.NotBondedTokens = sdk.NewInt(5000000000000)
	body, err := observeapp.MakeCodec().MarshalJSON(state)
	mustFixture(err)
	doc := tm.GenesisDoc{GenesisTime: time.Unix(1, 0).UTC(), ChainID: "cosmoshub-1", ConsensusParams: tm.DefaultConsensusParams(), AppState: body}
	genesis := filepath.Join(cache, "genesis", "genesis.cosmoshub-1.json")
	mustFixture(doc.SaveAs(genesis))
	app.InitChain(abci.RequestInitChain{Time: doc.GenesisTime, ChainId: doc.ChainID, ConsensusParams: tm.TM2PB.ConsensusParams(doc.ConsensusParams), AppStateBytes: body})
	movement, err := createRecordEvidence(filepath.Join(root, "movements.source-closed.gz"), true)
	mustFixture(err)
	history, err := createRecordEvidence(filepath.Join(root, "native-history.source-closed.gz"), true)
	mustFixture(err)
	app.SetRecorder(movement)
	mustFixture(auditHistoryAnchor(history, db, app, "cosmoshub-1", genesis))
	writeRaw := func(height int64, suffix string, value interface{}) {
		t.Helper()
		raw, err := cdc.MarshalJSON(value)
		mustFixture(err)
		envelope, err := json.Marshal(map[string]json.RawMessage{"result": raw})
		mustFixture(err)
		path := basePath(cache, "cosmoshub-1", height) + "-" + suffix + ".raw.gz"
		mustFixture(os.MkdirAll(filepath.Dir(path), 0700))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		mustFixture(err)
		z := gzip.NewWriter(f)
		_, err = z.Write(envelope)
		mustFixture(err)
		mustFixture(z.Close())
		mustFixture(f.Close())
	}
	var previous tm.BlockID
	var previousResults, appHash []byte
	var next cached
	for height := int64(1); height <= 4; height++ {
		commit := &tm.Commit{}
		if height > 1 {
			commit = &tm.Commit{BlockID: previous, Precommits: []*tm.CommitSig{nil}}
		}
		block := tm.MakeBlock(height, nil, commit, nil)
		block.ChainID = "cosmoshub-1"
		block.Time = time.Unix(1+height, 0).UTC()
		block.ProposerAddress = pub.Address()
		block.ValidatorsHash = bytes.Repeat([]byte{2}, 32)
		block.NextValidatorsHash = block.ValidatorsHash
		block.AppHash = appHash
		block.LastResultsHash = previousResults
		block.LastBlockID = previous
		meta := tm.NewBlockMeta(block, block.MakePartSet(65536))
		writeRaw(height, "block", ctypes.ResultBlock{Block: block, BlockMeta: meta})
		next = cached{height: height, block: block, metaID: meta.BlockID.Hash}
		if height == 4 {
			writeRaw(height, "block_results", ctypes.ResultBlockResults{Height: height, Results: &tmstate.ABCIResponses{}})
			break
		}
		begin := app.BeginBlock(abci.RequestBeginBlock{Hash: meta.BlockID.Hash, Header: tm.TM2PB.Header(&block.Header)})
		end := app.EndBlock(abci.RequestEndBlock{Height: height})
		appHash = app.Commit().Data
		mustFixture(app.RecorderError())
		writeRaw(height, "block_results", ctypes.ResultBlockResults{Height: height, Results: &tmstate.ABCIResponses{BeginBlock: &begin, EndBlock: &end}})
		mustFixture(auditNativeHistory(history, db, app, next, begin, []*abci.ResponseDeliverTx{}, end))
		previous = meta.BlockID
		previousResults = tm.NewResults(nil).Hash()
	}
	mustFixture(app.SealRecorder())
	mustFixture(completeNativeHistory(history, app, 1, next))
	mustFixture(movement.Close())
	mustFixture(history.Close())
	mustFixture(os.WriteFile(filepath.Join(root, "SYNTHETIC-FIXTURE"), []byte(fmt.Sprintf("synthetic native033 heights 1..3, unexecuted header 4\n")), 0600))
}
