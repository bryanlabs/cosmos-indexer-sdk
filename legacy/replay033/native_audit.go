package main

import (
	"encoding/json"
	"fmt"
	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"os"
)

type nativeStoreEvidence struct {
	Name       string               `json:"name"`
	Partitions []abci.ResponseQuery `json:"partitions"`
}

// Native033 subspace reads latest regardless of requested height. Use only
// immediately after loading the exact checkpoint or committing this height.
func auditNativeStores(app replayApp, h int64) ([]nativeStoreEvidence, error) {
	if h < 1 || app.LastBlockHeight() != h {
		return nil, fmt.Errorf("native audit application height mismatch")
	}
	names := []string{baseapp.MainStoreKey, auth.StoreKey, staking.StoreKey, slashing.StoreKey, mint.StoreKey, distribution.StoreKey, gov.StoreKey, auth.FeeStoreKey, params.StoreKey}
	stores := make([]nativeStoreEvidence, 0, len(names))
	for _, name := range names {
		partitions := make([]abci.ResponseQuery, 256)
		// Empty native query data is refused. All256 first-byte prefixes exhaust
		// every legal nonempty native key, including 0xff. Empty results stay too.
		for p := 0; p < 256; p++ {
			res := app.Query(abci.RequestQuery{Path: "/store/" + name + "/subspace", Height: h, Data: []byte{byte(p)}})
			if res.Code != 0 || res.Height != h {
				return nil, fmt.Errorf("native full-store audit query refused: store=%s prefix=%d code=%d height=%d expected=%d", name, p, res.Code, res.Height, h)
			}
			partitions[p] = res
		}
		stores = append(stores, nativeStoreEvidence{name, partitions})
	}
	return stores, nil
}
func auditNativeCheckpoint(path, chain string, app replayApp) error {
	stores, err := auditNativeStores(app, app.LastBlockHeight())
	if err != nil {
		return err
	}
	f, err := createPrivateEvidence(path)
	if err != nil {
		return err
	}
	defer f.Close()
	record := struct {
		Release string                `json:"release"`
		Chain   string                `json:"chain"`
		Height  int64                 `json:"height"`
		Commit  sdk.CommitID          `json:"commit"`
		Stores  []nativeStoreEvidence `json:"all_persistent_stores"`
	}{release, chain, app.LastBlockHeight(), app.LastCommitID(), stores}
	if json.NewEncoder(f).Encode(record) != nil || f.Sync() != nil {
		return fmt.Errorf("cannot persist full source-checkpoint audit")
	}
	return nil
}

// Either untouched Gaia or the observer: COMPLETE native ABCI responses, signed
// bytes and all committed raw store subspaces, never a financial-field subset.
func auditNative(f *os.File, app replayApp, c cached, begin abci.ResponseBeginBlock, deliver []*abci.ResponseDeliverTx, end abci.ResponseEndBlock) error {
	if f == nil {
		return nil
	}
	stores, err := auditNativeStores(app, c.height)
	if err != nil {
		return err
	}
	txs := make([][]byte, len(c.block.Data.Txs))
	for i, tx := range c.block.Data.Txs {
		txs[i] = tx
	}
	record := struct {
		Release      string                    `json:"release"`
		Chain        string                    `json:"chain"`
		Height       int64                     `json:"height"`
		BlockHash    []byte                    `json:"block_hash"`
		Commit       sdk.CommitID              `json:"commit"`
		Transactions [][]byte                  `json:"transactions"`
		Begin        abci.ResponseBeginBlock   `json:"begin"`
		Deliver      []*abci.ResponseDeliverTx `json:"deliver"`
		End          abci.ResponseEndBlock     `json:"end"`
		Stores       []nativeStoreEvidence     `json:"all_persistent_stores"`
	}{release, c.block.ChainID, c.height, c.metaID, app.LastCommitID(), txs, begin, deliver, end, stores}
	if json.NewEncoder(f).Encode(record) != nil || f.Sync() != nil {
		return fmt.Errorf("cannot persist private native audit")
	}
	return nil
}
