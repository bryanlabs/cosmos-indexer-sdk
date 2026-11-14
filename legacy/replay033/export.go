package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replay033/observeapp"
	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/syndtr/goleveldb/leveldb/opt"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
)

func outsideCheckpoint(home, output string) bool {
	root, err := filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Join(parent, filepath.Base(output)))
	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// The pinned native SDK performs zero-height preparation only in check-state
// caches. The source checkpoint is opened read-only and is never committed.
// This captures a full export, not independently classified payout evidence.
func exportZero(args []string) (failure error) {
	defer func() {
		if recover() != nil {
			failure = fmt.Errorf("native Hub 1 export stopped; no checkpoint commit attempted")
		}
	}()
	fs := flag.NewFlagSet("export-zero", flag.ContinueOnError)
	home := fs.String("home", "", "offline application checkpoint directory")
	out := fs.String("out", "", "private complete native export, exclusive new file")
	requiredHeight := fs.Int64("require-height", 500000, "exact committed source height")
	recordsPath := fs.String("coin-records", "", "optional exclusive private native payout and state evidence")
	if fs.Parse(args) != nil {
		return fmt.Errorf("invalid native export flags")
	}
	if *home == "" || *out == "" || *requiredHeight < 1 {
		return fmt.Errorf("home, out and positive require-height are required")
	}
	if *recordsPath != "" && (*recordsPath == *out || !outsideCheckpoint(*home, *recordsPath)) {
		return fmt.Errorf("private movement evidence must be distinct and outside source checkpoint")
	}
	if !outsideCheckpoint(*home, *out) {
		return fmt.Errorf("private export must be outside the existing source checkpoint")
	}
	lock, err := os.Lstat(filepath.Join(*home, "application.db", "LOCK"))
	if err != nil || !lock.Mode().IsRegular() {
		return fmt.Errorf("native source checkpoint must have its existing regular database lock")
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", *home, &opt.Options{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("cannot open offline Hub 1 checkpoint read-only")
	}
	defer db.Close()
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	if app.LastBlockHeight() != *requiredHeight {
		return fmt.Errorf("native export source height differs from required height")
	}
	sourceAppHash := hex.EncodeToString(app.LastCommitID().Hash)
	artifact, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create exclusive private native export")
	}
	defer artifact.Close()
	var state json.RawMessage
	var validators []tmtypes.GenesisValidator
	if *recordsPath != "" {
		records, e := createPrivateEvidence(*recordsPath)
		if e != nil {
			return e
		}
		defer records.Close()
		observed := observeapp.NewGaiaApp(log.NewNopLogger(), db, nil, true)
		observed.SetRecorder(records)
		state, validators, err = observed.ExportObserved()
		if observed.RecorderError() != nil || records.Sync() != nil {
			return fmt.Errorf("native export evidence failed")
		}
	} else {
		state, validators, err = app.ExportAppStateAndValidators(true, nil)
	}
	if err != nil {
		return fmt.Errorf("pinned native zero-height export failed")
	}
	validatorJSON, err := gaia.MakeCodec().MarshalJSON(validators)
	if err != nil {
		return fmt.Errorf("cannot encode complete native validator set")
	}
	result := struct {
		Release       string          `json:"release"`
		SourceHeight  int64           `json:"source_height"`
		SourceAppHash string          `json:"source_app_hash"`
		ForZeroHeight bool            `json:"for_zero_height"`
		AppState      json.RawMessage `json:"app_state"`
		Validators    json.RawMessage `json:"validators"`
	}{release, app.LastBlockHeight(), sourceAppHash, true, state, validatorJSON}
	if json.NewEncoder(artifact).Encode(result) != nil || artifact.Sync() != nil {
		return fmt.Errorf("cannot persist complete private native export")
	}
	fmt.Printf("native Hub 1 zero-height export complete at source height %d; checkpoint opened read-only\n", app.LastBlockHeight())
	return nil
}
