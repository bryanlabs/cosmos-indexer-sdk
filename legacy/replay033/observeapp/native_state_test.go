package observeapp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
)

// Failures deliberately contain no native addresses, balances or raw bytes.
func nativeRequire(t *testing.T, ok bool, reason string) {
	t.Helper()
	if !ok {
		t.Fatal(reason)
	}
}

func nativeFixture(t *testing.T) (*GaiaApp, sdk.Context, sdk.AccAddress, sdk.ValAddress, sdk.ValAddress) {
	t.Helper()
	app := NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true)
	state := NewDefaultGenesisState()
	del := sdk.AccAddress(bytes.Repeat([]byte{91}, 20))
	state.Accounts = []GenesisAccount{{Address: del, Coins: sdk.Coins{sdk.NewInt64Coin("stake", 10000000)}}}
	state.StakingData.Pool.NotBondedTokens = sdk.NewInt(10000000)
	var addresses []sdk.ValAddress
	for i := 0; i < 2; i++ {
		key := secp256k1.GenPrivKeySecp256k1([]byte{byte(80 + i)})
		addr := sdk.ValAddress(key.PubKey().Address())
		v := staking.NewValidator(addr, key.PubKey(), staking.Description{})
		v.Tokens, v.DelegatorShares = sdk.NewInt(1000000), sdk.NewDec(3000000)
		if i == 1 {
			v.Tokens, v.DelegatorShares = sdk.NewInt(2000000), sdk.NewDec(1000000)
		}
		state.StakingData.Validators = append(state.StakingData.Validators, v)
		state.StakingData.Delegations = append(state.StakingData.Delegations, staking.Delegation{DelegatorAddress: del, ValidatorAddress: addr, Shares: v.DelegatorShares})
		state.StakingData.Pool.NotBondedTokens = state.StakingData.Pool.NotBondedTokens.Add(v.Tokens)
		addresses = append(addresses, addr)
	}
	raw, err := app.cdc.MarshalJSON(state)
	nativeRequire(t, err == nil, "encode native fixture")
	app.InitChain(abci.RequestInitChain{AppStateBytes: raw, ChainId: "native-observe-test"})
	ctx := app.NewContext(false, abci.Header{Height: 1, ChainID: "native-observe-test", Time: time.Unix(1500000001, 0)})
	app.observer.height, app.observer.phase = 1, "message"
	return app, ctx, del, addresses[0], addresses[1]
}

func TestNativeFenceLosslessAllStoresAndGas(t *testing.T) {
	app, ctx, _, _, _ := nativeFixture(t)
	// Invalid-for-decoding bytes and array order must survive without guessing
	// a schema. Raw keeper indexes can legitimately have empty values.
	stores := app.nativeStores()
	for _, s := range stores {
		store := quiet(ctx).KVStore(s.key)
		store.Set([]byte{0xfa, 1}, []byte{0xff, 0, 0xfe})
		store.Set([]byte{0xfa, 2}, []byte{})
		store.Set([]byte{0xfa, 3}, []byte(`[3,1,2]`))
	}
	gas, block := sdk.NewGasMeter(1), sdk.NewGasMeter(1)
	ctx = ctx.WithGasMeter(gas).WithBlockGasMeter(block)
	closeFence := app.nativeFence(ctx)
	for _, s := range stores {
		store := quiet(ctx).KVStore(s.key)
		store.Delete([]byte{0xfa, 1})
		store.Set([]byte{0xfa, 3}, []byte(`[2,3,1]`))
		store.Set([]byte{0xfa, 4}, []byte{0, 0xff})
	}
	closeFence()
	nativeRequire(t, app.RecorderError() == nil, "raw native fence succeeds")
	nativeRequire(t, gas.GasConsumed() == 0 && block.GasConsumed() == 0, "native snapshot is gas neutral")
	checkpoints, changes := 0, 0
	for _, r := range app.observer.pending {
		nativeRequire(t, (r.Unit == "native_store_bytes" || r.Unit == "native_store_sha256") && r.Phase == "message", "raw units and phase")
		if r.Kind == "native_state_hash" {
			checkpoints++
			var before, after struct {
				SHA256 string `json:"sha256"`
				Keys   int    `json:"keys"`
			}
			nativeRequire(t, json.Unmarshal(r.Before, &before) == nil && json.Unmarshal(r.After, &after) == nil && len(before.SHA256) == 64 && len(after.SHA256) == 64 && before.Keys >= 3 && after.Keys >= 3 && before.SHA256 != after.SHA256, "complete per-store commitments detect raw mutation")
		} else if r.Kind == "native_state" {
			changes++
			switch {
			case strings.HasSuffix(r.Label, "/key:"+base64.StdEncoding.EncodeToString([]byte{0xfa, 1})):
				var b []byte
				nativeRequire(t, string(r.After) == "null" && json.Unmarshal(r.Before, &b) == nil && bytes.Equal(b, []byte{0xff, 0, 0xfe}), "deleted opaque bytes retained")
			case strings.HasSuffix(r.Label, "/key:"+base64.StdEncoding.EncodeToString([]byte{0xfa, 3})):
				var b, a []byte
				nativeRequire(t, json.Unmarshal(r.Before, &b) == nil && json.Unmarshal(r.After, &a) == nil && string(b) == `[3,1,2]` && string(a) == `[2,3,1]`, "array order preserved")
			case strings.HasSuffix(r.Label, "/key:"+base64.StdEncoding.EncodeToString([]byte{0xfa, 4})):
				nativeRequire(t, string(r.Before) == "null" && string(r.After) != "null", "added key evidence")
			default:
				t.Fatal("unexpected raw mutation")
			}
		}
	}
	nativeRequire(t, checkpoints == 9 && changes == 27, "all persistent native stores covered")
	app.observer.pending = nil
	app.nativeFence(ctx)()
	nativeRequire(t, len(app.observer.pending) == 9, "unchanged state still has complete commitments")
	for _, r := range app.observer.pending {
		nativeRequire(t, r.Kind == "native_state_hash" && bytes.Equal(r.Before, r.After), "unchanged exact native commitment")
	}
	app.observer.pending = nil
	app.nativeCheckpoint(ctx, "baseline")
	nativeRequire(t, len(app.observer.pending) == 9, "all complete baseline stores retained")
	for _, r := range app.observer.pending {
		var values []nativeKV
		nativeRequire(t, r.Kind == "native_state_checkpoint" && json.Unmarshal(r.Before, &values) == nil && len(values) >= 3, "complete native baseline values")
		for i := 1; i < len(values); i++ {
			nativeRequire(t, bytes.Compare(values[i-1].Key, values[i].Key) < 0, "raw iterator order preserved")
		}
		for _, kv := range values {
			if bytes.Equal(kv.Key, []byte{0xfa, 2}) {
				nativeRequire(t, kv.Value != nil && len(kv.Value) == 0, "present empty is not null")
			}
		}
	}
}

func TestNativePrincipalSharesRoundingAndRedelegation(t *testing.T) {
	for _, redelegate := range []bool{false, true} {
		name := "undelegate"
		if redelegate {
			name = "redelegate"
		}
		t.Run(name, func(t *testing.T) {
			app, ctx, del, src, dst := nativeFixture(t)
			requested := sdk.NewDecWithPrec(105, 1)
			var msg sdk.Msg = staking.NewMsgUndelegate(del, src, requested)
			if redelegate {
				msg = staking.NewMsgBeginRedelegate(del, src, dst, requested)
			}
			beforeSrc := app.nativeStake(ctx, del, src)
			beforeDst := app.nativeStake(ctx, del, dst)
			gas, block := sdk.NewGasMeter(1), sdk.NewGasMeter(1)
			observed := ctx.WithGasMeter(gas).WithBlockGasMeter(block)
			fence, done := app.nativeFence(observed), app.principal(observed, msg)
			nativeRequire(t, gas.GasConsumed() == 0 && block.GasConsumed() == 0, "principal before reads gas neutral")
			res := staking.NewHandler(app.stakingKeeper)(quiet(ctx), msg)
			nativeRequire(t, res.IsOK(), "native shares handler succeeds")
			done(true)
			fence()
			nativeRequire(t, gas.GasConsumed() == 0 && block.GasConsumed() == 0, "principal after reads gas neutral")
			nativeRequire(t, app.RecorderError() == nil, "native principal observer succeeds")
			afterSrc := app.nativeStake(ctx, del, src)
			afterDst := app.nativeStake(ctx, del, dst)
			principal, shares, requests := 0, 0, 0
			for _, r := range app.observer.pending {
				switch r.Kind {
				case "share_request":
					requests++
					nativeRequire(t, r.Amount == requested.String() && r.Unit == "delegation_shares" && r.Denom == "", "native decimal request retained")
				case "principal":
					principal++
					debit := beforeSrc.Validator.Tokens.Sub(afterSrc.Validator.Tokens)
					nativeRequire(t, debit.Equal(sdk.NewInt(3)) && r.Amount == debit.String() && r.Unit == "native_bond_tokens", "actual native debit not requested shares or rounded token guess")
					nativeRequire(t, len(r.Before) > 0 && len(r.After) > 0 && r.Phase == "message", "native typed principal states")
				case "staking_shares":
					shares++
					nativeRequire(t, r.Unit == "delegation_shares" && r.Denom == "", "shares never labeled tokens")
					if strings.HasSuffix(r.Label, ":source") {
						nativeRequire(t, r.Amount == requested.Neg().String(), "actual native source share debit")
					} else {
						actual := nativeDelShares(afterDst).Sub(nativeDelShares(beforeDst))
						nativeRequire(t, actual.Equal(sdk.NewDecWithPrec(15, 1)) && r.Amount == actual.String(), "native destination uses its own exchange rate")
					}
				}
			}
			wantShares := 1
			if redelegate {
				wantShares = 2
				red, found := app.stakingKeeper.GetRedelegation(quiet(ctx), del, src, dst)
				nativeRequire(t, found && len(red.Entries) == 1, "native redelegation entry exists")
			} else {
				ubd, found := app.stakingKeeper.GetUnbondingDelegation(quiet(ctx), del, src)
				nativeRequire(t, found && len(ubd.Entries) == 1, "native unbonding entry exists")
			}
			nativeRequire(t, principal == 1 && requests == 1 && shares == wantShares, "complete native principal and shares evidence")
		})
	}
}

func TestNativeFailedAttemptKeepsRawEvidenceNotPrincipal(t *testing.T) {
	app, ctx, del, src, _ := nativeFixture(t)
	cache, _ := ctx.CacheContext()
	// Zero destination is not used: a well-formed unknown destination lets
	// native unbond mutate its cache before native redelegation fails.
	dst := sdk.ValAddress(bytes.Repeat([]byte{77}, 20))
	msg := staking.NewMsgBeginRedelegate(del, src, dst, sdk.NewDec(12))
	fence, done := app.nativeFence(cache), app.principal(cache, msg)
	res := staking.NewHandler(app.stakingKeeper)(cache, msg)
	nativeRequire(t, !res.IsOK(), "native handler rejects unknown destination")
	done(false)
	fence()
	nativeRequire(t, app.RecorderError() == nil, "expected failed native operation is evidence not observer failure")
	changes, request := 0, false
	for _, r := range app.observer.pending {
		nativeRequire(t, r.Kind != "principal" && r.Kind != "staking_shares", "failed native operation never emits principal")
		if r.Kind == "native_state" {
			changes++
		}
		request = request || r.Kind == "share_request"
	}
	nativeRequire(t, changes > 0 && request, "failed native partial changes and requested shares retained")
	outer := app.nativeStake(ctx, del, src)
	nativeRequire(t, outer.Validator.Tokens.Equal(sdk.NewInt(1000000)) && nativeDelShares(outer).Equal(sdk.NewDec(3000000)), "discarded cache has no committed debit")
}

func TestNativeLastShareDeletionUsesActualNativeTokens(t *testing.T) {
	app, ctx, del, src, _ := nativeFixture(t)
	v, found := app.stakingKeeper.GetValidator(quiet(ctx), src)
	nativeRequire(t, found, "native source exists")
	// Move this synthetic source to the native immediately-unbondable state.
	pool := app.stakingKeeper.GetPool(quiet(ctx))
	pool.BondedTokens = pool.BondedTokens.Sub(v.Tokens)
	pool.NotBondedTokens = pool.NotBondedTokens.Add(v.Tokens)
	app.stakingKeeper.SetPool(quiet(ctx), pool)
	v.Status = sdk.Unbonded
	app.stakingKeeper.SetValidator(quiet(ctx), v)
	msg := staking.NewMsgUndelegate(del, src, v.DelegatorShares)
	fence, done := app.nativeFence(ctx), app.principal(ctx, msg)
	res := staking.NewHandler(app.stakingKeeper)(quiet(ctx), msg)
	nativeRequire(t, res.IsOK(), "native complete unbond succeeds")
	done(true)
	fence()
	nativeRequire(t, app.RecorderError() == nil, "native last-share observation succeeds")
	after := app.nativeStake(ctx, del, src)
	nativeRequire(t, after.Validator == nil && after.Delegation == nil, "native keeper deleted zero-share source")
	principal, deleted := false, 0
	for _, r := range app.observer.pending {
		if r.Kind == "principal" {
			var state nativeStakeState
			nativeRequire(t, r.Amount == v.Tokens.String() && json.Unmarshal(r.After, &state) == nil && state.Validator == nil && state.Delegation == nil, "last share gets all native tokens with explicit absent after state")
			principal = true
		}
		if r.Kind == "native_state" && string(r.After) == "null" {
			deleted++
		}
	}
	nativeRequire(t, principal && deleted > 1, "native deletions are independently evidenced")
}

func TestNativeUnbondingEntryArrayBytesRemainOrdered(t *testing.T) {
	app, ctx, del, src, _ := nativeFixture(t)
	for _, requested := range []sdk.Dec{sdk.NewDec(10), sdk.NewDec(15)} {
		msg := staking.NewMsgUndelegate(del, src, requested)
		fence, done := app.nativeFence(ctx), app.principal(ctx, msg)
		res := staking.NewHandler(app.stakingKeeper)(quiet(ctx), msg)
		nativeRequire(t, res.IsOK(), "native successive unbond succeeds")
		done(true)
		fence()
	}
	ubd, found := app.stakingKeeper.GetUnbondingDelegation(quiet(ctx), del, src)
	nativeRequire(t, found && len(ubd.Entries) == 2, "native repeated unbond keeps both entries")
	nativeRequire(t, ubd.Entries[0].Balance.Equal(sdk.NewInt(3)) && ubd.Entries[1].Balance.Equal(sdk.NewInt(5)), "native entry order and truncation")
	key := staking.GetUBDKey(del, src)
	raw := quiet(ctx).KVStore(app.keyStaking).Get(key)
	evidenced := false
	for _, r := range app.observer.pending {
		if r.Kind != "native_state" || !strings.HasSuffix(r.Label, "/key:"+base64.StdEncoding.EncodeToString(key)) {
			continue
		}
		var after []byte
		nativeRequire(t, json.Unmarshal(r.After, &after) == nil, "decode native array-valued object")
		if bytes.Equal(after, raw) {
			evidenced = true
		}
	}
	nativeRequire(t, evidenced && app.RecorderError() == nil, "complete native array bytes evidenced without reordering")
}

func TestNativeInvalidAndUnknownFailClosed(t *testing.T) {
	for _, mode := range []string{"negative_request", "nil_request", "unknown_success", "negative_state", "inconsistent_success"} {
		t.Run(mode, func(t *testing.T) {
			app, ctx, del, src, _ := nativeFixture(t)
			request := sdk.NewDec(1)
			switch mode {
			case "negative_request":
				request = sdk.NewDec(-1)
			case "nil_request":
				request = sdk.Dec{}
			case "unknown_success":
				src = sdk.ValAddress(bytes.Repeat([]byte{76}, 20))
			case "negative_state":
				v, _ := app.stakingKeeper.GetValidator(quiet(ctx), src)
				v.Tokens = sdk.NewInt(-1)
				app.stakingKeeper.SetValidator(quiet(ctx), v)
			}
			app.principal(ctx, staking.NewMsgUndelegate(del, src, request))(true)
			nativeRequire(t, app.RecorderError() != nil && app.RecorderError().Error() == "private replay accounting failed; stop", "invalid native evidence fails closed with fixed error")
			for _, r := range app.observer.pending {
				nativeRequire(t, r.Kind != "principal", "invalid native observation cannot emit principal")
			}
		})
	}
}
