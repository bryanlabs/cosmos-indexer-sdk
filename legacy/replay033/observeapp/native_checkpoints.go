package observeapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
)

// SealRecorder preserves the entire final committed state after the caller's
// target next-header proof. It does not commit or reinterpret incomplete output.
func (app *GaiaApp) SealRecorder() error {
	o := &app.observer
	if o.err != nil {
		return o.err
	}
	if o.active || o.writer == nil || len(o.pending) != 0 {
		app.fail()
		return o.err
	}
	o.tx, o.message = -1, -1
	o.phase = "segment"
	app.nativeCheckpoint(app.NewContext(true, abci.Header{Height: app.LastBlockHeight()}), "segment_final")
	if o.err != nil {
		o.pending = nil
		return o.err
	}
	app.marker("observation_completed", "segment", 0)
	app.flush("committed")
	return o.err
}
func (app *GaiaApp) nativeDigest(values []nativeKV) json.RawMessage {
	hash := sha256.New()
	if json.NewEncoder(hash).Encode(values) != nil {
		app.fail()
		return nil
	}
	return app.nativeJSON(struct {
		SHA256 string `json:"sha256"`
		Keys   int    `json:"keys"`
	}{hex.EncodeToString(hash.Sum(nil)), len(values)})
}

// Full snapshots bracket an observation. Per-scope commitments and complete
// before/after changed keys reconstruct every intermediate native cache state,
// including failed attempts. No unobserved value is normalized or discarded.
func (app *GaiaApp) nativeCheckpoint(ctx sdk.Context, label string) {
	stores := app.nativeStores()
	values, ok := app.nativeSnapshot(ctx, stores)
	if !ok {
		return
	}
	for i, s := range stores {
		r := app.nativeRecord("native_state_checkpoint", label+"/store:"+s.key.Name()+":"+s.name, "native_store_bytes")
		r.Before = app.nativeJSON(values[i].values)
		app.observer.pending = append(app.observer.pending, r)
	}
}
