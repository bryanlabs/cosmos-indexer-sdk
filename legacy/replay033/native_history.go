package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	tmtypes "github.com/tendermint/tendermint/types"
)

type historyStoreInfo struct {
	Name string
	Core struct{ CommitID sdk.CommitID }
}
type historyCommitInfo struct {
	Version    int64
	StoreInfos []historyStoreInfo
}

// Read only the NEW replay's committed metadata, never a source DB. The pinned
// native MultiStoreProof implementation recomputes the all-store root. No tree
// export, prefix projection, reconstructed balance, or additional commit occurs.
func historyRoots(db dbm.DB, app replayApp) (historyCommitInfo, error) {
	var info historyCommitInfo
	h := app.LastBlockHeight()
	if h < 1 || cdc.UnmarshalBinaryLengthPrefixed(db.Get([]byte(fmt.Sprintf("s/%d", h))), &info) != nil || info.Version != h {
		return info, fmt.Errorf("private history commit metadata invalid")
	}
	expected := map[string]bool{"main": true, "acc": true, "staking": true, "slashing": true, "mint": true, "distr": true, "gov": true, "fee": true, "params": true}
	if len(info.StoreInfos) != len(expected) {
		return info, fmt.Errorf("private history store set invalid")
	}
	for _, s := range info.StoreInfos {
		if !expected[s.Name] || s.Core.CommitID.Version != h {
			return info, fmt.Errorf("private history store identity invalid")
		}
		delete(expected, s.Name)
	}
	raw, err := json.Marshal(info)
	var proof rootmulti.MultiStoreProof
	if err != nil || json.Unmarshal(raw, &proof) != nil || !bytes.Equal(proof.ComputeRootHash(), app.LastCommitID().Hash) {
		return info, fmt.Errorf("private history native root differs")
	}
	return info, nil
}

func auditHistoryAnchor(f *evidenceWriter, db dbm.DB, app replayApp, chain, genesis string) error {
	if f == nil {
		return nil
	}
	in, err := os.Open(genesis)
	if err != nil {
		return fmt.Errorf("native history genesis unavailable")
	}
	hash := sha256.New()
	_, err = io.Copy(hash, in)
	in.Close()
	if err != nil {
		return fmt.Errorf("native history genesis unreadable")
	}
	var roots *historyCommitInfo
	if app.LastBlockHeight() > 0 {
		info, err := historyRoots(db, app)
		if err != nil {
			return err
		}
		roots = &info
	}
	row := struct {
		Schema        string             `json:"schema"`
		Kind          string             `json:"kind"`
		Release       string             `json:"release"`
		Chain         string             `json:"chain"`
		Height        int64              `json:"height"`
		Commit        sdk.CommitID       `json:"commit"`
		Roots         *historyCommitInfo `json:"native_commit_info"`
		GenesisSHA256 string             `json:"genesis_sha256"`
	}{"native033-lossless-history-v1", "source_anchor", release, chain, app.LastBlockHeight(), app.LastCommitID(), roots, hex.EncodeToString(hash.Sum(nil))}
	if json.NewEncoder(f).Encode(row) != nil || f.Sync() != nil {
		return fmt.Errorf("native history anchor output failed")
	}
	return nil
}

// A clean gzip trailer does not seal history. This marker is written ONLY
// after the driver's full next-header/validator/results verification succeeds.
func completeNativeHistory(f *evidenceWriter, app replayApp, first int64, next cached) error {
	row := struct {
		Schema        string         `json:"schema"`
		Kind          string         `json:"kind"`
		First         int64          `json:"first_height"`
		Height        int64          `json:"height"`
		Commit        sdk.CommitID   `json:"commit"`
		NextHeader    tmtypes.Header `json:"unexecuted_next_header"`
		NextBlockHash []byte         `json:"unexecuted_next_block_hash"`
	}{"native033-lossless-history-v1", "history_completed", first, app.LastBlockHeight(), app.LastCommitID(), next.block.Header, next.metaID}
	if json.NewEncoder(f).Encode(row) != nil || f.Sync() != nil {
		return fmt.Errorf("native history completion output failed")
	}
	return nil
}

// Compact HISTORY is lossless by reference to the complete raw-state anchors,
// per-phase deltas and source Set/Delete journal. It preserves native roots and
// ALL typed ABCI results without dumping nine full stores at every height.
func auditNativeHistory(f *evidenceWriter, db dbm.DB, app replayApp, c cached, begin abci.ResponseBeginBlock, deliver []*abci.ResponseDeliverTx, end abci.ResponseEndBlock) error {
	if f == nil {
		return nil
	}
	roots, err := historyRoots(db, app)
	if err != nil {
		return err
	}
	txs := make([][]byte, len(c.block.Data.Txs))
	for i, tx := range c.block.Data.Txs {
		txs[i] = tx
	}
	row := struct {
		Schema       string                    `json:"schema"`
		Kind         string                    `json:"kind"`
		Release      string                    `json:"release"`
		Chain        string                    `json:"chain"`
		Height       int64                     `json:"height"`
		Header       tmtypes.Header            `json:"header"`
		BlockHash    []byte                    `json:"block_hash"`
		Commit       sdk.CommitID              `json:"commit"`
		Roots        historyCommitInfo         `json:"native_commit_info"`
		Transactions [][]byte                  `json:"transactions"`
		Begin        abci.ResponseBeginBlock   `json:"begin"`
		Deliver      []*abci.ResponseDeliverTx `json:"deliver"`
		End          abci.ResponseEndBlock     `json:"end"`
	}{"native033-lossless-history-v1", "height", release, c.block.ChainID, c.height, c.block.Header, c.metaID, app.LastCommitID(), roots, txs, begin, deliver, end}
	if json.NewEncoder(f).Encode(row) != nil || f.Sync() != nil {
		return fmt.Errorf("cannot persist private native history")
	}
	return nil
}
