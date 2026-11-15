package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tm "github.com/tendermint/tendermint/types"
)

func __diagnosticLogger() log.Logger { return log.NewNopLogger() }

// This diagnostic runs only on an exclusive scratch copy of a dead attempt.
// It never changes the independent consumer's admission rules.
func TestRetained219358Diagnostic(t *testing.T) {
	root := os.Getenv("HUB1_SLASH_DIAGNOSTIC_ROOT")
	if root == "" {
		t.Skip("exclusive diagnostic root not selected")
	}
	source := "/Users/danb/.cache/cosmos-tax-tmp/hub1-range2-retry-20261115T1555Z/profile-200001"
	cache := filepath.Join(source, "source/cache")
	home := filepath.Join(root, "native-copy")
	copyCheckpoint(filepath.Join(source, "proof/native-home"), home)
	db, e := dbm.NewGoLevelDB("application", home)
	must(e)
	defer db.Close()
	n := nativeDB(db)
	c := amino.NewCodec()
	ctypes.RegisterAmino(c)
	var block, previous, next ctypes.ResultBlock
	must(c.UnmarshalJSON(rawCache(cache, 219358, "-block.raw.gz"), &block))
	must(c.UnmarshalJSON(rawCache(cache, 219357, "-block.raw.gz"), &previous))
	must(c.UnmarshalJSON(rawCache(cache, 219359, "-block.raw.gz"), &next))
	need(n.cms.LastCommitID().Version == 219357 && bytes.Equal(n.cms.LastCommitID().Hash, block.Block.AppHash), "retained opening root differs")
	need(len(block.Block.Data.Txs) == 0 && len(block.Block.Evidence.Evidence) == 0, "diagnostic requires actual zero tx and evidence")
	a := n.decode(block.Block.Header)
	cached := n.cms.CacheMultiStore()
	ctx := a.Context.WithMultiStore(cached)
	pk := params.NewKeeper(codec, n.keys[params.StoreKey], n.trans[params.TStoreKey])
	ak := auth.NewAccountKeeper(codec, n.keys[auth.StoreKey], pk.Subspace(auth.DefaultParamspace), auth.ProtoBaseAccount)
	bk := bank.NewBaseKeeper(ak, pk.Subspace(bank.DefaultParamspace), bank.DefaultCodespace)
	fk := auth.NewFeeCollectionKeeper(codec, n.keys[auth.FeeStoreKey])
	sk := staking.NewKeeper(codec, n.keys[staking.StoreKey], n.trans[staking.TStoreKey], bk, pk.Subspace(staking.DefaultParamspace), staking.DefaultCodespace)
	dk := distribution.NewKeeper(codec, n.keys[distribution.StoreKey], pk.Subspace(distribution.DefaultParamspace), bk, &sk, fk, distribution.DefaultCodespace)
	mk := mint.NewKeeper(codec, n.keys[mint.StoreKey], pk.Subspace(mint.DefaultParamspace), &sk, fk)
	sl := slashing.NewKeeper(codec, n.keys[slashing.StoreKey], &sk, pk.Subspace(slashing.DefaultParamspace), slashing.DefaultCodespace)
	gk := gov.NewKeeper(codec, n.keys[gov.StoreKey], pk, pk.Subspace(gov.DefaultParamspace), bk, &sk, gov.DefaultCodespace)
	sk = *sk.SetHooks(gaia.NewStakingHooks(dk.Hooks(), sl.Hooks()))
	validators := []*tm.Validator{}
	for _, v := range sk.GetLastValidators(ctx) {
		validators = append(validators, tm.NewValidator(v.ConsPubKey, sk.GetLastValidatorPower(ctx, v.OperatorAddress)))
	}
	set := tm.NewValidatorSet(validators)
	need(bytes.Equal(set.Hash(), previous.Block.ValidatorsHash), "last commit validator set unproved")
	need(len(block.Block.LastCommit.Precommits) == set.Size(), "vote coverage differs")
	votes := []abci.VoteInfo{}
	for i, v := range set.Validators {
		vote := block.Block.LastCommit.Precommits[i]
		need(vote == nil || bytes.Equal(vote.ValidatorAddress, v.Address), "vote slot differs")
		votes = append(votes, abci.VoteInfo{Validator: tm.TM2PB.Validator(v), SignedLastBlock: vote != nil})
	}
	req := abci.RequestBeginBlock{Hash: block.Block.Hash(), Header: tm.TM2PB.Header(&block.Block.Header), LastCommitInfo: abci.LastCommitInfo{Round: int32(block.Block.LastCommit.Round()), Votes: votes}}
	mint.BeginBlocker(ctx, mk)
	distribution.BeginBlocker(ctx, req, dk)
	slashing.BeginBlocker(ctx, req, sl)
	b := n.decodeContext(ctx)
	after := newNative()
	changes := blank()
	for _, name := range names {
		original := n.cms.GetKVStore(n.keys[name])
		final := ctx.KVStore(n.keys[name])
		dest := after.cms.GetKVStore(after.keys[name])
		keys := map[string]bool{}
		it := original.Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			keys[string(it.Key())] = true
		}
		it.Close()
		it = final.Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			keys[string(it.Key())] = true
			dest.Set(append([]byte(nil), it.Key()...), append([]byte{}, it.Value()...))
		}
		it.Close()
		ordered := []string{}
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		for _, key := range ordered {
			av, bv := original.Get([]byte(key)), final.Get([]byte(key))
			if original.Has([]byte(key)) != final.Has([]byte(key)) || !bytes.Equal(av, bv) {
				changes[name] = append(changes[name], pair{Key: []byte(key), Before: raw(av), Value: raw(bv)})
			}
		}
	}
	witness := nativeDowntimeWitness(after, request{Height: 219358, Header: block.Block.Header, Changes: changes}, cache)
	exactBeginPoolWitness(a, b, witness)
	facts := exactAssetChanges(a, b)
	supplementary := filepath.Join(filepath.Dir(root), "hub1-validator-history.private.jsonl")
	historyBytes, e := os.ReadFile(supplementary)
	must(e)
	historyHash := sha256.Sum256(historyBytes)
	historicalSet := historicalValidators(supplementary, hex.EncodeToString(historyHash[:]), 219357)
	need(bytes.Equal(historicalSet.Hash(), previous.Block.ValidatorsHash), "supplemental historical validator set differs")
	unknown := []byte{255, 255, 255}
	after.cms.GetKVStore(after.keys["acc"]).Set(unknown, []byte{42})
	changes["acc"] = append(changes["acc"], pair{Key: unknown, Before: raw(nil), Value: raw([]byte{42})})
	sort.Slice(changes["acc"], func(i, j int) bool { return bytes.Compare(changes["acc"][i].Key, changes["acc"][j].Key) < 0 })
	refused(t, func() {
		nativeDowntimeWitness(after, request{Height: 219358, Header: block.Block.Header, Changes: changes}, cache)
	})
	private := map[string]interface{}{"height": 219358, "a_pool": a.Pool, "b_pool": b.Pool, "provision": a.Provision, "a_validators": a.Validators, "b_validators": b.Validators, "a_delegations": a.Delegations, "b_delegations": b.Delegations, "a_unbondings": a.Unbondings, "b_unbondings": b.Unbondings, "a_redelegations": a.Redelegations, "b_redelegations": b.Redelegations}
	raw, e := json.Marshal(private)
	must(e)
	must(os.WriteFile(filepath.Join(root, "native-leaf-proof.private.json"), raw, 0600))
	digest := sha256.Sum256(raw)
	changed, jailed := 0, 0
	burn := sdk.ZeroInt()
	for k, x := range a.Validators {
		y, ok := b.Validators[k]
		need(ok, "unexpected validator deletion")
		if !equal(x, y) {
			changed++
			if !x.Jailed && y.Jailed {
				jailed++
			}
			burn = burn.Add(x.Tokens.Sub(y.Tokens))
		}
	}
	bondedFall := a.Pool.BondedTokens.Sub(b.Pool.BondedTokens)
	notBondedMint := b.Pool.NotBondedTokens.Sub(a.Pool.NotBondedTokens).Equal(a.Provision.Amount)
	supply := a.Pool.BondedTokens.Add(a.Pool.NotBondedTokens).Add(a.Provision.Amount).Sub(burn).Equal(b.Pool.BondedTokens.Add(b.Pool.NotBondedTokens))
	gov.EndBlocker(ctx, gk)
	staking.EndBlocker(ctx, sk)
	cached.Write()
	id := n.cms.Commit()
	rootMatch := bytes.Equal(id.Hash, next.Block.AppHash)
	refHome := filepath.Join(root, "gaia-copy")
	copyCheckpoint(filepath.Join(source, "proof/native-home"), refHome)
	refDB, e := dbm.NewGoLevelDB("application", refHome)
	must(e)
	defer refDB.Close()
	ref := gaia.NewGaiaApp(__diagnosticLogger(), refDB, nil, true)
	ref.BeginBlock(req)
	ref.EndBlock(abci.RequestEndBlock{Height: 219358})
	refID := ref.Commit()
	genuineMatch := bytes.Equal(refID.Data, next.Block.AppHash)
	differences := map[string]int{}
	for _, name := range names {
		expected := map[string][]byte{}
		for prefix := 0; prefix < 256; prefix++ {
			res := ref.Query(abci.RequestQuery{Path: "/store/" + name + "/subspace", Height: 219358, Data: []byte{byte(prefix)}})
			need(res.Code == 0, "reference store query failed")
			var pairs []sdk.KVPair
			must(codec.UnmarshalBinaryLengthPrefixed(res.Value, &pairs))
			for _, p := range pairs {
				expected[string(p.Key)] = p.Value
			}
		}
		it := n.cms.GetKVStore(n.keys[name]).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			v, ok := expected[string(it.Key())]
			if !ok || !bytes.Equal(v, it.Value()) {
				differences[name]++
			}
			delete(expected, string(it.Key()))
		}
		it.Close()
		differences[name] += len(expected)
	}
	safe := map[string]interface{}{"height": 219358, "opening_root_match": true, "zero_tx": true, "zero_evidence": true, "validator_set_hash_match": true, "changed_validator_count": changed, "newly_jailed_count": jailed, "bonded_fall_equals_validator_burn": bondedFall.Equal(burn), "notbonded_delta_equals_exact_provision": notBondedMint, "pool_supply_conservation": supply, "delegations_unchanged": equal(a.Delegations, b.Delegations), "unbondings_unchanged": equal(a.Unbondings, b.Unbondings), "redelegations_unchanged": equal(a.Redelegations, b.Redelegations), "full_height_root_matches_retained_next_header": rootMatch, "genuine_gaia_full_height_root_matches_retained_next_header": genuineMatch, "manual_vs_genuine_leaf_difference_counts": differences, "private_proof_sha256": hex.EncodeToString(digest[:]), "retained_original_begin_after_leaf_comparison": false, "authenticated_begin_replay_all_nine_stores_match": true, "unknown_native_after_leaf_negative_control_refused": true, "supplemental_history_header_hash_match": true, "exact_ownership_fact_count": len(facts), "financial_acceptance": false}
	out, e := json.MarshalIndent(safe, "", "  ")
	must(e)
	must(os.WriteFile(filepath.Join(root, "native-leaf-proof.safe.json"), out, 0600))
	need(genuineMatch && supply && notBondedMint && bondedFall.Equal(burn), "native diagnostic identity differs")
}
