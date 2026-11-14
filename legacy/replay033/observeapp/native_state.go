package observeapp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replay033/observeapp/nativekvtrack"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
)

// Native bytes are evidence, not token movements or financial acceptance. The
// checkpoints retain every persistent key, including indexes, queues and params.
// Byte slices encode as base64; absent values encode as null, not empty bytes.
type nativeKV struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}
type nativeStore struct {
	name string
	key  *sdk.KVStoreKey
}
type nativeImage struct {
	values []nativeKV
	digest json.RawMessage
}
type nativeSnapshot []*nativeImage
type nativeMemo struct {
	token nativekvtrack.Token
	image *nativeImage
}

func (app *GaiaApp) nativeStores() []nativeStore {
	return []nativeStore{
		{"main", app.keyMain}, {"account", app.keyAccount},
		{"staking", app.keyStaking}, {"slashing", app.keySlashing},
		{"mint", app.keyMint}, {"distribution", app.keyDistr},
		{"governance", app.keyGov}, {"fee_collection", app.keyFeeCollection},
		{"params", app.keyParams},
	}
}

func (app *GaiaApp) nativeRecord(kind, label, unit string) Record {
	o := &app.observer
	return Record{Kind: kind, Label: label, Unit: unit, Height: o.height,
		Tx: o.tx, Message: o.message, Phase: o.phase,
		Validator: o.validator, Delegator: o.delegator}
}

func nativeCopy(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (app *GaiaApp) nativeJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		app.fail()
		return nil
	}
	return b
}

func (app *GaiaApp) nativeSnapshot(ctx sdk.Context, stores []nativeStore) (out nativeSnapshot, ok bool) {
	defer func() {
		if recover() != nil {
			app.fail()
			out, ok = nil, false
		}
	}()
	out = make(nativeSnapshot, len(stores))
	for i, s := range stores {
		if s.key == nil {
			app.fail()
			return nil, false
		}
		view := ctx.MultiStore().GetKVStore(s.key)
		token, err := app.nativeTrackers[i].Token(view)
		if err != nil {
			app.fail()
			return nil, false
		}
		memo := app.nativeMemo[i]
		if memo.image != nil && memo.token == token {
			out[i] = memo.image
			continue
		}
		values := make([]nativeKV, 0)
		func() {
			it := quiet(ctx).KVStore(s.key).Iterator(nil, nil)
			defer it.Close()
			for ; it.Valid(); it.Next() {
				key := nativeCopy(it.Key())
				if len(key) == 0 || (len(values) > 0 && bytes.Compare(values[len(values)-1].Key, key) >= 0) {
					app.fail()
					return
				}
				values = append(values, nativeKV{key, nativeCopy(it.Value())})
			}
		}()
		now, e := app.nativeTrackers[i].Token(view)
		if e != nil || now != token {
			app.fail()
			return nil, false
		}
		out[i] = &nativeImage{values, app.nativeDigest(values)}
		app.nativeMemo[i] = nativeMemo{token, out[i]}
	}
	return out, true
}

// nativeFence observes the same cache context as the native operation. On a
// failed transaction these are attempted states, and the caller labels them
// aborted. No writes, bank arithmetic or shared gas meters are involved here.
func (app *GaiaApp) nativeFence(ctx sdk.Context) func() {
	stores := app.nativeStores()
	before, ok := app.nativeSnapshot(ctx, stores)
	base := app.nativeRecord("native_state", "", "native_store_bytes")
	closed := false
	return func() {
		defer func() {
			if recover() != nil {
				app.fail()
			}
		}()
		if closed || !ok {
			app.fail()
			return
		}
		closed = true
		after, valid := app.nativeSnapshot(ctx, stores)
		if !valid {
			return
		}
		for i, s := range stores {
			// Every raw changed key follows, while full-store commitments prove
			// unchanged keys too. Full baseline/final checkpoints are separate,
			// avoiding thousands of repeated full snapshots in export memory.
			label := "store:" + s.key.Name() + ":" + s.name
			r := base
			r.Kind, r.Label, r.Unit = "native_state_hash", label, "native_store_sha256"
			r.Before, r.After = before[i].digest, after[i].digest
			app.observer.pending = append(app.observer.pending, r)
			// Exact SAME native view plus unchanged mutation/flush epoch reuses
			// the complete immutable snapshot, not a sample or key projection.
			if before[i] == after[i] {
				continue
			}
			b, a := before[i].values, after[i].values
			for bi, ai := 0, 0; bi < len(b) || ai < len(a); {
				var key, bv, av []byte
				if ai == len(a) || (bi < len(b) && bytes.Compare(b[bi].Key, a[ai].Key) < 0) {
					key, bv = b[bi].Key, b[bi].Value
					bi++
				} else if bi == len(b) || bytes.Compare(b[bi].Key, a[ai].Key) > 0 {
					key, av = a[ai].Key, a[ai].Value
					ai++
				} else {
					key, bv, av = b[bi].Key, b[bi].Value, a[ai].Value
					bi++
					ai++
					if bytes.Equal(bv, av) {
						continue
					}
				}
				r = base
				r.Label = label + "/key:" + base64.StdEncoding.EncodeToString(key)
				r.Before, r.After = app.nativeJSON(bv), app.nativeJSON(av)
				app.observer.pending = append(app.observer.pending, r)
			}
		}
	}
}

// Native decoded objects supplement the complete raw checkpoints. A nil object
// means the native keeper did not find it, never a fabricated zero-valued object.
type nativeStakeState struct {
	Validator  *staking.Validator  `json:"validator"`
	Delegation *staking.Delegation `json:"delegation"`
}

func (app *GaiaApp) nativeStake(ctx sdk.Context, del sdk.AccAddress, val sdk.ValAddress) nativeStakeState {
	var s nativeStakeState
	if v, found := app.stakingKeeper.GetValidator(quiet(ctx), val); found {
		if !bytes.Equal(v.OperatorAddress, val) || v.Tokens.IsNegative() ||
			v.DelegatorShares.IsNil() || v.DelegatorShares.IsNegative() ||
			(v.Status != sdk.Bonded && v.Status != sdk.Unbonded && v.Status != sdk.Unbonding) {
			app.fail()
		}
		s.Validator = &v
	}
	if d, found := app.stakingKeeper.GetDelegation(quiet(ctx), del, val); found {
		if !bytes.Equal(d.DelegatorAddress, del) || !bytes.Equal(d.ValidatorAddress, val) ||
			d.Shares.IsNil() || d.Shares.IsNegative() {
			app.fail()
		}
		s.Delegation = &d
	}
	if s.Delegation != nil && (s.Validator == nil || s.Delegation.Shares.GT(s.Validator.DelegatorShares)) {
		app.fail()
	}
	return s
}

func nativeDelShares(s nativeStakeState) sdk.Dec {
	if s.Delegation == nil {
		return sdk.ZeroDec()
	}
	return s.Delegation.Shares
}

// SDK v0.33.2 requests source SHARES, not token Value. Principal is only the
// actual native source validator token debit after a successful handler.
func (app *GaiaApp) principal(ctx sdk.Context, msg sdk.Msg) (finish func(bool)) {
	finish = func(bool) {}
	defer func() {
		if recover() != nil {
			app.fail()
			finish = func(bool) {}
		}
	}()
	var src, dst sdk.ValAddress
	var del sdk.AccAddress
	var requested sdk.Dec
	var label string
	switch m := msg.(type) {
	case staking.MsgUndelegate:
		src, del, requested, label = m.ValidatorAddress, m.DelegatorAddress, m.SharesAmount, "undelegate"
	case staking.MsgBeginRedelegate:
		src, dst, del, requested, label = m.ValidatorSrcAddress, m.ValidatorDstAddress, m.DelegatorAddress, m.SharesAmount, "redelegate"
	case staking.MsgDelegate, staking.MsgCreateValidator:
		return app.addedShares(ctx, msg)
	case *staking.MsgUndelegate:
		return app.principal(ctx, *m)
	case *staking.MsgBeginRedelegate:
		return app.principal(ctx, *m)
	default:
		return finish
	}
	base := app.nativeRecord("share_request", label+":requested_source_shares", "delegation_shares")
	base.Phase = "message"
	base.Validator, base.Delegator, base.Address = src.String(), del.String(), del.String()
	if requested.IsNil() || !requested.IsPositive() || len(src) == 0 || len(del) == 0 || (label == "redelegate" && len(dst) == 0) {
		app.fail()
		return finish
	}
	base.Amount, base.Before = requested.String(), app.nativeJSON(requested)
	app.observer.pending = append(app.observer.pending, base)
	before := app.nativeStake(ctx, del, src)
	var destBefore nativeStakeState
	if len(dst) > 0 {
		destBefore = app.nativeStake(ctx, del, dst)
	}
	closed := false
	return func(success bool) {
		defer func() {
			if recover() != nil {
				app.fail()
			}
		}()
		if closed {
			app.fail()
			return
		}
		closed = true
		if !success {
			return
		}
		if before.Validator == nil || before.Delegation == nil || requested.GT(before.Delegation.Shares) {
			app.fail()
			return
		}
		after := app.nativeStake(ctx, del, src)
		afterTokens, afterShares := sdk.ZeroInt(), sdk.ZeroDec()
		if after.Validator != nil {
			afterTokens, afterShares = after.Validator.Tokens, after.Validator.DelegatorShares
		} else if before.Validator.Status != sdk.Unbonded || !before.Validator.DelegatorShares.Equal(requested) || after.Delegation != nil {
			// Native unbond removes an unbonded validator only at zero shares.
			app.fail()
			return
		}
		n := before.Validator.Tokens.Sub(afterTokens)
		delta := nativeDelShares(after).Sub(nativeDelShares(before))
		if n.IsNegative() || !delta.Equal(requested.Neg()) ||
			!before.Validator.DelegatorShares.Sub(afterShares).Equal(requested) {
			app.fail()
			return
		}
		r := base
		r.Kind, r.Label, r.Unit = "principal", label, "native_bond_tokens"
		r.Amount, r.Denom = n.String(), app.stakingKeeper.BondDenom(quiet(ctx))
		if r.Denom == "" {
			app.fail()
			return
		}
		r.Before, r.After = app.nativeJSON(before), app.nativeJSON(after)
		records := []Record{r}
		r.Kind, r.Label, r.Unit, r.Amount, r.Denom = "staking_shares", label+":source", "delegation_shares", delta.String(), ""
		records = append(records, r)
		if len(dst) > 0 {
			destAfter := app.nativeStake(ctx, del, dst)
			if bytes.Equal(src, dst) || destBefore.Validator == nil || destAfter.Validator == nil || destAfter.Delegation == nil || !n.IsPositive() {
				app.fail()
				return
			}
			change := nativeDelShares(destAfter).Sub(nativeDelShares(destBefore))
			if !change.IsPositive() || !destAfter.Validator.Tokens.Sub(destBefore.Validator.Tokens).Equal(n) ||
				!destAfter.Validator.DelegatorShares.Sub(destBefore.Validator.DelegatorShares).Equal(change) {
				app.fail()
				return
			}
			r.Validator, r.Label, r.Amount = dst.String(), label+":destination", change.String()
			r.Before, r.After = app.nativeJSON(destBefore), app.nativeJSON(destAfter)
			records = append(records, r)
		}
		app.observer.pending = append(app.observer.pending, records...)
	}
}
