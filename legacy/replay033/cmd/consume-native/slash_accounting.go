package main

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"math/big"
	"sort"
)

// Stake reduction is an asset-state fact, never a deductible tax-loss decision.
// Fractions retain the exact ownership of every native token and avoid plugging
// the rounding remainder into an arbitrary delegator's balance.
type stakeReduction struct {
	Position                     string `json:"position"`
	BeforePresent                bool   `json:"before_present"`
	AfterPresent                 bool   `json:"after_present"`
	Validator                    string `json:"validator"`
	Delegator                    string `json:"delegator"`
	Numerator                    string `json:"numerator"`
	Denominator                  string `json:"denominator"`
	Classification               string `json:"classification"`
	SharesBefore                 string `json:"native_delegation_shares_before"`
	SharesAfter                  string `json:"native_delegation_shares_after"`
	ValidatorSharesBefore        string `json:"native_validator_shares_before"`
	ValidatorSharesAfter         string `json:"native_validator_shares_after"`
	ValidatorTokensBefore        string `json:"native_validator_tokens_before"`
	ValidatorTokensAfter         string `json:"native_validator_tokens_after"`
	NativeDecBefore              string `json:"native_dec_before"`
	NativeDecAfter               string `json:"native_dec_after"`
	NativeDecReduction           string `json:"native_dec_reduction"`
	RoundingRemainderNumerator   string `json:"rounding_remainder_numerator"`
	RoundingRemainderDenominator string `json:"rounding_remainder_denominator"`
}

// This helper is for a native-replayed, all-store-verified slash transition.
// It deliberately refuses share-changing/redelegation paths until their
// separate native ownership transitions have been supplied and verified.
func unchangedShareReductions(a, b financial) []stakeReduction {
	need(equal(a.Delegations, b.Delegations), "share-changing slash needs separate native ownership proof")
	need(equal(a.Unbondings, b.Unbondings) && equal(a.Redelegations, b.Redelegations), "unchanged-share helper cannot drop unbonding or redelegation changes")
	out := []stakeReduction{}
	keys := []string{}
	for k := range a.Validators {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := a.Validators[k]
		w, ok := b.Validators[k]
		need(ok, "slash validator disappearance lacks ownership proof")
		loss := v.Tokens.Sub(w.Tokens)
		need(!loss.IsNegative(), "slash validator token increase unexplained")
		if loss.IsZero() {
			continue
		}
		need(v.DelegatorShares.Equal(w.DelegatorShares) && v.DelegatorShares.IsPositive(), "slash total shares changed")
		total := new(big.Rat)
		shares := sdk.ZeroDec()
		dels := []string{}
		for key, d := range a.Delegations {
			if d.ValidatorAddress.String() == k {
				dels = append(dels, key)
			}
		}
		sort.Strings(dels)
		for _, key := range dels {
			d := a.Delegations[key]
			shares = shares.Add(d.Shares)
			fraction, ok := new(big.Rat).SetString(d.Shares.String())
			need(ok, "native delegation share decimal invalid")
			denominator, ok := new(big.Rat).SetString(v.DelegatorShares.String())
			need(ok, "native validator share decimal invalid")
			fraction.Quo(fraction, denominator)
			fraction.Mul(fraction, new(big.Rat).SetInt(loss.BigInt()))
			total.Add(total, fraction)
			nativeBefore, nativeAfter := v.ShareTokens(d.Shares), w.ShareTokens(d.Shares)
			nativeReduction := nativeBefore.Sub(nativeAfter)
			nativeRational, ok := new(big.Rat).SetString(nativeReduction.String())
			need(ok, "native reduction decimal invalid")
			remainder := new(big.Rat).Sub(new(big.Rat).Set(fraction), nativeRational)
			out = append(out, stakeReduction{Validator: k, Delegator: d.DelegatorAddress.String(), Numerator: fraction.Num().String(), Denominator: fraction.Denom().String(), Classification: "native_stake_asset_reduction_unclassified", NativeDecBefore: nativeBefore.String(), NativeDecAfter: nativeAfter.String(), NativeDecReduction: nativeReduction.String(), RoundingRemainderNumerator: remainder.Num().String(), RoundingRemainderDenominator: remainder.Denom().String()})
		}
		need(shares.Equal(v.DelegatorShares), "all-delegator share coverage differs")
		need(total.Cmp(new(big.Rat).SetInt(loss.BigInt())) == 0, "exact ownership reduction conservation differs")
	}
	return out
}

func requiresNativeBeginWitness(r request, a, b financial) bool {
 return r.Phase == "begin_block" && beginNeedsWitness(a,b)
}

func beginNeedsWitness(a, b financial) bool {
	return !(b.Pool.NotBondedTokens.Sub(a.Pool.NotBondedTokens).Equal(a.Provision.Amount) && b.Pool.BondedTokens.Equal(a.Pool.BondedTokens) && equal(a.Validators, b.Validators) && equal(a.Delegations, b.Delegations) && equal(a.Unbondings, b.Unbondings) && equal(a.Redelegations, b.Redelegations))
}

// expected must come from actual SDK BeginBlock execution with authenticated
// native before-state, votes/evidence and all-nine-store after-state equality.
// Merely observing a pool decrease does not establish slash authority.
func exactBeginPoolWitness(a, b, expected financial) {
	need(equal(b.Pool, expected.Pool) && equal(b.Validators, expected.Validators) && equal(b.Delegations, expected.Delegations) && equal(b.Unbondings, expected.Unbondings) && equal(b.Redelegations, expected.Redelegations), "native slash witness differs")
	before := a.Pool.BondedTokens.Add(a.Pool.NotBondedTokens)
	after := b.Pool.BondedTokens.Add(b.Pool.NotBondedTokens)
	// Redelegation entries are overlapping slash claims, not additional tokens.
	// Their destination delegation ownership and validator tokens are already
	// counted. All created/deleted positions remain covered by the native witness.
	stakeTotal := func(v financial) sdk.Int {
		sum := sdk.ZeroInt()
		for _, x := range v.Validators {
			sum = sum.Add(x.Tokens)
		}
		for _, u := range v.Unbondings {
			for _, e := range u.Entries {
				sum = sum.Add(e.Balance)
			}
		}
		return sum
	}
	burned := stakeTotal(a).Sub(stakeTotal(b))
	need(!burned.IsNegative() && before.Add(a.Provision.Amount).Sub(burned).Equal(after), "exact native mint/slash pool conservation differs")
}
