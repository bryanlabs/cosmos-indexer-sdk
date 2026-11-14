package observeapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/gov"
	abci "github.com/tendermint/tendermint/abci/types"
	tmtypes "github.com/tendermint/tendermint/types"
)

// Records contain private financial data. The caller must provide a private,
// exclusive destination. An io.Writer cannot promise fsync or atomic batches.
// Status is committed, aborted (attempted writes discarded), or exported.
// Amount is a signed integer, never a floating point value.
type Record struct {
	Classification  string          `json:"classification,omitempty"`
	EvidenceQuality string          `json:"evidence_quality,omitempty"`
	Before          json.RawMessage `json:"before,omitempty"`
	After           json.RawMessage `json:"after,omitempty"`
	Unit            string          `json:"unit,omitempty"`
	Validator       string          `json:"validator,omitempty"`
	Delegator       string          `json:"delegator,omitempty"`
	Kind            string          `json:"kind"`
	Code            uint32          `json:"code"`
	Height          int64           `json:"height"`
	Tx              int             `json:"tx"`
	Message         int             `json:"message"`
	Phase           string          `json:"phase"`
	Label           string          `json:"label"`
	Status          string          `json:"status"`
	Address         string          `json:"address"`
	Denom           string          `json:"denom"`
	Amount          string          `json:"amount"`
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
	blockFence           *accountingFence
	blockNative          func()
	fullSnapshots        uint64
	baselineCaptured     bool
	handlers             int
	handlerFailure       bool
	deposits             []gov.Deposit
	anteCalled           bool
	exporting            bool
	exportedMovements    []Record
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
	app.observer.fullSnapshots++
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

// Full account snapshots are the independent bank reconciliation oracle.
type accountingFence struct {
	ctx    sdk.Context
	before balances
}

func (app *GaiaApp) openAccounting(ctx sdk.Context) *accountingFence {
	return &accountingFence{ctx: ctx, before: app.snapshot(ctx)}
}
func (app *GaiaApp) closeAccounting(f *accountingFence, records []Record) (balances, balances) {
	if f == nil {
		app.fail()
		return balances{}, balances{}
	}
	return f.before, app.snapshot(f.ctx)
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
		class, known := movementClass(label)
		if !known {
			app.fail()
		}
		o.pending = append(o.pending, Record{Kind: "movement", Validator: o.validator, Delegator: o.delegator, Height: o.height, Tx: o.tx, Message: o.message, Phase: o.phase, Label: label, Address: k.address, Denom: k.denom, Amount: d.String(), Before: app.nativeJSON(amount(before, k).String()), After: app.nativeJSON(amount(after, k).String()), Unit: "native_bank_tokens", Classification: class, EvidenceQuality: "source_native_bank_call"})
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
	fence := app.openAccounting(ctx)
	finishNative := app.nativeFence(ctx)
	start := len(o.pending)
	defer func() {
		finishNative()
		before, after := app.closeAccounting(fence, o.pending[start:])
		app.reconcile(before, after, o.pending[start:])
		app.marker("accounting", name, 0)
		// Private partial phases are written, but only export_completed seals
		// the whole attempt. Interrupted prefixes remain inadmissible.
		if o.exporting && o.err == nil {
			for _, r := range o.pending {
				if r.Kind == "movement" {
					o.exportedMovements = append(o.exportedMovements, r)
				}
			}
			app.flush("exported")
		}
		o.phase, o.store = previous, previousStore
	}()
	fn()
}
func (app *GaiaApp) observedBeginBlocker(ctx sdk.Context, req abci.RequestBeginBlock) (res abci.ResponseBeginBlock) {
	if app.observer.active {
		app.observer.blockFence = app.openAccounting(ctx)
		previous := app.observer.phase
		app.observer.phase = "block"
		if !app.observer.baselineCaptured {
			app.nativeCheckpoint(ctx, "segment_baseline")
			app.observer.baselineCaptured = true
		}
		app.observer.blockNative = app.nativeFence(ctx)
		app.observer.phase = previous
	}
	app.phase(ctx, "begin_block", func() { res = app.BeginBlocker(ctx, req) })
	return
}
func (app *GaiaApp) observedEndBlocker(ctx sdk.Context, req abci.RequestEndBlock) (res abci.ResponseEndBlock) {
	if app.observer.active {
		app.observer.deposits = app.governanceDeposits(ctx)
	}
	app.phase(ctx, "end_block", func() { res = app.EndBlocker(ctx, req) })
	if len(app.observer.deposits) != 0 {
		app.fail()
	}
	return
}
func (app *GaiaApp) observeHandler(h sdk.Handler) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) (res sdk.Result) {
		o := &app.observer
		if !o.active || ctx.IsCheckTx() {
			return h(ctx, msg)
		}
		o.message++
		if !supportedMessage(msg) {
			app.execution("message", "unsupported_unknown", nil, nil, 0)
			app.fail()
		}
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
		restore := app.messageCause(msg)
		defer restore()
		start := len(o.pending)
		principal := app.principal(ctx, msg)
		app.phase(ctx, "message", func() { res = h(ctx, msg) })
		principal(res.IsOK())
		app.execution("message", msg.Route()+"/"+msg.Type(), msg, res, uint32(res.Code))
		returned = true
		for i := start; i < len(o.pending); i++ {
			o.pending[i].Code = uint32(res.Code)
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
		o.anteCalled = true
		previousPhase := o.phase
		o.phase = "ante"
		defer func() { o.phase = previousPhase }()
		fence := app.openAccounting(ctx)
		finishNative := app.nativeFence(ctx)
		start := len(o.pending)
		returned := false
		defer func() {
			if !returned {
				abort = true
			}
			afterCtx := ctx
			if !newCtx.IsZero() {
				afterCtx = newCtx
			}
			finishNative()
			fence.ctx = afterCtx
			before, after := app.closeAccounting(fence, o.pending[start:])
			// auth v0.33.2 writes the payer directly, not through bank. Independently
			// expect its signed fee only, and reconcile against the complete account set.
			std, ok := tx.(auth.StdTx)
			if ok && len(std.GetSigners()) > 0 {
				payer := std.GetSigners()[0].String()
				for _, c := range std.Fee.Amount {
					k := balanceKey{payer, c.Denom}
					if !abort || amount(after, k).Equal(amount(before, k).Sub(c.Amount)) {
						o.pending = append(o.pending, Record{Kind: "movement", Unit: "native_bank_tokens", Classification: "signed_ante_fee", EvidenceQuality: "signed_fee_and_full_account_reconciliation", Height: o.height, Tx: o.tx, Message: -1, Phase: "ante", Label: "ante_fee", Address: payer, Denom: c.Denom, Amount: c.Amount.Neg().String()})
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
		}()
		newCtx, res, abort = h(ctx, tx, sim)
		returned = true
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
	res := app.BaseApp.BeginBlock(req)
	app.execution("begin_block", "native_abci", req, res, 0)
	return res
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
	o.anteCalled = false
	ctx := app.NewContext(false, abci.Header{Height: o.height})
	fence := app.openAccounting(ctx)
	previousPhase := o.phase
	o.phase = "transaction"
	finishNative := app.nativeFence(ctx)
	o.phase = previousPhase
	start := len(o.pending)
	res := app.BaseApp.DeliverTx(tx)
	finishNative()
	if !o.anteCalled {
		o.pending = append(o.pending, Record{Kind: "accounting", Height: o.height, Tx: o.tx, Message: -1, Phase: "ante", Label: "ante_not_invoked", Status: "aborted", Code: res.Code})
	}
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
	before, after := app.closeAccounting(fence, retained)
	app.reconcile(before, after, retained)
	o.message = -1
	app.marker("accounting", "transaction", res.Code)
	app.execution("transaction", "native_abci", tx, &res, res.Code)
	return res
}
func (app *GaiaApp) EndBlock(req abci.RequestEndBlock) abci.ResponseEndBlock {
	if app.observer.err != nil {
		return abci.ResponseEndBlock{}
	}
	app.observer.tx, app.observer.message = -1, -1
	res := app.BaseApp.EndBlock(req)
	app.execution("end_block", "native_abci", req, res, 0)
	return res
}

// Commit never calls BaseApp after a latched failure. A post-commit output
// failure cannot undo the DB commit: callers must check RecorderError again and
// stop, never continue with an incomplete evidence stream.
func (app *GaiaApp) Commit() abci.ResponseCommit {
	o := &app.observer
	if app.RecorderError() != nil {
		return abci.ResponseCommit{}
	}
	if o.active {
		if o.blockNative == nil {
			app.fail()
			return abci.ResponseCommit{}
		}
		o.blockNative()
		o.blockNative = nil
		var retained []Record
		for _, r := range o.pending {
			if r.Status != "aborted" {
				retained = append(retained, r)
			}
		}
		before, after := app.closeAccounting(o.blockFence, retained)
		o.blockFence = nil
		app.reconcile(before, after, retained)
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
// emits private partial phases, sealed ONLY after the complete export succeeds.
// A missing export_completed marker makes the entire attempt inadmissible.
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
		o.exporting = false
		o.exportedMovements = nil
	}()
	o.exporting = o.writer != nil
	if o.writer != nil {
		o.active = true
		o.height = app.LastBlockHeight()
		o.tx = -1
		o.message = -1
		o.phase = "zero_height_export"
	}
	ctx := app.NewContext(true, abci.Header{Height: app.LastBlockHeight()})
	o.store = ctx.MultiStore().GetKVStore(app.keyAccount)
	var fence *accountingFence
	finishNative := func() {}
	if o.active {
		fence = app.openAccounting(ctx)
		app.nativeCheckpoint(ctx, "export_baseline")
		finishNative = app.nativeFence(ctx)
	}
	// Close even on export failure or panic, before the view is abandoned.
	defer func() {
		if fence != nil {
			app.closeAccounting(fence, o.pending)
		}
	}()
	state, vals, err = app.ExportAppStateAndValidators(true, nil)
	finishNative()
	if o.active {
		app.nativeCheckpoint(ctx, "export_final")
	}
	if err != nil {
		app.fail()
	}
	if o.active {
		before, after := app.closeAccounting(fence, o.pending)
		fence = nil
		records := append(o.exportedMovements, o.pending...)
		app.reconcile(before, after, records)
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
