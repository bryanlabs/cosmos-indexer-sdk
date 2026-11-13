package observeapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	abci "github.com/tendermint/tendermint/abci/types"
	tmtypes "github.com/tendermint/tendermint/types"
)

// Records contain private financial data. The caller must provide a private,
// exclusive destination. An io.Writer cannot promise fsync or atomic batches.
// Status is committed, aborted (attempted writes discarded), or exported.
// Amount is a signed integer, never a floating point value.
type Record struct {
	Validator string `json:"validator,omitempty"`
	Delegator string `json:"delegator,omitempty"`
	Kind      string `json:"kind"`
	Code      uint32 `json:"code"`
	Height    int64  `json:"height"`
	Tx        int    `json:"tx"`
	Message   int    `json:"message"`
	Phase     string `json:"phase"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Address   string `json:"address"`
	Denom     string `json:"denom"`
	Amount    string `json:"amount"`
}

type balanceKey struct{ address, denom string }
type balances map[balanceKey]sdk.Int

type observer struct {
	writer               io.Writer
	err                  error
	active               bool
	phase, label         string
	validator, delegator string
	height               int64
	tx, message          int
	pending              []Record
	store                sdk.KVStore
	blockBefore          balances
	handlers             int
	handlerFailure       bool
}

// SetRecorder enables optional accounting before a block or export. It may not
// be changed within an active block or after an error. nil disables observation.
// All operations are serial, like BaseApp itself.
func (app *GaiaApp) SetRecorder(w io.Writer) {
	o := &app.observer
	if o.active || len(o.pending) != 0 || o.err != nil {
		app.fail()
		return
	}
	o.writer = w
}
func (app *GaiaApp) RecorderError() error { return app.observer.err }
func (app *GaiaApp) marker(kind, phase string, code uint32) {
	o := &app.observer
	o.pending = append(o.pending, Record{Kind: kind, Validator: o.validator, Delegator: o.delegator, Height: o.height, Tx: o.tx, Message: o.message, Phase: phase, Label: o.label, Code: code})
}
func (app *GaiaApp) fail() {
	if app.observer.err == nil {
		app.observer.err = errors.New("private replay accounting failed; stop")
	}
}
func quiet(ctx sdk.Context) sdk.Context {
	return ctx.WithGasMeter(sdk.NewInfiniteGasMeter()).WithBlockGasMeter(sdk.NewInfiniteGasMeter())
}
func (app *GaiaApp) snapshot(ctx sdk.Context) balances {
	out := balances{}
	app.accountKeeper.IterateAccounts(quiet(ctx), func(a auth.Account) bool {
		for _, c := range a.GetCoins() {
			if !c.Amount.IsZero() {
				out[balanceKey{a.GetAddress().String(), c.Denom}] = c.Amount
			}
		}
		return false
	})
	return out
}
func amount(b balances, k balanceKey) sdk.Int {
	if n, ok := b[k]; ok {
		return n
	}
	return sdk.ZeroInt()
}
func equal(a, b balances) bool {
	for k, v := range a {
		if !v.Equal(amount(b, k)) {
			return false
		}
	}
	for k, v := range b {
		if !v.Equal(amount(a, k)) {
			return false
		}
	}
	return true
}
func apply(before balances, records []Record) balances {
	out := balances{}
	for k, v := range before {
		out[k] = v
	}
	for _, r := range records {
		if r.Kind != "movement" {
			continue
		}
		k := balanceKey{r.Address, r.Denom}
		n, ok := sdk.NewIntFromString(r.Amount)
		if !ok {
			continue
		}
		out[k] = amount(out, k).Add(n)
	}
	return out
}
func (app *GaiaApp) reconcile(before, after balances, records []Record) {
	if !equal(apply(before, records), after) {
		app.fail()
	}
}
func (app *GaiaApp) recordDelta(before, after balances, label string) {
	keys := map[balanceKey]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	ordered := make([]balanceKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].address != ordered[j].address {
			return ordered[i].address < ordered[j].address
		}
		return ordered[i].denom < ordered[j].denom
	})
	o := &app.observer
	for _, k := range ordered {
		d := amount(after, k).Sub(amount(before, k))
		if d.IsZero() {
			continue
		}
		o.pending = append(o.pending, Record{Kind: "movement", Validator: o.validator, Delegator: o.delegator, Height: o.height, Tx: o.tx, Message: o.message, Phase: o.phase, Label: label, Address: k.address, Denom: k.denom, Amount: d.String()})
	}
}
func (app *GaiaApp) phase(ctx sdk.Context, name string, fn func()) {
	o := &app.observer
	if !o.active {
		fn()
		return
	}
	previous, previousStore := o.phase, o.store
	o.phase, o.store = name, ctx.MultiStore().GetKVStore(app.keyAccount)
	before := app.snapshot(ctx)
	start := len(o.pending)
	defer func() {
		app.reconcile(before, app.snapshot(ctx), o.pending[start:])
		app.marker("accounting", name, 0)
		o.phase, o.store = previous, previousStore
	}()
	fn()
}
func (app *GaiaApp) observedBeginBlocker(ctx sdk.Context, req abci.RequestBeginBlock) (res abci.ResponseBeginBlock) {
	if app.observer.active {
		app.observer.blockBefore = app.snapshot(ctx)
	}
	app.phase(ctx, "begin_block", func() { res = app.BeginBlocker(ctx, req) })
	return
}
func (app *GaiaApp) observedEndBlocker(ctx sdk.Context, req abci.RequestEndBlock) (res abci.ResponseEndBlock) {
	app.phase(ctx, "end_block", func() { res = app.EndBlocker(ctx, req) })
	return
}
func (app *GaiaApp) observeHandler(h sdk.Handler) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) (res sdk.Result) {
		o := &app.observer
		if !o.active || ctx.IsCheckTx() {
			return h(ctx, msg)
		}
		o.message++
		o.handlers++
		returned := false
		defer func() {
			if !returned || !res.IsOK() {
				o.handlerFailure = true
			}
		}()
		previous := o.label
		o.label = msg.Route() + "/" + msg.Type()
		defer func() { o.label = previous }()
		app.phase(ctx, "message", func() { res = h(ctx, msg) })
		returned = true
		if len(o.pending) > 0 {
			o.pending[len(o.pending)-1].Code = uint32(res.Code)
		}
		return
	}
}
func (app *GaiaApp) observeAnte(h sdk.AnteHandler) sdk.AnteHandler {
	return func(ctx sdk.Context, tx sdk.Tx, sim bool) (newCtx sdk.Context, res sdk.Result, abort bool) {
		o := &app.observer
		if !o.active || ctx.IsCheckTx() || sim {
			return h(ctx, tx, sim)
		}
		before := app.snapshot(ctx)
		start := len(o.pending)
		newCtx, res, abort = h(ctx, tx, sim)
		afterCtx := ctx
		if !newCtx.IsZero() {
			afterCtx = newCtx
		}
		after := app.snapshot(afterCtx)
		// auth v0.34.10 writes the payer directly, not through bank. Independently
		// expect its signed fee only, and reconcile against the complete account set.
		std, ok := tx.(auth.StdTx)
		if ok && len(std.GetSigners()) > 0 {
			payer := std.GetSigners()[0].String()
			for _, c := range std.Fee.Amount {
				k := balanceKey{payer, c.Denom}
				if !abort || amount(after, k).Equal(amount(before, k).Sub(c.Amount)) {
					o.pending = append(o.pending, Record{Kind: "movement", Height: o.height, Tx: o.tx, Message: -1, Phase: "ante", Label: "ante_fee", Address: payer, Denom: c.Denom, Amount: c.Amount.Neg().String()})
				}
			}
		}
		app.reconcile(before, after, o.pending[start:])
		app.marker("accounting", "ante", uint32(res.Code))
		if abort {
			for i := start; i < len(o.pending); i++ {
				o.pending[i].Status = "aborted"
			}
		}
		return
	}
}

func (app *GaiaApp) BeginBlock(req abci.RequestBeginBlock) abci.ResponseBeginBlock {
	o := &app.observer
	if o.err != nil {
		return abci.ResponseBeginBlock{}
	}
	if o.writer != nil {
		if o.active {
			app.fail()
			return abci.ResponseBeginBlock{}
		}
		o.active = true
		o.height = req.Header.Height
		o.tx = -1
		o.message = -1
	}
	return app.BaseApp.BeginBlock(req)
}
func (app *GaiaApp) DeliverTx(tx []byte) abci.ResponseDeliverTx {
	o := &app.observer
	if o.err != nil {
		return abci.ResponseDeliverTx{Code: 1, Log: "private replay accounting failed; stop"}
	}
	if !o.active {
		return app.BaseApp.DeliverTx(tx)
	}
	o.tx++
	o.message = -1
	o.handlers, o.handlerFailure = 0, false
	ctx := app.NewContext(false, abci.Header{Height: o.height})
	before := app.snapshot(ctx)
	start := len(o.pending)
	res := app.BaseApp.DeliverTx(tx)
	// BaseApp may return a deferred gas error after writing the message cache.
	// Net-zero movements cannot disambiguate that case. Never classify them
	// as aborted merely because the final response failed.
	if res.Code != 0 && o.handlers > 0 && !o.handlerFailure {
		app.fail()
	}
	var retained []Record
	for i := start; i < len(o.pending); i++ {
		r := &o.pending[i]
		if r.Status == "aborted" {
			continue
		}
		if r.Phase == "message" && res.Code != 0 {
			r.Status = "aborted"
			continue
		}
		retained = append(retained, *r)
	}
	app.reconcile(before, app.snapshot(ctx), retained)
	o.message = -1
	app.marker("accounting", "transaction", res.Code)
	return res
}
func (app *GaiaApp) EndBlock(req abci.RequestEndBlock) abci.ResponseEndBlock {
	if app.observer.err != nil {
		return abci.ResponseEndBlock{}
	}
	app.observer.tx, app.observer.message = -1, -1
	return app.BaseApp.EndBlock(req)
}

// Commit never calls BaseApp after a latched failure. A post-commit output
// failure cannot undo the DB commit: callers must check RecorderError again and
// stop, never continue with an incomplete evidence stream.
func (app *GaiaApp) Commit() abci.ResponseCommit {
	o := &app.observer
	if o.err != nil {
		return abci.ResponseCommit{}
	}
	if o.active {
		var retained []Record
		for _, r := range o.pending {
			if r.Status != "aborted" {
				retained = append(retained, r)
			}
		}
		app.reconcile(o.blockBefore, app.snapshot(app.NewContext(false, abci.Header{Height: o.height})), retained)
		if o.err != nil {
			return abci.ResponseCommit{}
		}
	}
	res := app.BaseApp.Commit()
	if o.active {
		o.tx, o.message = -1, -1
		app.marker("height_completed", "block", 0)
		app.flush("committed")
		o.active = false
	}
	return res
}
func (app *GaiaApp) flush(status string) {
	o := &app.observer
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range o.pending {
		if r.Status == "" {
			r.Status = status
		}
		if enc.Encode(r) != nil {
			app.fail()
			return
		}
	}
	if buf.Len() > 0 {
		n, err := o.writer.Write(buf.Bytes())
		if err != nil || n != buf.Len() {
			app.fail()
		}
	}
	o.pending = nil
}

// ExportObserved invokes the actual pinned zero-height export. It only changes
// BaseApp's check cache, and must be used on a separate, read-only DB app. It
// emits exported records after the complete export succeeds, never Commit.
// Upstream export panics are converted to a generic private-accounting error.
func (app *GaiaApp) ExportObserved() (state json.RawMessage, vals []tmtypes.GenesisValidator, err error) {
	o := &app.observer
	if o.err != nil {
		return nil, nil, o.err
	}
	if o.active || len(o.pending) != 0 {
		app.fail()
		return nil, nil, o.err
	}
	defer func() {
		if recover() != nil {
			app.fail()
			state = nil
			vals = nil
			err = o.err
		}
		o.active = false
		o.pending = nil
	}()
	if o.writer != nil {
		o.active = true
		o.height = app.LastBlockHeight()
		o.tx = -1
		o.message = -1
		o.phase = "zero_height_export"
	}
	ctx := app.NewContext(true, abci.Header{Height: app.LastBlockHeight()})
	o.store = ctx.MultiStore().GetKVStore(app.keyAccount)
	var before balances
	if o.active {
		before = app.snapshot(ctx)
	}
	state, vals, err = app.ExportAppStateAndValidators(true, nil)
	if err != nil {
		app.fail()
	}
	if o.active && o.err == nil {
		app.reconcile(before, app.snapshot(ctx), o.pending)
	}
	if o.err != nil {
		return nil, nil, o.err
	}
	if o.active {
		app.marker("accounting", "zero_height_export", 0)
		app.marker("export_completed", "zero_height_export", 0)
		app.flush("exported")
	}
	if o.err != nil {
		return nil, nil, o.err
	}
	return state, vals, nil
}
