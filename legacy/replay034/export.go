package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replay034/observeapp"
	"github.com/syndtr/goleveldb/leveldb/opt"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/types"
)

// Evidence contains private chain data. Never overwrite an earlier run or echo
// its path in an error. A failed run's file is retained, not cleaned up.
func createPrivateEvidence(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot create exclusive private evidence file")
	}
	return f, nil
}

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

// exportZero opens the committed application read-only. Gaia's real zero-height
// preparation writes only to the application's check-state cache. It never
// commits, touches validator replay history, or modifies the source checkpoint.
// The caller must use an offline checkpoint or an independently copied DB: the
// LevelDB lock rejects a concurrent writer rather than bypassing its lock.
func exportZero(args []string) (failure error) {
	defer func() {
		if recover() != nil {
			failure = fmt.Errorf("zero-height export stopped inside the application; no checkpoint commit was attempted")
		}
	}()
	fs := flag.NewFlagSet("export-zero", flag.ContinueOnError)
	home := fs.String("home", "", "offline application checkpoint directory")
	out := fs.String("out", "", "private zero-height export artifact, must not exist")
	recordsPath := fs.String("coin-records", "", "private observed export movement evidence, must not exist")
	sparseChecks := fs.Bool("sparse-account-checks", false, "use exact affected-account checks instead of full snapshots; requires coin-records")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid export flags")
	}
	if *sparseChecks && *recordsPath == "" {
		return fmt.Errorf("sparse-account-checks requires coin-records")
	}
	if *home == "" || *out == "" || *recordsPath == "" || *out == *recordsPath {
		return fmt.Errorf("home, out and distinct coin-records are required")
	}
	if _, err := os.Stat(*home); err != nil {
		return fmt.Errorf("offline checkpoint directory is unavailable")
	}
	if !outsideCheckpoint(*home, *out) || !outsideCheckpoint(*home, *recordsPath) {
		return fmt.Errorf("export evidence must be outside the source checkpoint directory")
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", *home, &opt.Options{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("cannot open offline checkpoint read-only")
	}
	defer db.Close()
	app := observeapp.NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	if app.LastBlockHeight() < 1 {
		return fmt.Errorf("export requires a committed checkpoint")
	}
	records, err := createPrivateEvidence(*recordsPath)
	if err != nil {
		return err
	}
	defer records.Close()
	artifact, err := createPrivateEvidence(*out)
	if err != nil {
		return err
	}
	defer artifact.Close()
	app.SetSparseAccounting(*sparseChecks)
	app.SetRecorder(records)
	state, validators, err := app.ExportObserved()
	if err != nil {
		return fmt.Errorf("zero-height export or observed accounting failed")
	}
	if app.RecorderError() != nil {
		return fmt.Errorf("zero-height observation failed")
	}
	if err := records.Sync(); err != nil {
		return fmt.Errorf("cannot sync export movement evidence")
	}
	result := struct {
		Release       string                   `json:"release"`
		SourceHeight  int64                    `json:"source_height"`
		ForZeroHeight bool                     `json:"for_zero_height"`
		AppState      json.RawMessage          `json:"app_state"`
		Validators    []types.GenesisValidator `json:"validators"`
	}{release, app.LastBlockHeight(), true, state, validators}
	if err := json.NewEncoder(artifact).Encode(result); err != nil {
		return fmt.Errorf("cannot write zero-height export artifact")
	}
	if err := artifact.Sync(); err != nil {
		return fmt.Errorf("cannot sync zero-height export artifact")
	}
	fmt.Printf("zero-height export complete at source height %d; checkpoint opened read-only\n", app.LastBlockHeight())
	return nil
}
