package main

import (
	"fmt"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"math/big"
	"sort"
)

type nativeOwnership struct {
	Owner, Validator, Kind, Shares, ValidatorShares, ValidatorTokens string
	Exact                                                            *big.Rat
	Native                                                           sdk.Dec
}

func decimalRat(v sdk.Dec) *big.Rat {
	q, ok := new(big.Rat).SetString(v.String())
	need(ok, "native decimal not rational")
	return q
}
func ownershipState(v financial) map[string]nativeOwnership {
	out := map[string]nativeOwnership{}
	covered := map[string]sdk.Dec{}
	for key, d := range v.Delegations {
		validator, ok := v.Validators[d.ValidatorAddress.String()]
		need(ok && validator.DelegatorShares.IsPositive(), "delegation lacks native validator share denominator")
		q := new(big.Rat).Quo(decimalRat(d.Shares), decimalRat(validator.DelegatorShares))
		q.Mul(q, new(big.Rat).SetInt(validator.Tokens.BigInt()))
		out["delegation/"+key] = nativeOwnership{Owner: d.DelegatorAddress.String(), Validator: d.ValidatorAddress.String(), Kind: "delegation", Shares: d.Shares.String(), ValidatorShares: validator.DelegatorShares.String(), ValidatorTokens: validator.Tokens.String(), Exact: q, Native: validator.ShareTokens(d.Shares)}
		total, ok := covered[d.ValidatorAddress.String()]
		if !ok {
			total = sdk.ZeroDec()
		}
		covered[d.ValidatorAddress.String()] = total.Add(d.Shares)
	}
	for key, validator := range v.Validators {
		total, ok := covered[key]
		if !ok {
			total = sdk.ZeroDec()
		}
		need(total.Equal(validator.DelegatorShares), "native all-delegation ownership shares incomplete")
		need(validator.DelegatorShares.IsPositive() || validator.Tokens.IsZero(), "native validator tokens without delegation ownership")
	}
	for _, u := range v.Unbondings {
		for _, entry := range u.Entries {
			key := fmt.Sprintf("unbonding/%s/%s/%d/%s", u.DelegatorAddress.String(), u.ValidatorAddress.String(), entry.CreationHeight, entry.CompletionTime.UTC().Format("2006-01-02T15:04:05.999999999Z"))
			// Multiple entries at the same native creation/completion boundary belong
			// to the same owner. Aggregate rather than dropping an entry.
			old, exists := out[key]
			q := new(big.Rat).SetInt(entry.Balance.BigInt())
			native := entry.Balance.ToDec()
			if exists {
				q.Add(q, old.Exact)
				native = native.Add(old.Native)
			}
			out[key] = nativeOwnership{Owner: u.DelegatorAddress.String(), Validator: u.ValidatorAddress.String(), Kind: "unbonding", Exact: q, Native: native}
		}
	}
	return out
}
func exactAssetChanges(a, b financial) []stakeReduction {
	before, after := ownershipState(a), ownershipState(b)
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	ordered := []string{}
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	out := []stakeReduction{}
	sum := new(big.Rat)
	for _, k := range ordered {
		x, xok := before[k]
		y, yok := after[k]
		identity := x
		if !xok {
			identity = y
			x.Exact = new(big.Rat)
			x.Native = sdk.ZeroDec()
		}
		if !yok {
			y.Exact = new(big.Rat)
			y.Native = sdk.ZeroDec()
		}
		loss := new(big.Rat).Sub(x.Exact, y.Exact)
		native := x.Native.Sub(y.Native)
		sum.Add(sum, loss)
		if loss.Sign() == 0 && native.IsZero() && xok == yok && x.Shares == y.Shares && x.ValidatorShares == y.ValidatorShares && x.ValidatorTokens == y.ValidatorTokens {
			continue
		}
		remainder := new(big.Rat).Sub(new(big.Rat).Set(loss), decimalRat(native))
		out = append(out, stakeReduction{Position: k, BeforePresent: xok, AfterPresent: yok, Validator: identity.Validator, Delegator: identity.Owner, SharesBefore: x.Shares, SharesAfter: y.Shares, ValidatorSharesBefore: x.ValidatorShares, ValidatorSharesAfter: y.ValidatorShares, ValidatorTokensBefore: x.ValidatorTokens, ValidatorTokensAfter: y.ValidatorTokens, Numerator: loss.Num().String(), Denominator: loss.Denom().String(), Classification: "native_stake_asset_change_unclassified", NativeDecBefore: x.Native.String(), NativeDecAfter: y.Native.String(), NativeDecReduction: native.String(), RoundingRemainderNumerator: remainder.Num().String(), RoundingRemainderDenominator: remainder.Denom().String()})
	}
	tokenSum := func(v financial) sdk.Int {
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
	need(sum.Cmp(new(big.Rat).SetInt(tokenSum(a).Sub(tokenSum(b)).BigInt())) == 0, "all-account exact stake ownership conservation differs")
	return out
}
