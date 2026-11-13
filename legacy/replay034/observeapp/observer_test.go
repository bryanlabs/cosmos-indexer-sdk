package observeapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	upstream "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

// Never print fixture accounts, transaction bytes, errors from SDK, or balances.
func must(t *testing.T, ok bool, reason string) {
	t.Helper()
	if !ok {
		t.Fatal(reason)
	}
}
func coins(n int64) sdk.Coins { return sdk.NewCoins(sdk.NewInt64Coin("stake", n)) }
func fixture(t *testing.T, validator bool, maxGas ...int64) (*GaiaApp, dbm.DB, secp256k1.PrivKeySecp256k1, []byte) {
	t.Helper()
	key := secp256k1.GenPrivKeySecp256k1([]byte("private synthetic observation fixture"))
	addr := sdk.AccAddress(key.PubKey().Address())
	state := NewDefaultGenesisState()
	state.Accounts = []GenesisAccount{{Address: addr, Coins: coins(10000000)}}
	state.StakingData.Pool.NotBondedTokens = sdk.NewInt(10000000)
	if validator {
		v := staking.NewValidator(sdk.ValAddress(addr), key.PubKey(), staking.Description{})
		v.Tokens = sdk.NewInt(1000000)
		v.DelegatorShares = sdk.NewDec(1000000)
		state.StakingData.Validators = []staking.Validator{v}
		state.StakingData.Delegations = []staking.Delegation{{DelegatorAddress: addr, ValidatorAddress: sdk.ValAddress(addr), Shares: sdk.NewDec(1000000)}}
		state.StakingData.Pool.NotBondedTokens = state.StakingData.Pool.NotBondedTokens.AddRaw(1000000)
	}
	db := dbm.NewMemDB()
	app := NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	raw, err := app.cdc.MarshalJSON(state)
	must(t, err == nil, "encode genesis")
	req := abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"}
	if len(maxGas) > 0 {
		req.ConsensusParams = &abci.ConsensusParams{Block: &abci.BlockParams{MaxGas: maxGas[0]}}
	}
	app.InitChain(req)
	return app, db, key, raw
}
func begin(app *GaiaApp, h int64) {
	app.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: h, ChainID: "observe-test", Time: time.Unix(1500000000+h, 0)}})
}
func signed(t *testing.T, app *GaiaApp, key secp256k1.PrivKeySecp256k1, seq uint64, msgs ...sdk.Msg) []byte {
	t.Helper()
	fee := auth.NewStdFee(200000, coins(7))
	sign := auth.StdSignBytes("observe-test", 0, seq, fee, msgs, "")
	sig, err := key.Sign(sign)
	must(t, err == nil, "sign fixture")
	tx := auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: key.PubKey(), Signature: sig}}, "")
	raw, err := app.cdc.MarshalBinaryLengthPrefixed(tx)
	must(t, err == nil, "encode fixture")
	return raw
}
func rows(t *testing.T, b *bytes.Buffer) []Record {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b.Bytes()))
	var out []Record
	for {
		var r Record
		err := d.Decode(&r)
		if err == io.EOF {
			break
		}
		must(t, err == nil, "decode private records")
		out = append(out, r)
	}
	return out
}
func TestAbortedMultiMessageRetainsAnte(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	payer := sdk.AccAddress(key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{3}, 20))
	before := app.snapshot(app.NewContext(false, abci.Header{}))
	first := bank.NewMsgSend(payer, receiver, coins(100))
	second := bank.NewMsgSend(payer, receiver, coins(20000000))
	res := app.DeliverTx(signed(t, app, key, 0, first, second))
	must(t, res.Code != 0, "expected later message failure")
	ctx := app.NewContext(false, abci.Header{})
	must(t, app.bankKeeper.GetCoins(quiet(ctx), receiver).IsZero(), "message cache must roll back")
	want := apply(before, []Record{{Kind: "movement", Address: payer.String(), Denom: "stake", Amount: "-7"}})
	must(t, equal(want, app.snapshot(ctx)), "only successful ante fee persists")
	must(t, app.RecorderError() == nil, "rollback accounting")
	must(t, out.Len() == 0, "no records before commit")
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	must(t, app.RecorderError() == nil, "commit accounting")
	var aborted, fee, txMarker, height bool
	for _, r := range rows(t, &out) {
		if r.Phase == "message" && r.Kind == "movement" {
			must(t, r.Status == "aborted", "attempted transfer is not committed")
			aborted = true
		}
		if r.Label == "ante_fee" {
			must(t, r.Status == "committed", "fee retained")
			fee = true
		}
		if r.Phase == "transaction" {
			txMarker = r.Code != 0
		}
		if r.Kind == "height_completed" {
			height = true
		}
	}
	if !(aborted && fee && txMarker && height) {
		t.Logf("flags %t %t %t %t, code %d", aborted, fee, txMarker, height, res.Code)
	}
	must(t, aborted && fee && txMarker && height, "complete rollback evidence")
}
func TestFailedAnteAndEmptyBlockMarkers(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	payer := sdk.AccAddress(key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{4}, 20))
	before := app.snapshot(app.NewContext(false, abci.Header{}))
	res := app.DeliverTx(signed(t, app, key, 9, bank.NewMsgSend(payer, receiver, coins(1))))
	must(t, res.Code != 0, "invalid sequence must fail ante")
	must(t, equal(before, app.snapshot(app.NewContext(false, abci.Header{}))), "failed ante discarded")
	must(t, app.RecorderError() == nil, "failed ante reconciliation")
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	begin(app, 2)
	app.EndBlock(abci.RequestEndBlock{Height: 2})
	app.Commit()
	completed := 0
	for _, r := range rows(t, &out) {
		if r.Kind == "height_completed" {
			completed++
		}
		must(t, r.Kind != "movement", "failed ante no actual debit")
	}
	must(t, completed == 2, "including empty height completion")
}
func TestDirectDeltaMismatchRefusesCommit(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	ctx := app.NewContext(false, abci.Header{})
	payer := sdk.AccAddress(key.PubKey().Address())
	app.phase(ctx, "test_direct", func() {
		a := app.accountKeeper.GetAccount(ctx, payer)
		must(t, a.SetCoins(coins(1)) == nil, "fixture mutation")
		app.accountKeeper.SetAccount(ctx, a)
	})
	must(t, app.RecorderError() != nil, "unobserved direct write must fail")
	old := app.LastCommitID()
	app.Commit()
	must(t, app.LastCommitID().Version == old.Version && bytes.Equal(app.LastCommitID().Hash, old.Hash), "failed accounting cannot commit")
	must(t, out.Len() == 0, "failed block emits no records")
}

type badWriter struct{ short bool }

func (w badWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("sensitive destination details")
}
func TestOutputFailureIsLatchedAndScrubbed(t *testing.T) {
	for _, short := range []bool{false, true} {
		app, _, _, _ := fixture(t, false)
		app.SetRecorder(badWriter{short})
		begin(app, 1)
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		app.Commit()
		must(t, app.LastBlockHeight() == 1, "commit precedes output")
		must(t, app.RecorderError() != nil, "writer failure latched")
		must(t, !bytes.Contains([]byte(app.RecorderError().Error()), []byte("sensitive")), "public error scrubbed")
	}
}
func TestStateHashAndGasEquivalentToPinnedGaia(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		app, _, key, raw := fixture(t, false)
		original := upstream.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0)
		original.InitChain(abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"})
		var out bytes.Buffer
		if enabled {
			app.SetRecorder(&out)
		}
		req := abci.RequestBeginBlock{Header: abci.Header{Height: 1, ChainID: "observe-test", Time: time.Unix(1500000001, 0)}}
		app.BeginBlock(req)
		original.BeginBlock(req)
		payer := sdk.AccAddress(key.PubKey().Address())
		receiver := sdk.AccAddress(bytes.Repeat([]byte{5}, 20))
		for i := 0; i < 2; i++ {
			msgs := []sdk.Msg{bank.NewMsgSend(payer, receiver, coins(100))}
			if i == 1 {
				msgs = append(msgs, bank.NewMsgSend(payer, receiver, coins(20000000)))
			}
			tx := signed(t, app, key, uint64(i), msgs...)
			a := app.DeliverTx(tx)
			b := original.DeliverTx(tx)
			if a.Code != b.Code || a.GasUsed != b.GasUsed {
				t.Logf("enabled %t codes %d %d gas %d %d", enabled, a.Code, b.Code, a.GasUsed, b.GasUsed)
			}
			must(t, a.Code == b.Code && a.GasUsed == b.GasUsed && a.GasWanted == b.GasWanted && bytes.Equal(a.Data, b.Data), "transaction result and gas equivalence")
		}
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		original.EndBlock(abci.RequestEndBlock{Height: 1})
		must(t, app.RecorderError() == nil, "hash fixture accounting")
		must(t, bytes.Equal(app.Commit().Data, original.Commit().Data), "pinned state hash equivalence")
	}
}
func TestActualZeroHeightExportLabelsAndCacheIsolation(t *testing.T) {
	app, db, key, _ := fixture(t, true)
	begin(app, 1)
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	ctx := app.NewContext(false, abci.Header{Height: 1})
	owner := sdk.AccAddress(key.PubKey().Address())
	val := sdk.ValAddress(owner)
	destination := sdk.AccAddress(bytes.Repeat([]byte{6}, 20))
	// Seed internally consistent distribution state, not inferred wallet income.
	app.distrKeeper.SetDelegatorWithdrawAddr(ctx, owner, destination)
	app.distrKeeper.SetValidatorAccumulatedCommission(ctx, val, sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(3))})
	current := app.distrKeeper.GetValidatorCurrentRewards(ctx, val)
	current.Rewards = sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(5))}
	app.distrKeeper.SetValidatorCurrentRewards(ctx, val, current)
	app.distrKeeper.SetValidatorOutstandingRewards(ctx, val, sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(8))})
	pool := app.stakingKeeper.GetPool(ctx)
	pool.NotBondedTokens = pool.NotBondedTokens.AddRaw(8)
	app.stakingKeeper.SetPool(ctx, pool)
	app.Commit()
	exported := NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	var out bytes.Buffer
	exported.SetRecorder(&out)
	before := exported.LastCommitID()
	raw, vals, err := exported.ExportObserved()
	if err != nil || len(vals) != 1 {
		t.Logf("export error %t, validator count %d", err != nil, len(vals))
	}
	must(t, err == nil && len(raw) > 0 && len(vals) == 1, "actual zero-height export succeeds")
	must(t, exported.LastCommitID().Version == before.Version && bytes.Equal(exported.LastCommitID().Hash, before.Hash), "export never commits")
	var commission, reward, completed bool
	for _, r := range rows(t, &out) {
		must(t, r.Status == "exported", "separate export status")
		if r.Kind == "movement" {
			must(t, r.Address == destination.String() && r.Validator == val.String(), "withdraw address and validator provenance")
			if r.Label == "validator_commission/bank.add" {
				commission = true
			}
			if r.Label == "delegator_reward/bank.add" {
				reward = true
				must(t, r.Delegator == owner.String(), "delegator provenance")
			}
		}
		if r.Kind == "export_completed" {
			completed = true
		}
	}
	must(t, commission && reward && completed, "both actual payout labels and completion")
	reopened := NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	must(t, reopened.bankKeeper.GetCoins(quiet(reopened.NewContext(true, abci.Header{})), destination).IsZero(), "export payouts never reach persistent state")
	// Compare full zero-height artifact with the untouched pinned implementation.
	original := upstream.NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
	expected, _, e := original.ExportAppStateAndValidators(true, nil)
	must(t, e == nil && bytes.Equal(raw, expected), "pinned zero-height export equivalence")
}

func TestPostWriteBlockGasFailureStopsWithoutMislabeling(t *testing.T) {
	app, _, key, _ := fixture(t, false, 25000)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	payer := sdk.AccAddress(key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{7}, 20))
	res := app.DeliverTx(signed(t, app, key, 0, bank.NewMsgSend(payer, receiver, coins(100))))
	must(t, res.Code != 0, "post-write block gas failure")
	must(t, !app.bankKeeper.GetCoins(quiet(app.NewContext(false, abci.Header{})), receiver).IsZero(), "pinned SDK wrote message cache before deferred gas failure")
	must(t, app.RecorderError() != nil, "ambiguous cache success must fail closed")
	app.Commit()
	must(t, out.Len() == 0 && app.LastBlockHeight() == 0, "ambiguous block never commits or emits")
}
func TestZeroPayoutExportCompletionAndWriterFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		app, db, _, _ := fixture(t, false)
		begin(app, 1)
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		app.Commit()
		exported := NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
		var out bytes.Buffer
		if fail {
			exported.SetRecorder(badWriter{})
		} else {
			exported.SetRecorder(&out)
		}
		raw, _, err := exported.ExportObserved()
		if fail {
			must(t, err != nil && raw == nil && exported.RecorderError() != nil, "export write error fails closed")
			continue
		}
		must(t, err == nil, "empty payout export succeeds")
		completed := false
		for _, r := range rows(t, &out) {
			must(t, r.Kind != "movement", "empty export has no invented payouts")
			if r.Kind == "export_completed" {
				completed = true
			}
		}
		must(t, completed, "zero payout completion evidence")
	}
}
func TestUnscopedBlockWriteDetectedBeforeCommit(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	ctx := app.NewContext(false, abci.Header{})
	a := app.accountKeeper.GetAccount(ctx, sdk.AccAddress(key.PubKey().Address()))
	must(t, a.SetCoins(coins(1)) == nil, "fixture mutation")
	app.accountKeeper.SetAccount(ctx, a)
	app.Commit()
	must(t, app.RecorderError() != nil && out.Len() == 0 && app.LastBlockHeight() == 0, "whole block reconciliation catches unscoped mutation")
}
