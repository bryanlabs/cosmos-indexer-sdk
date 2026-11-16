// hub1-store-facts reads the staking, slashing and distribution stores of a
// private copy of a native working state at two committed heights and says
// what changed between them. It opens the copy read-only in effect: nothing is
// committed. The public line carries counts and yes/no only; amounts and
// identities go to the private file.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	dbm "github.com/tendermint/tendermint/libs/db"
)

var names = []string{"main", "acc", "staking", "slashing", "mint", "distr", "gov", "fee", "params"}
var cdc = gaia.MakeCodec()

func must(e error) {
	if e != nil {
		panic(e)
	}
}

type view struct {
	Version        int64
	AppHash        string
	Pool           skt.Pool
	Validators     map[string]skt.Validator
	Delegations    map[string]string
	Unbonding      map[string]string
	UnbondingSum   sdk.Int
	Redelegation   map[string]string
	Signing        map[string]slashing.ValidatorSigningInfo
	MissedBits     map[string]bool
	SlashEvents    map[string]bool
	LastPower      map[string]bool
	ValidatorQueue map[string]bool
	raw            map[string]map[string]string
}

func read(home string, version int64) view {
	db, e := dbm.NewGoLevelDB("application", home)
	must(e)
	defer db.Close()
	cms := store.NewCommitMultiStore(db)
	keys := map[string]*sdk.KVStoreKey{}
	for _, name := range names {
		keys[name] = sdk.NewKVStoreKey(name)
		cms.MountStoreWithDB(keys[name], st.StoreTypeIAVL, nil)
	}
	cms.SetPruning(st.NewPruningOptions(100, 0))
	must(cms.LoadVersion(version))
	id := cms.LastCommitID()
	v := view{Version: id.Version, AppHash: hex.EncodeToString(id.Hash), Validators: map[string]skt.Validator{}, Delegations: map[string]string{}, Unbonding: map[string]string{}, UnbondingSum: sdk.ZeroInt(),
		Redelegation: map[string]string{}, Signing: map[string]slashing.ValidatorSigningInfo{}, MissedBits: map[string]bool{}, SlashEvents: map[string]bool{}, LastPower: map[string]bool{}, ValidatorQueue: map[string]bool{}, raw: map[string]map[string]string{}}
	each := func(name string, f func(k, val []byte)) {
		v.raw[name] = map[string]string{}
		it := cms.GetKVStore(keys[name]).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			sum := sha256.Sum256(it.Value())
			v.raw[name][hex.EncodeToString(it.Key())] = hex.EncodeToString(sum[:])
			if f != nil {
				f(it.Key(), it.Value())
			}
		}
		it.Close()
	}
	for _, name := range names {
		switch name {
		case "staking":
			each(name, func(k, val []byte) {
				id := hex.EncodeToString(k)
				switch k[0] {
				case 0x00:
					v.Pool = skt.MustUnmarshalPool(cdc, val)
				case 0x11:
					v.LastPower[id] = true
				case 0x21:
					v.Validators[id] = skt.MustUnmarshalValidator(cdc, val)
				case 0x31:
					v.Delegations[id] = skt.MustUnmarshalDelegation(cdc, val).Shares.String()
				case 0x32:
					u := skt.MustUnmarshalUBD(cdc, val)
					for _, entry := range u.Entries {
						v.UnbondingSum = v.UnbondingSum.Add(entry.Balance)
					}
					v.Unbonding[id] = hex.EncodeToString(val)
				case 0x34:
					v.Redelegation[id] = hex.EncodeToString(val)
				case 0x43:
					v.ValidatorQueue[id] = true
				}
			})
		case "slashing":
			each(name, func(k, val []byte) {
				id := hex.EncodeToString(k)
				switch k[0] {
				case 0x01:
					var info slashing.ValidatorSigningInfo
					cdc.MustUnmarshalBinaryLengthPrefixed(val, &info)
					v.Signing[id] = info
				case 0x02:
					v.MissedBits[id] = true
				}
			})
		case "distr":
			each(name, func(k, val []byte) {
				if k[0] == 0x08 {
					v.SlashEvents[hex.EncodeToString(k)] = true
				}
			})
		default:
			each(name, nil)
		}
	}
	return v
}

func changed(a, b map[string]string) (added, removed, differ int) {
	for k, x := range a {
		if y, ok := b[k]; !ok {
			removed++
		} else if x != y {
			differ++
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			added++
		}
	}
	return
}

func set(a, b map[string]bool) (added, removed int) {
	for k := range a {
		if !b[k] {
			removed++
		}
	}
	for k := range b {
		if !a[k] {
			added++
		}
	}
	return
}

func main() {
	home := flag.String("home", "", "private copy of a native working state")
	before := flag.Int64("before", 0, "committed height before the block")
	after := flag.Int64("after", 0, "committed height of the block")
	afterHome := flag.String("after-home", "", "a second private copy holding the after height; default is the same copy")
	private := flag.String("private", "", "private file for amounts and identities")
	flag.Parse()
	if *afterHome == "" {
		*afterHome = *home
	}
	a, b := read(*home, *before), read(*afterHome, *after)
	out := map[string]interface{}{"schema": "hub1-store-facts-v1", "before_height": a.Version, "after_height": b.Version, "before_app_hash": a.AppHash, "after_app_hash": b.AppHash}
	detail := map[string]interface{}{"before_pool": a.Pool, "after_pool": b.Pool, "validators": []interface{}{}, "signing": []interface{}{}}
	tokens, shares, jailed, unjailed, status, tokenDown, tokenUp := 0, 0, 0, 0, 0, 0, 0
	sumA, sumB := sdk.ZeroInt(), sdk.ZeroInt()
	ids := []string{}
	for k := range a.Validators {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	vd := []interface{}{}
	for _, k := range ids {
		x := a.Validators[k]
		sumA = sumA.Add(x.Tokens)
		y, ok := b.Validators[k]
		if !ok {
			continue
		}
		d := false
		if !x.Tokens.Equal(y.Tokens) {
			tokens++
			d = true
			if y.Tokens.LT(x.Tokens) {
				tokenDown++
			} else {
				tokenUp++
			}
		}
		if !x.DelegatorShares.Equal(y.DelegatorShares) {
			shares++
			d = true
		}
		if x.Jailed != y.Jailed {
			d = true
			if y.Jailed {
				jailed++
			} else {
				unjailed++
			}
		}
		if x.Status != y.Status {
			status++
			d = true
		}
		if d {
			vd = append(vd, map[string]interface{}{"validator": x.OperatorAddress.String(), "tokens_before": x.Tokens.String(), "tokens_after": y.Tokens.String(), "shares_before": x.DelegatorShares.String(), "shares_after": y.DelegatorShares.String(),
				"jailed_before": x.Jailed, "jailed_after": y.Jailed, "status_before": x.Status, "status_after": y.Status})
		}
	}
	for _, y := range b.Validators {
		sumB = sumB.Add(y.Tokens)
	}
	detail["validators"] = vd
	jailedNow := 0
	for _, y := range b.Validators {
		if y.Jailed {
			jailedNow++
		}
	}
	va, vr := 0, 0
	for k := range a.Validators {
		if _, ok := b.Validators[k]; !ok {
			vr++
		}
	}
	for k := range b.Validators {
		if _, ok := a.Validators[k]; !ok {
			va++
		}
	}
	sd := []interface{}{}
	jailedUntil, counterReset, counterUp, counterDown, tomb, start := 0, 0, 0, 0, 0, 0
	signing := []string{}
	for k := range a.Signing {
		signing = append(signing, k)
	}
	sort.Strings(signing)
	for _, k := range signing {
		x := a.Signing[k]
		y, ok := b.Signing[k]
		if !ok {
			continue
		}
		d := false
		if !x.JailedUntil.Equal(y.JailedUntil) {
			jailedUntil++
			d = true
		}
		if x.MissedBlocksCounter != y.MissedBlocksCounter {
			d = true
			if y.MissedBlocksCounter == 0 {
				counterReset++
			} else if y.MissedBlocksCounter > x.MissedBlocksCounter {
				counterUp++
			} else {
				counterDown++
			}
		}
		if x.Tombstoned != y.Tombstoned {
			tomb++
			d = true
		}
		if x.StartHeight != y.StartHeight {
			start++
			d = true
		}
		if d {
			sd = append(sd, map[string]interface{}{"key": k, "before": x, "after": y})
		}
	}
	detail["signing"] = sd
	da, dr, dd := changed(a.Delegations, b.Delegations)
	ua, ur, ud := changed(a.Unbonding, b.Unbonding)
	ra, rr, rd := changed(a.Redelegation, b.Redelegation)
	ma, mr := set(a.MissedBits, b.MissedBits)
	sa, sr := set(a.SlashEvents, b.SlashEvents)
	la, lr := set(a.LastPower, b.LastPower)
	qa, qr := set(a.ValidatorQueue, b.ValidatorQueue)
	stores := map[string]int{}
	for _, name := range names {
		x, y, z := changed(a.raw[name], b.raw[name])
		stores[name] = x + y + z
	}
	poolA, poolB := a.Pool.BondedTokens.Add(a.Pool.NotBondedTokens), b.Pool.BondedTokens.Add(b.Pool.NotBondedTokens)
	stakeA, stakeB := sumA.Add(a.UnbondingSum), sumB.Add(b.UnbondingSum)
	detail["pool_total_change"] = poolB.Sub(poolA).String()
	detail["validator_tokens_plus_unbonding_change"] = stakeB.Sub(stakeA).String()
	detail["validator_tokens_change"] = sumB.Sub(sumA).String()
	detail["unbonding_balance_change"] = b.UnbondingSum.Sub(a.UnbondingSum).String()
	out["validators"] = map[string]interface{}{"before": len(a.Validators), "after": len(b.Validators), "added": va, "removed": vr, "tokens_changed": tokens, "tokens_went_down": tokenDown, "tokens_went_up": tokenUp, "shares_changed": shares,
		"newly_jailed": jailed, "unjailed": unjailed, "status_changed": status, "jailed_after": jailedNow}
	out["bonded_set_members"] = map[string]int{"added": la, "removed": lr}
	out["validators_queued_to_finish_unbonding"] = map[string]int{"added": qa, "removed": qr}
	out["delegations"] = map[string]int{"added": da, "removed": dr, "shares_changed": dd}
	out["unbonding_delegations"] = map[string]int{"added": ua, "removed": ur, "changed": ud}
	out["redelegations"] = map[string]int{"added": ra, "removed": rr, "changed": rd}
	out["signing_info"] = map[string]int{"before": len(a.Signing), "after": len(b.Signing), "jailed_until_changed": jailedUntil, "missed_counter_reset_to_zero": counterReset, "missed_counter_up": counterUp, "missed_counter_down": counterDown,
		"tombstoned_changed": tomb, "start_height_changed": start}
	out["missed_block_bits"] = map[string]int{"added": ma, "removed": mr}
	out["distribution_slash_events"] = map[string]int{"added": sa, "removed": sr}
	out["bonded_pool_changed"] = !a.Pool.BondedTokens.Equal(b.Pool.BondedTokens)
	out["not_bonded_pool_changed"] = !a.Pool.NotBondedTokens.Equal(b.Pool.NotBondedTokens)
	out["bonded_pool_went_down"] = b.Pool.BondedTokens.LT(a.Pool.BondedTokens)
	out["pool_total_went_down"] = poolB.LT(poolA)
	out["validator_tokens_plus_unbonding_balances_went_down"] = stakeB.LT(stakeA)
	out["validator_tokens_plus_unbonding_balances_unchanged"] = stakeB.Equal(stakeA)
	out["keys_changed_by_store"] = stores
	if *private != "" {
		raw, e := json.MarshalIndent(map[string]interface{}{"public": out, "private": detail}, "", " ")
		must(e)
		f, e := os.OpenFile(*private, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		must(e)
		_, e = f.Write(append(raw, '\n'))
		must(e)
		must(f.Close())
		sum := sha256.Sum256(append(raw, '\n'))
		out["private_sha256"] = hex.EncodeToString(sum[:])
	}
	line := &bytes.Buffer{}
	must(json.NewEncoder(line).Encode(out))
	fmt.Print(line.String())
}
