package main

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	"math/big"
	"testing"
	"time"
)

func TestChangingSharesPreservesEveryOwnersExactState(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	val := sdk.ValAddress(make([]byte, 20))
	owner1 := sdk.AccAddress(append([]byte{1}, make([]byte, 19)...))
	owner2 := sdk.AccAddress(append([]byte{2}, make([]byte, 19)...))
	a.Validators[val.String()] = skt.Validator{Tokens: sdk.NewInt(10), DelegatorShares: sdk.NewDec(10)}
	b.Validators[val.String()] = skt.Validator{Tokens: sdk.NewInt(9), DelegatorShares: sdk.NewDec(9)}
	for _, v := range []struct {
		owner         sdk.AccAddress
		before, after int64
	}{{owner1, 5, 4}, {owner2, 5, 5}} {
		key := v.owner.String() + "/" + val.String()
		a.Delegations[key] = skt.Delegation{DelegatorAddress: v.owner, ValidatorAddress: val, Shares: sdk.NewDec(v.before)}
		b.Delegations[key] = skt.Delegation{DelegatorAddress: v.owner, ValidatorAddress: val, Shares: sdk.NewDec(v.after)}
	}
	facts := exactAssetChanges(a, b)
	if len(facts) != 2 {
		t.Fatal("unchanged-value owner's native share context dropped")
	}
	positive := 0
	for _, fact := range facts {
		loss, ok := new(big.Rat).SetString(fact.Numerator + "/" + fact.Denominator)
		if !ok {
			t.Fatal("fraction")
		}
		if loss.Sign() > 0 {
			positive++
			if fact.Delegator != owner1.String() || loss.Cmp(big.NewRat(1, 1)) != 0 {
				t.Fatal("changing-share loss attributed incorrectly")
			}
		}
	}
	if positive != 1 {
		t.Fatal("changing-share positive loss count")
	}
}
func TestDeletedAndNewOwnershipStatesNotDropped(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	val := sdk.ValAddress(make([]byte, 20))
	old := sdk.AccAddress(append([]byte{1}, make([]byte, 19)...))
	next := sdk.AccAddress(append([]byte{2}, make([]byte, 19)...))
	a.Validators[val.String()] = skt.Validator{Tokens: sdk.NewInt(10), DelegatorShares: sdk.NewDec(10)}
	b.Validators[val.String()] = skt.Validator{Tokens: sdk.NewInt(9), DelegatorShares: sdk.NewDec(9)}
	a.Delegations[old.String()+"/"+val.String()] = skt.Delegation{DelegatorAddress: old, ValidatorAddress: val, Shares: sdk.NewDec(10)}
	b.Delegations[next.String()+"/"+val.String()] = skt.Delegation{DelegatorAddress: next, ValidatorAddress: val, Shares: sdk.NewDec(9)}
	facts := exactAssetChanges(a, b)
	if len(facts) != 2 {
		t.Fatal("opening or closing owner dropped")
	}
}
func TestUnbondingEntryBurnRecordedWithoutDelegationShares(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	val := sdk.ValAddress(make([]byte, 20))
	owner := sdk.AccAddress(append([]byte{1}, make([]byte, 19)...))
	entry := skt.UnbondingDelegationEntry{CreationHeight: 10, CompletionTime: time.Unix(100, 0), InitialBalance: sdk.NewInt(7), Balance: sdk.NewInt(7)}
	a.Unbondings = []skt.UnbondingDelegation{{DelegatorAddress: owner, ValidatorAddress: val, Entries: []skt.UnbondingDelegationEntry{entry}}}
	entry.Balance = sdk.NewInt(5)
	b.Unbondings = []skt.UnbondingDelegation{{DelegatorAddress: owner, ValidatorAddress: val, Entries: []skt.UnbondingDelegationEntry{entry}}}
	facts := exactAssetChanges(a, b)
	if len(facts) != 1 || facts[0].Numerator != "2" || facts[0].Denominator != "1" {
		t.Fatal("unbonding burn omitted")
	}
	refused(t, func() { unchangedShareReductions(a, b) })
}
func TestUnknownShareCoverageRefusesRatherThanResidualPlug(t *testing.T) {
	a := zeroFinancial()
	val := sdk.ValAddress(make([]byte, 20))
	a.Validators[val.String()] = skt.Validator{Tokens: sdk.NewInt(1), DelegatorShares: sdk.OneDec()}
	refused(t, func() { ownershipState(a) })
}
