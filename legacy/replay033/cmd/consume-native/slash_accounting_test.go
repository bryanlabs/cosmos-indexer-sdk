package main

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	"math/big"
	"testing"
)

func TestTransactionDrivenRemovalsCannotBecomeBeginSlashFacts(t *testing.T) {
 a,b:=zeroFinancial(),zeroFinancial();a.Provision=sdk.NewInt64Coin("uatom",3)
 a.Pool.BondedTokens=sdk.NewInt(10);b.Pool.BondedTokens=sdk.NewInt(9)
 for _,h:=range []int64{99677,168912,347236,347349} {for _,phase:=range []string{"message","transaction","ante","end_block"} {if requiresNativeBeginWitness(request{Height:h,Phase:phase},a,b){t.Fatal("non-BeginBlock state treated as slash")}}}
 if !requiresNativeBeginWitness(request{Height:219358,Phase:"begin_block"},a,b){t.Fatal("actual BeginBlock burn not witnessed")}
}

func TestExactSlashOwnershipRemainders(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	val := sdk.ValAddress(make([]byte, 20))
	v := skt.Validator{OperatorAddress: val, Tokens: sdk.NewInt(10), DelegatorShares: sdk.NewDec(3)}
	w := v
	w.Tokens = sdk.NewInt(9)
	a.Validators[val.String()] = v
	b.Validators[val.String()] = w
	for i := byte(1); i <= 3; i++ {
		raw := make([]byte, 20)
		raw[0] = i
		owner := sdk.AccAddress(raw)
		d := skt.Delegation{DelegatorAddress: owner, ValidatorAddress: val, Shares: sdk.OneDec()}
		key := owner.String() + "/" + val.String()
		a.Delegations[key] = d
		b.Delegations[key] = d
	}
	rows := unchangedShareReductions(a, b)
	if len(rows) != 3 {
		t.Fatal("ownership dropped")
	}
	total := new(big.Rat)
	for _, r := range rows {
		if r.Classification != "native_stake_asset_reduction_unclassified" {
			t.Fatal("tax classification fabricated")
		}
		f, ok := new(big.Rat).SetString(r.Numerator + "/" + r.Denominator)
		if !ok {
			t.Fatal("fraction")
		}
		total.Add(total, f)
		native, ok := new(big.Rat).SetString(r.NativeDecReduction)
		if !ok {
			t.Fatal("native decimal")
		}
		remainder, ok := new(big.Rat).SetString(r.RoundingRemainderNumerator + "/" + r.RoundingRemainderDenominator)
		if !ok || new(big.Rat).Add(native, remainder).Cmp(f) != 0 {
			t.Fatal("native rounding remainder not exact")
		}
	}
	if total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatal("rounding remainder lost")
	}
	delete(a.Delegations, rows[0].Delegator+"/"+val.String())
	delete(b.Delegations, rows[0].Delegator+"/"+val.String())
	refused(t, func() { unchangedShareReductions(a, b) })
}
func TestExactPoolWitnessRejectsArbitraryBondedFall(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	a.Provision = sdk.NewInt64Coin("uatom", 3)
	a.Pool.BondedTokens = sdk.NewInt(10)
	b.Pool.BondedTokens = sdk.NewInt(9)
	b.Pool.NotBondedTokens = sdk.NewInt(3)
	refused(t, func() { exactBeginPoolWitness(a, b, b) })
	addr := sdk.ValAddress(make([]byte, 20))
	val := addr.String()
	v := skt.NewValidator(addr, secp256k1.GenPrivKeySecp256k1([]byte("slash-test")).PubKey(), skt.Description{})
	v.Tokens = sdk.NewInt(10)
	a.Validators[val] = v
	v.Tokens = sdk.NewInt(9)
	b.Validators[val] = v
	exactBeginPoolWitness(a, b, b)
	different := b
	different.Pool.BondedTokens = sdk.NewInt(8)
	refused(t, func() { exactBeginPoolWitness(a, b, different) })
}
