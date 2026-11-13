package observeapp

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

// Compare synthetic evidence without printing accounts, balances, or SDK errors.
type sparsePair struct {
	full, sparse       *GaiaApp
	fullOut, sparseOut bytes.Buffer
	key                secp256k1.PrivKeySecp256k1
}

func newSparsePair(t *testing.T, validator bool, maxGas ...int64) *sparsePair {
	t.Helper()
	full, _, key, _ := fixture(t, validator, maxGas...)
	sparse, _, _, _ := fixture(t, validator, maxGas...)
	sparse.SetSparseAccounting(true)
	return &sparsePair{full: full, sparse: sparse, key: key}
}
func (p *sparsePair) record() {
	p.full.SetRecorder(&p.fullOut)
	p.sparse.SetRecorder(&p.sparseOut)
}
func (p *sparsePair) begin(t *testing.T, h int64) {
	t.Helper()
	req := abci.RequestBeginBlock{Header: abci.Header{Height: h, ChainID: "observe-test", Time: time.Unix(1500000000+h, 0)}}
	a, b := p.full.BeginBlock(req), p.sparse.BeginBlock(req)
	must(t, reflect.DeepEqual(a, b), "full and sparse begin responses match exactly")
}
func (p *sparsePair) deliver(t *testing.T, raw []byte) abci.ResponseDeliverTx {
	t.Helper()
	a, b := p.full.DeliverTx(raw), p.sparse.DeliverTx(raw)
	must(t, reflect.DeepEqual(a, b), "full and sparse transaction responses match exactly")
	must(t, (p.full.RecorderError() == nil) == (p.sparse.RecorderError() == nil), "full and sparse transaction failure latches match")
	return a
}
func (p *sparsePair) end(t *testing.T, h int64) {
	t.Helper()
	a, b := p.full.EndBlock(abci.RequestEndBlock{Height: h}), p.sparse.EndBlock(abci.RequestEndBlock{Height: h})
	must(t, reflect.DeepEqual(a, b), "full and sparse end responses match exactly")
}
func (p *sparsePair) commit(t *testing.T, wantError bool) {
	t.Helper()
	a, b := p.full.Commit(), p.sparse.Commit()
	must(t, reflect.DeepEqual(a, b), "full and sparse commit responses match exactly")
	must(t, reflect.DeepEqual(p.full.LastCommitID(), p.sparse.LastCommitID()), "full and sparse committed roots match exactly")
	must(t, (p.full.RecorderError() != nil) == wantError && (p.sparse.RecorderError() != nil) == wantError, "full and sparse accounting outcomes match expectation")
	must(t, bytes.Equal(p.fullOut.Bytes(), p.sparseOut.Bytes()), "full and sparse serialized records match exactly")
	must(t, reflect.DeepEqual(rows(t, &p.fullOut), rows(t, &p.sparseOut)), "full and sparse decoded records match exactly")
	if wantError {
		must(t, p.fullOut.Len() == 0, "failed accounting emits no records")
	}
}

func TestSparseGenesisBaselineBeforeFirstCommit(t *testing.T) {
	_, _, key, raw := fixture(t, false)
	p := &sparsePair{
		full:   NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0),
		sparse: NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true, 0),
		key:    key,
	}
	p.sparse.SetSparseAccounting(true)
	req := abci.RequestInitChain{AppStateBytes: raw, ChainId: "observe-test"}
	a, b := p.full.InitChain(req), p.sparse.InitChain(req)
	must(t, reflect.DeepEqual(a, b), "sparse genesis response matches full reference")
	must(t, p.sparse.LastBlockHeight() == 0, "genesis baseline is not committed")
	p.record()
	p.begin(t, 1)
	owner := sdk.AccAddress(key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{38}, 20))
	must(t, p.deliver(t, signed(t, p.full, key, 0, bank.NewMsgSend(owner, receiver, coins(1)))).Code == 0, "first observed transaction uses uncommitted genesis baseline")
	p.end(t, 1)
	p.commit(t, false)
	must(t, p.sparse.observer.fullSnapshots == 0 && p.sparse.observer.sparseDecoded > 0, "genesis baseline stays sparse")
}

func TestSparseDifferentialTransactionsAndEmptyBlocks(t *testing.T) {
	p := newSparsePair(t, false)
	p.record()
	p.begin(t, 1)
	payer := sdk.AccAddress(p.key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{31}, 20))
	must(t, p.deliver(t, signed(t, p.full, p.key, 0, bank.NewMsgSend(payer, receiver, coins(100)))).Code == 0, "successful synthetic transfer")
	must(t, p.deliver(t, signed(t, p.full, p.key, 1, bank.NewMsgSend(payer, receiver, coins(25)), bank.NewMsgSend(payer, receiver, coins(20000000)))).Code != 0, "later message rolls back")
	must(t, p.deliver(t, signed(t, p.full, p.key, 99, bank.NewMsgSend(payer, receiver, coins(1)))).Code != 0, "failed ante sequence")
	must(t, p.deliver(t, []byte{255}).Code != 0, "undecodable transaction rejection")
	must(t, p.deliver(t, signed(t, p.full, p.key, 2, bank.NewMsgSend(payer, receiver, sdk.Coins{}))).Code != 0, "pre-ante message validation rejection")
	must(t, p.deliver(t, signed(t, p.full, p.key, 2, bank.NewMsgSend(payer, receiver, coins(1)))).Code == 0, "execution continues after discarded caches")
	p.end(t, 1)
	p.commit(t, false)
	p.begin(t, 2)
	p.end(t, 2)
	p.commit(t, false)
	var committed, aborted, completed bool
	for _, r := range rows(t, &p.sparseOut) {
		committed = committed || r.Kind == "movement" && r.Status == "committed"
		aborted = aborted || r.Kind == "movement" && r.Status == "aborted"
		completed = completed || r.Kind == "height_completed" && r.Height == 2
	}
	must(t, committed && aborted && completed, "non-vacuous committed, aborted, and empty block evidence")
}

func TestSparseDifferentialAntePanicAndLateGasFailure(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "ante_panic", true: "late_block_gas"}[late], func(t *testing.T) {
			var p *sparsePair
			if late {
				p = newSparsePair(t, false, 25000)
			} else {
				p = newSparsePair(t, false)
			}
			p.record()
			p.begin(t, 1)
			payer := sdk.AccAddress(p.key.PubKey().Address())
			receiver := sdk.AccAddress(bytes.Repeat([]byte{32}, 20))
			msg := bank.NewMsgSend(payer, receiver, coins(100))
			raw := signed(t, p.full, p.key, 0, msg)
			if !late {
				fee := auth.NewStdFee(1, coins(7))
				sig, err := p.key.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, []sdk.Msg{msg}, ""))
				must(t, err == nil, "sign synthetic ante panic")
				raw, err = p.full.cdc.MarshalBinaryLengthPrefixed(auth.NewStdTx([]sdk.Msg{msg}, fee, []auth.StdSignature{{PubKey: p.key.PubKey(), Signature: sig}}, ""))
				must(t, err == nil, "encode synthetic ante panic")
			}
			must(t, p.deliver(t, raw).Code != 0, "synthetic gas failure")
			if !late {
				p.end(t, 1)
			}
			p.commit(t, late)
			if late {
				must(t, p.sparse.LastBlockHeight() == 0, "late ambiguous failure cannot commit")
			}
		})
	}
}

func TestSparseDifferentialStakingAndZeroHeightExport(t *testing.T) {
	p := newSparsePair(t, true)
	// Seed genuine payout state before observation so exports exercise nested
	// distribution bank writes, not just an empty export completion marker.
	p.begin(t, 1)
	p.end(t, 1)
	owner := sdk.AccAddress(p.key.PubKey().Address())
	validator := sdk.ValAddress(owner)
	destination := sdk.AccAddress(bytes.Repeat([]byte{33}, 20))
	for _, app := range []*GaiaApp{p.full, p.sparse} {
		ctx := app.NewContext(false, abci.Header{Height: 1})
		app.distrKeeper.SetDelegatorWithdrawAddr(ctx, owner, destination)
		app.distrKeeper.SetValidatorAccumulatedCommission(ctx, validator, sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(3))})
		current := app.distrKeeper.GetValidatorCurrentRewards(ctx, validator)
		current.Rewards = sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(5))}
		app.distrKeeper.SetValidatorCurrentRewards(ctx, validator, current)
		app.distrKeeper.SetValidatorOutstandingRewards(ctx, validator, sdk.DecCoins{sdk.NewDecCoin("stake", sdk.NewInt(8))})
		pool := app.stakingKeeper.GetPool(ctx)
		pool.NotBondedTokens = pool.NotBondedTokens.AddRaw(8)
		app.stakingKeeper.SetPool(ctx, pool)
		params := app.stakingKeeper.GetParams(ctx)
		params.UnbondingTime = 0
		app.stakingKeeper.SetParams(ctx, params)
	}
	p.commit(t, false)
	p.record()
	p.begin(t, 2)
	for i, msg := range []sdk.Msg{staking.NewMsgDelegate(owner, validator, coins(10)[0]), staking.NewMsgUndelegate(owner, validator, coins(4)[0])} {
		must(t, p.deliver(t, signed(t, p.full, p.key, uint64(i), msg)).Code == 0, "synthetic staking operation succeeds")
	}
	p.end(t, 2)
	p.commit(t, false)
	before := p.sparse.LastCommitID()
	a, av, ae := p.full.ExportObserved()
	b, bv, be := p.sparse.ExportObserved()
	must(t, ae == nil && be == nil && len(a) > 0 && len(av) > 0, "observed validator export succeeds")
	must(t, bytes.Equal(a, b) && reflect.DeepEqual(av, bv), "full and sparse entire zero-height export matches")
	must(t, bytes.Equal(p.fullOut.Bytes(), p.sparseOut.Bytes()), "full and sparse export records match exactly")
	must(t, reflect.DeepEqual(before, p.sparse.LastCommitID()) && reflect.DeepEqual(p.full.LastCommitID(), p.sparse.LastCommitID()), "export leaves persistent roots untouched")
	commission, reward, principal, mature, completion := false, false, false, false, false
	for _, r := range rows(t, &p.sparseOut) {
		commission = commission || r.Label == "validator_commission/bank.add" && r.Status == "exported"
		if r.Kind == "movement" && r.Label == "distribution/delegation_reward/bank.add" {
			must(t, r.Validator == validator.String() && r.Delegator == owner.String() && r.Address == destination.String(), "auto-reward identities and withdraw destination preserved")
			reward = true
		}
		if r.Kind == "principal" {
			must(t, r.Label == "undelegate" && r.Validator == validator.String() && r.Delegator == owner.String() && r.Amount == "4" && r.Status == "committed", "actual undelegated principal identity and amount preserved")
			principal = true
		}
		if r.Kind == "movement" && r.Label == "staking/unbonding_complete/bank.undelegate" {
			must(t, r.Validator == validator.String() && r.Delegator == owner.String() && r.Address == owner.String() && r.Amount == "4", "mature unbonding payout identity and amount preserved")
			mature = true
		}
		completion = completion || r.Kind == "export_completed"
	}
	must(t, commission && reward && principal && mature && completion, "payout, principal, mature unbonding, and export evidence exercised")
}

func TestSparseDifferentialGovernanceBurn(t *testing.T) {
	p := newSparsePair(t, false)
	p.begin(t, 1)
	owner := sdk.AccAddress(p.key.PubKey().Address())
	var expiry time.Time
	for _, app := range []*GaiaApp{p.full, p.sparse} {
		ctx := app.NewContext(false, abci.Header{Height: 1, Time: time.Unix(1500000001, 0)})
		proposal, err := app.govKeeper.SubmitProposal(ctx, gov.NewTextProposal("synthetic", "synthetic"))
		must(t, err == nil, "create synthetic governance proposal")
		err, _ = app.govKeeper.AddDeposit(ctx, proposal.ProposalID, owner, coins(12))
		must(t, err == nil, "create synthetic governance deposit")
		expiry = proposal.DepositEndTime.Add(time.Second)
	}
	p.end(t, 1)
	p.commit(t, false)
	p.record()
	req := abci.RequestBeginBlock{Header: abci.Header{Height: 2, ChainID: "observe-test", Time: expiry}}
	a, b := p.full.BeginBlock(req), p.sparse.BeginBlock(req)
	must(t, reflect.DeepEqual(a, b), "governance expiry begin responses match")
	p.end(t, 2)
	p.commit(t, false)
	count := 0
	for _, r := range rows(t, &p.sparseOut) {
		if r.Kind == "movement" && r.Label == "gov/deposit_burn/bank.send" {
			must(t, r.Delegator == owner.String() && r.Status == "committed", "governance burn retains original depositor")
			count++
		}
	}
	must(t, count == 2, "governance burn debit and credit exercised")
}

func TestSparseDifferentialZeroNetLateGasFailure(t *testing.T) {
	p := newSparsePair(t, false, 1)
	second := secp256k1.GenPrivKeySecp256k1([]byte("second sparse synthetic signer"))
	a, b := sdk.AccAddress(p.key.PubKey().Address()), sdk.AccAddress(second.PubKey().Address())
	var number uint64
	for _, app := range []*GaiaApp{p.full, p.sparse} {
		ctx := app.NewContext(false, abci.Header{})
		account := app.accountKeeper.NewAccountWithAddress(ctx, b)
		number = account.GetAccountNumber()
		app.accountKeeper.SetAccount(ctx, account)
	}
	msgs := []sdk.Msg{bank.NewMsgSend(a, b, coins(100)), bank.NewMsgSend(b, a, coins(100))}
	fee := auth.NewStdFee(200000, coins(7))
	one, err := p.key.Sign(auth.StdSignBytes("observe-test", 0, 0, fee, msgs, ""))
	must(t, err == nil, "sign first synthetic zero-net signer")
	two, err := second.Sign(auth.StdSignBytes("observe-test", number, 0, fee, msgs, ""))
	must(t, err == nil, "sign second synthetic zero-net signer")
	raw, err := p.full.cdc.MarshalBinaryLengthPrefixed(auth.NewStdTx(msgs, fee, []auth.StdSignature{{PubKey: p.key.PubKey(), Signature: one}, {PubKey: second.PubKey(), Signature: two}}, ""))
	must(t, err == nil, "encode synthetic zero-net transaction")
	p.record()
	p.begin(t, 1)
	must(t, p.deliver(t, raw).Code != 0, "zero-net transaction fails after cache write")
	for _, app := range []*GaiaApp{p.full, p.sparse} {
		must(t, app.observer.handlers == 2 && !app.observer.handlerFailure, "both zero-net handlers succeed before late failure")
	}
	p.commit(t, true)
	must(t, p.sparse.LastBlockHeight() == 0, "zero-net late failure cannot commit")
}

func sparseAccountKey(address sdk.AccAddress) []byte {
	return append([]byte{1}, address...)
}

func TestSparseDifferentialDirectAndRawWrites(t *testing.T) {
	for _, kind := range []string{"direct", "raw", "create", "delete", "create_zero", "delete_zero", "metadata", "phantom", "phantom_net_zero", "unscoped"} {
		t.Run(kind, func(t *testing.T) {
			p := newSparsePair(t, false)
			owner := sdk.AccAddress(p.key.PubKey().Address())
			other := sdk.AccAddress(bytes.Repeat([]byte{34}, 20))
			if kind == "delete_zero" {
				for _, app := range []*GaiaApp{p.full, p.sparse} {
					ctx := app.NewContext(false, abci.Header{})
					app.accountKeeper.SetAccount(ctx, app.accountKeeper.NewAccountWithAddress(ctx, other))
				}
			}
			p.record()
			p.begin(t, 1)
			p.end(t, 1)
			for _, app := range []*GaiaApp{p.full, p.sparse} {
				ctx := app.NewContext(false, abci.Header{})
				mutate := func() {
					store := ctx.MultiStore().GetKVStore(app.keyAccount)
					switch kind {
					case "direct", "unscoped":
						a := app.accountKeeper.GetAccount(ctx, owner)
						must(t, a.SetCoins(coins(1)) == nil, "direct synthetic balance mutation")
						app.accountKeeper.SetAccount(ctx, a)
					case "raw":
						a := app.accountKeeper.GetAccount(ctx, owner)
						must(t, a.SetCoins(coins(2)) == nil, "raw synthetic balance mutation")
						raw, err := app.cdc.MarshalBinaryBare(a)
						must(t, err == nil, "encode raw synthetic account")
						store.Set(sparseAccountKey(owner), raw)
					case "create", "create_zero":
						a := app.accountKeeper.NewAccountWithAddress(ctx, other)
						if kind == "create" {
							must(t, a.SetCoins(coins(3)) == nil, "fund synthetic new account")
						}
						app.accountKeeper.SetAccount(ctx, a)
					case "delete":
						store.Delete(sparseAccountKey(owner))
					case "delete_zero":
						store.Delete(sparseAccountKey(other))
					case "metadata":
						a := app.accountKeeper.GetAccount(ctx, owner)
						must(t, a.SetSequence(a.GetSequence()+1) == nil, "synthetic metadata mutation")
						app.accountKeeper.SetAccount(ctx, a)
					case "phantom", "phantom_net_zero":
						app.observer.pending = append(app.observer.pending, Record{Kind: "movement", Address: other.String(), Denom: "stake", Amount: "9"})
						if kind == "phantom_net_zero" {
							app.observer.pending = append(app.observer.pending, Record{Kind: "movement", Address: other.String(), Denom: "stake", Amount: "-9"})
						}
					}
				}
				if kind == "unscoped" {
					mutate()
				} else {
					app.phase(ctx, "synthetic_raw", mutate)
				}
			}
			wantError := kind != "create_zero" && kind != "delete_zero" && kind != "metadata" && kind != "phantom_net_zero"
			p.commit(t, wantError)
			if wantError {
				must(t, p.sparse.LastBlockHeight() == 0, "unexplained account movement cannot commit")
			}
		})
	}
}

func TestSparseMalformedAccountEvidenceFailsClosed(t *testing.T) {
	for _, kind := range []string{"bytes", "empty", "key_length", "address_mismatch", "record_address"} {
		t.Run(kind, func(t *testing.T) {
			app, _, key, _ := fixture(t, false)
			app.SetSparseAccounting(true)
			var out bytes.Buffer
			app.SetRecorder(&out)
			begin(app, 1)
			app.EndBlock(abci.RequestEndBlock{Height: 1})
			ctx := app.NewContext(false, abci.Header{})
			owner := sdk.AccAddress(key.PubKey().Address())
			app.phase(ctx, "synthetic_malformed", func() {
				store := ctx.MultiStore().GetKVStore(app.keyAccount)
				switch kind {
				case "bytes":
					store.Set(sparseAccountKey(owner), []byte{255})
				case "empty":
					store.Set(sparseAccountKey(owner), []byte{})
				case "key_length":
					store.Set([]byte{1, 2}, []byte{255})
				case "address_mismatch":
					a := app.accountKeeper.GetAccount(ctx, owner)
					raw, err := app.cdc.MarshalBinaryBare(a)
					must(t, err == nil, "encode mismatched synthetic account")
					store.Set(sparseAccountKey(sdk.AccAddress(bytes.Repeat([]byte{35}, 20))), raw)
				case "record_address":
					app.observer.pending = append(app.observer.pending, Record{Kind: "movement", Address: "invalid synthetic address", Denom: "stake", Amount: "1"})
				}
			})
			app.Commit()
			must(t, app.RecorderError() != nil && out.Len() == 0 && app.LastBlockHeight() == 0, "malformed sparse evidence fails privately before commit")
		})
	}
}

func TestSparseDisabledThenEnabledTransition(t *testing.T) {
	p := newSparsePair(t, false)
	p.begin(t, 1)
	owner := sdk.AccAddress(p.key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{36}, 20))
	must(t, p.deliver(t, signed(t, p.full, p.key, 0, bank.NewMsgSend(owner, receiver, coins(100)))).Code == 0, "disabled observation transfer succeeds")
	p.end(t, 1)
	p.commit(t, false)
	must(t, p.sparse.observer.sparseDecoded == 0 && p.sparse.observer.fullSnapshots == 0, "disabled observation performs no accounting decode or scan")
	p.record()
	p.begin(t, 2)
	must(t, p.deliver(t, signed(t, p.full, p.key, 1, bank.NewMsgSend(owner, receiver, coins(1)))).Code == 0, "enabled observation sees existing accounts")
	p.end(t, 2)
	p.commit(t, false)
	p.full.SetRecorder(nil)
	p.sparse.SetRecorder(nil)
	p.begin(t, 3)
	p.end(t, 3)
	p.commit(t, false)
	p.record()
	p.begin(t, 4)
	p.end(t, 4)
	p.commit(t, false)
	must(t, p.sparse.observer.fullSnapshots == 0, "sparse transitions never fall back to full snapshots")
}

func TestSparseAccountingModeRoundTripBetweenBlocks(t *testing.T) {
	p := newSparsePair(t, false)
	p.record()
	owner := sdk.AccAddress(p.key.PubKey().Address())
	receiver := sdk.AccAddress(bytes.Repeat([]byte{39}, 20))
	for h := int64(1); h <= 3; h++ {
		p.sparse.SetSparseAccounting(h != 2)
		before := p.sparse.observer.fullSnapshots
		p.begin(t, h)
		must(t, p.deliver(t, signed(t, p.full, p.key, uint64(h-1), bank.NewMsgSend(owner, receiver, coins(1)))).Code == 0, "transfer succeeds across accounting mode transition")
		p.end(t, h)
		p.commit(t, false)
		if h == 2 {
			must(t, p.sparse.observer.fullSnapshots > before, "disabled sparse mode uses full reference")
		} else {
			must(t, p.sparse.observer.fullSnapshots == before, "enabled sparse mode performs no full snapshots")
		}
	}
}

func TestSparseUnknownDisabledPendingFlushFailsClosed(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(map[bool]string{false: "multistore", true: "standalone"}[standalone], func(t *testing.T) {
			app, _, key, _ := fixture(t, false)
			app.SetSparseAccounting(true)
			ctx := app.NewContext(false, abci.Header{})
			owner := sdk.AccAddress(key.PubKey().Address())
			a := app.accountKeeper.GetAccount(ctx, owner)
			must(t, a.SetSequence(7) == nil, "prepare synthetic disabled metadata write")
			raw, err := app.cdc.MarshalBinaryBare(a)
			must(t, err == nil, "encode synthetic disabled account")
			var flush func()
			if standalone {
				cache := ctx.MultiStore().GetKVStore(app.keyAccount).CacheWrap()
				cache.(sdk.KVStore).Set(sparseAccountKey(owner), raw)
				flush = cache.Write
			} else {
				cache := ctx.MultiStore().CacheMultiStore()
				cache.GetKVStore(app.keyAccount).Set(sparseAccountKey(owner), raw)
				flush = cache.Write
			}
			var out bytes.Buffer
			app.SetRecorder(&out)
			begin(app, 1)
			app.phase(app.NewContext(false, abci.Header{}), "synthetic_delayed_flush", flush)
			must(t, app.accountKeeper.GetAccount(app.NewContext(false, abci.Header{}), owner).GetSequence() == 7, "unsafe flush still preserves native execution")
			app.Commit()
			must(t, app.RecorderError() != nil && app.LastBlockHeight() == 0 && out.Len() == 0, "unknown disabled pending flush cannot produce accepted evidence")
		})
	}
}

func TestSparseStaleContextFailsClosedAfterCommit(t *testing.T) {
	app, _, _, _ := fixture(t, false)
	app.SetSparseAccounting(true)
	var out bytes.Buffer
	app.SetRecorder(&out)
	begin(app, 1)
	stale := app.NewContext(false, abci.Header{})
	app.EndBlock(abci.RequestEndBlock{Height: 1})
	app.Commit()
	must(t, app.RecorderError() == nil, "first synthetic block succeeds")
	before := app.LastCommitID()
	out.Reset()
	begin(app, 2)
	app.phase(stale, "synthetic_stale_context", func() {})
	app.Commit()
	must(t, app.RecorderError() != nil && out.Len() == 0 && reflect.DeepEqual(before, app.LastCommitID()), "stale account context cannot reconcile or commit a new block")
}

func TestSparseEmptyBlockWorkDoesNotScaleWithAccounts(t *testing.T) {
	var decoded [2]uint64
	for index, count := range []int{1, 1024} {
		p := newSparsePair(t, false)
		for _, app := range []*GaiaApp{p.full, p.sparse} {
			ctx := app.NewContext(false, abci.Header{})
			for i := 0; i < count; i++ {
				address := bytes.Repeat([]byte{37}, 20)
				binary.BigEndian.PutUint64(address[12:], uint64(i))
				a := app.accountKeeper.NewAccountWithAddress(ctx, sdk.AccAddress(address))
				app.accountKeeper.SetAccount(ctx, a)
			}
		}
		p.record()
		before := p.sparse.observer.sparseDecoded
		for h := int64(1); h <= 3; h++ {
			p.begin(t, h)
			p.end(t, h)
			p.commit(t, false)
		}
		decoded[index] = p.sparse.observer.sparseDecoded - before
		must(t, p.sparse.observer.fullSnapshots == 0, "empty sparse blocks perform no full account snapshots")
		must(t, p.full.observer.fullSnapshots > 0, "default observer remains full snapshot reference")
	}
	must(t, decoded[0] == decoded[1], "empty sparse block decode work is independent of account count")
}
