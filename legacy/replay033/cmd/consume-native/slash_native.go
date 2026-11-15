package main

import (
	"bytes"
	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tm "github.com/tendermint/tendermint/types"
)

// Reconstruct a private before-state from the already-checked native after
// image. No existing tree or process is mutated. Every leaf remains covered.
func beforeClone(after *native, changes patch) *native {
	out := newNative()
	for _, name := range names {
		it := after.cms.GetKVStore(after.keys[name]).Iterator(nil, nil)
		kv := out.cms.GetKVStore(out.keys[name])
		for ; it.Valid(); it.Next() {
			kv.Set(append([]byte(nil), it.Key()...), append([]byte{}, it.Value()...))
		}
		it.Close()
	}
	inverse := patch{}
	for _, name := range names {
		for _, p := range changes[name] {
			inverse[name] = append(inverse[name], pair{Key: p.Key, Before: p.Value, Value: p.Before})
		}
		if inverse[name] == nil {
			inverse[name] = []pair{}
		}
	}
	out.apply(inverse)
	return out
}

// Native execution is a witness, not a heuristic inferred from pool deltas.
// Historical evidence and vote sets must match their retained header hashes.
func nativeDowntimeWitness(after *native, r request, cache string, historical ...string) financial {
	n := beforeClone(after, r.Changes)
	c := amino.NewCodec()
	ctypes.RegisterAmino(c)
	var block, previous ctypes.ResultBlock
	must(c.UnmarshalJSON(rawCache(cache, r.Height, "-block.raw.gz"), &block))
	must(c.UnmarshalJSON(rawCache(cache, r.Height-1, "-block.raw.gz"), &previous))
	need(block.Block != nil && previous.Block != nil && equal(block.Block.Header, r.Header), "slash witness header differs")
	historyPath, historyPin, evidenceCache, evidenceCapturePin := "", "", cache, ""
	if len(historical) == 4 {
		historyPath, historyPin, evidenceCache, evidenceCapturePin = historical[0], historical[1], historical[2], historical[3]
		if evidenceCache == "" {
			evidenceCache = cache
		}
	}
	a := n.decode(r.Header)
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
	sk = *sk.SetHooks(gaia.NewStakingHooks(dk.Hooks(), sl.Hooks()))
	validators := []*tm.Validator{}
	for _, v := range sk.GetLastValidators(ctx) {
		validators = append(validators, tm.NewValidator(v.ConsPubKey, sk.GetLastValidatorPower(ctx, v.OperatorAddress)))
	}
	set := tm.NewValidatorSet(validators)
	if !bytes.Equal(set.Hash(), previous.Block.ValidatorsHash) {
		set = historicalValidators(historyPath, historyPin, r.Height-1)
	}
	need(bytes.Equal(set.Hash(), previous.Block.ValidatorsHash), "slash last-commit validator set unproved")
	need(block.Block.LastCommit != nil && len(block.Block.LastCommit.Precommits) == set.Size(), "slash vote coverage differs")
	votes := []abci.VoteInfo{}
	for i, v := range set.Validators {
		vote := block.Block.LastCommit.Precommits[i]
		need(vote == nil || bytes.Equal(vote.ValidatorAddress, v.Address), "slash vote slot differs")
		votes = append(votes, abci.VoteInfo{Validator: tm.TM2PB.Validator(v), SignedLastBlock: vote != nil})
	}
	req := abci.RequestBeginBlock{Hash: block.Block.Hash(), Header: tm.TM2PB.Header(&block.Block.Header), LastCommitInfo: abci.LastCommitInfo{Round: int32(block.Block.LastCommit.Round()), Votes: votes}}
	for _, ev := range block.Block.Evidence.Evidence {
		evidenceSet := historicalValidators(historyPath, historyPin, ev.Height())
		var historicalBlock ctypes.ResultBlock
		if evidenceCache != cache {
			evidenceSourceCache(evidenceCache, evidenceCapturePin, ev.Height())
		}
		must(c.UnmarshalJSON(rawCache(evidenceCache, ev.Height(), "-block.raw.gz"), &historicalBlock))
		need(historicalBlock.Block != nil && bytes.Equal(evidenceSet.Hash(), historicalBlock.Block.ValidatorsHash), "evidence validator set header differs")
		_, validator := evidenceSet.GetByAddress(ev.Address())
		need(validator != nil, "evidence validator missing from authenticated set")
		req.ByzantineValidators = append(req.ByzantineValidators, tm.TM2PB.Evidence(ev, evidenceSet, block.Block.Time))
	}
	mint.BeginBlocker(ctx, mk)
	distribution.BeginBlocker(ctx, req, dk)
	slashing.BeginBlocker(ctx, req, sl)
	expected := n.decodeContext(ctx)
	cached.Write()
	sameRaw(n, after)
	return expected
}
