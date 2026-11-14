package main

import (
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/staking"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	"testing"
)

func refused(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("negative control accepted")
		}
	}()
	f()
}
func raw(v interface{}) json.RawMessage { b, e := json.Marshal(v); must(e); return b }
func blank() patch {
	p := patch{}
	for _, name := range names {
		p[name] = []pair{}
	}
	return p
}
func TestAllNineAndExplicitBeforeImages(t *testing.T) {
	n := newNative()
	p := blank()
	p["acc"] = []pair{{Key: []byte{255, 0}, Before: raw(nil), Value: raw([]byte{})}}
	n.apply(p)
	need(n.cms.GetKVStore(n.keys["acc"]).Has([]byte{255, 0}), "present empty unknown key lost")
	q := blank()
	q["acc"] = []pair{{Key: []byte{255, 0}, Before: raw(nil), Value: raw([]byte{1})}}
	refused(t, func() { n.apply(q) })
	q["acc"][0].Before = raw([]byte{9})
	refused(t, func() { n.apply(q) })
	q["acc"][0].Before = nil
	refused(t, func() { n.apply(q) })
	q["acc"][0].Before = raw([]byte{})
	q["acc"][0].Value = nil
	refused(t, func() { n.apply(q) })
	q = blank()
	delete(q, "params")
	refused(t, func() { n.apply(q) })
	q = blank()
	delete(q, "params")
	q["unknown"] = []pair{}
	refused(t, func() { n.apply(q) })
	q = blank()
	q["acc"] = []pair{{Key: []byte{255, 0}, Before: raw([]byte{}), Value: raw(nil)}}
	n.apply(q)
	need(!n.cms.GetKVStore(n.keys["acc"]).Has([]byte{255, 0}), "native delete lost")
}
func TestNativePatchOrder(t *testing.T) {
	for _, keys := range [][][]byte{{{2}, {1}}, {{1}, {1}}, {{}}} {
		p := blank()
		for _, key := range keys {
			p["acc"] = append(p["acc"], pair{Key: key, Before: raw(nil), Value: raw([]byte{1})})
		}
		refused(t, func() { newNative().apply(p) })
	}
}
func zeroFinancial() financial {
	return financial{Bank: map[string]sdk.Int{}, Fees: sdk.Coins{}, Pool: skt.Pool{NotBondedTokens: sdk.ZeroInt(), BondedTokens: sdk.ZeroInt()}, Minter: mint.Minter{Inflation: sdk.ZeroDec(), AnnualProvisions: sdk.ZeroDec()}, Validators: map[string]skt.Validator{}, Delegations: map[string]skt.Delegation{}}
}
func TestPaidFeeNativeCollectionNotSignedIntent(t *testing.T) {
	a, b := zeroFinancial(), zeroFinancial()
	a.Bank["payer/uatom"] = sdk.NewInt(100)
	b.Bank["payer/uatom"] = sdk.NewInt(90)
	b.Fees = sdk.Coins{sdk.NewInt64Coin("uatom", 10)}
	r := request{Height: 7, Tx: 0, Message: -1, Phase: "ante", Status: "committed", Rows: []record{{Kind: "movement", Height: 7, Tx: 0, Message: -1, Phase: "ante", Status: "committed", Label: "ante_fee", Address: "payer", Denom: "uatom", Amount: "-10", Unit: "native_bank_tokens"}}}
	financialCheck(r, a, b)
	r.Rows[0].Amount = "-11"
	refused(t, func() { financialCheck(r, a, b) })
	r.Rows[0].Amount = "-10"
	b.Fees = sdk.Coins{}
	refused(t, func() { financialCheck(r, a, b) })
	b.Fees = sdk.Coins{sdk.NewInt64Coin("uatom", 10)}
	r.Rows[0].Amount = "10"
	refused(t, func() { financialCheck(r, a, b) })
}
func TestPreAnteFailureCannotInventEffects(t *testing.T) {
	marker := record{Kind: "accounting", Label: "ante_not_invoked", Height: 9, Tx: 0, Message: -1, Phase: "ante", Status: "aborted", Code: 11}
	original := []byte{1, 2, 3}
	r := request{Height: 9, Tx: 0, Message: -1, Phase: "transaction", Changes: blank(), Transactions: [][]byte{original}, UninvokedAnte: []record{marker}, Rows: []record{{Kind: "native_execution", Label: "native_abci", Height: 9, Tx: 0, Message: -1, Phase: "transaction", Status: "committed", Code: 11, Before: raw(original), After: raw(map[string]interface{}{"code": 11})}}}
	preAnteFailure(r)
	r.Rows[0].Code = 12
	refused(t, func() { preAnteFailure(r) })
	r.Rows[0].Code = 11
	r.Rows[0].Kind = "movement"
	refused(t, func() { preAnteFailure(r) })
	r.Rows[0].Kind = "native_execution"
	r.UninvokedAnte[0].Status = "committed"
	refused(t, func() { preAnteFailure(r) })
	r.UninvokedAnte[0].Status = "aborted"
	r.Changes["acc"] = []pair{{Key: []byte{1}, Before: raw(nil), Value: raw([]byte{2})}}
	refused(t, func() { preAnteFailure(r) })
}

func TestSignedZeroFeeVectorAndPayerBinding(t *testing.T) {
	del := sdk.AccAddress(make([]byte, 20))
	val := sdk.ValAddress(make([]byte, 20))
	a, b := zeroFinancial(), zeroFinancial()
	makeRequest := func(fees sdk.Coins) request {
		tx := auth.NewStdTx([]sdk.Msg{staking.NewMsgDelegate(del, val, sdk.NewInt64Coin("uatom", 1))}, auth.StdFee{Amount: fees}, nil, "")
		data, e := codec.MarshalBinaryLengthPrefixed(tx)
		must(e)
		return request{Tx: 0, Phase: "ante", Status: "committed", Transactions: [][]byte{data}}
	}
	signedAnte(makeRequest(sdk.Coins{{Denom: "uatom", Amount: sdk.ZeroInt()}}), a, b)
	signedAnte(makeRequest(sdk.Coins{}), a, b)
	r := makeRequest(sdk.Coins{sdk.NewInt64Coin("uatom", 10)})
	refused(t, func() { signedAnte(r, a, b) })
	a.Bank[del.String()+"/uatom"] = sdk.NewInt(100)
	b.Bank[del.String()+"/uatom"] = sdk.NewInt(90)
	b.Fees = sdk.Coins{sdk.NewInt64Coin("uatom", 10)}
	signedAnte(r, a, b)
	refused(t, func() { signedAnte(makeRequest(sdk.Coins{{Denom: "uatom", Amount: sdk.ZeroInt()}}), a, b) })
	b.Fees = sdk.Coins{sdk.NewInt64Coin("uatom", 9)}
	refused(t, func() { signedAnte(r, a, b) })
	b.Fees = sdk.Coins{sdk.NewInt64Coin("uatom", 10)}
	b.Bank = map[string]sdk.Int{"wrong-payer/uatom": sdk.NewInt(-10)}
	refused(t, func() { signedAnte(r, a, b) })
}

func TestOriginalNativeStakingRequestBinding(t *testing.T) {
	del := sdk.AccAddress(make([]byte, 20))
	val := sdk.ValAddress(make([]byte, 20))
	coin := sdk.NewInt64Coin("uatom", 10)
	tx := auth.NewStdTx([]sdk.Msg{staking.NewMsgDelegate(del, val, coin)}, auth.StdFee{}, nil, "")
	data, e := codec.MarshalBinaryLengthPrefixed(tx)
	must(e)
	row := record{Kind: "principal_request", Label: "delegate", Validator: val.String(), Delegator: del.String(), Address: del.String(), Denom: "uatom", Amount: "10", Unit: "native_bond_tokens", Before: raw(coin)}
	r := request{Tx: 0, Message: 0, Transactions: [][]byte{data}}
	signedStakingBinding(r, row)
	bad := row
	bad.Amount = "11"
	refused(t, func() { signedStakingBinding(r, bad) })
	bad = row
	bad.Unit = "delegation_shares"
	refused(t, func() { signedStakingBinding(r, bad) })
	bad = row
	bad.Before = raw(sdk.NewInt64Coin("uatom", 11))
	refused(t, func() { signedStakingBinding(r, bad) })
	tx = auth.NewStdTx([]sdk.Msg{staking.NewMsgUndelegate(del, val, sdk.NewDec(3))}, auth.StdFee{}, nil, "")
	data, e = codec.MarshalBinaryLengthPrefixed(tx)
	must(e)
	r.Transactions = [][]byte{data}
	row.Kind = "share_request"
	row.Label = "undelegate:requested_source_shares"
	row.Unit = "delegation_shares"
	row.Denom = ""
	row.Amount = sdk.NewDec(3).String()
	row.Before = raw(sdk.NewDec(3))
	signedStakingBinding(r, row)
	row.Amount = sdk.NewDec(4).String()
	refused(t, func() { signedStakingBinding(r, row) })
}

func TestExactNativePrincipalAndShares(t *testing.T) {
	addr := sdk.ValAddress(make([]byte, 20))
	del := sdk.AccAddress(make([]byte, 20))
	a, b := zeroFinancial(), zeroFinancial()
	va := skt.NewValidator(addr, secp256k1.GenPrivKeySecp256k1([]byte("native-consumer-test")).PubKey(), skt.Description{})
	va.Tokens = sdk.NewInt(100)
	va.DelegatorShares = sdk.NewDec(100)
	vb := va
	vb.Tokens = sdk.NewInt(90)
	vb.DelegatorShares = sdk.NewDec(90)
	da := skt.Delegation{DelegatorAddress: del, ValidatorAddress: addr, Shares: sdk.NewDec(100)}
	db := da
	db.Shares = sdk.NewDec(90)
	a.Validators[addr.String()] = va
	b.Validators[addr.String()] = vb
	a.Delegations[del.String()+"/"+addr.String()] = da
	b.Delegations[del.String()+"/"+addr.String()] = db
	row := record{Kind: "principal", Height: 8, Tx: 0, Message: 0, Phase: "message", Status: "committed", Validator: addr.String(), Delegator: del.String(), Unit: "native_bond_tokens", Amount: "10"}
	row.Before = raw(stakeObject(a, row))
	row.After = raw(stakeObject(b, row))
	r := request{Height: 8, Tx: 0, Message: 0, Phase: "message", Status: "committed", Rows: []record{row}}
	financialCheck(r, a, b)
	r.Rows[0].Amount = "11"
	refused(t, func() { financialCheck(r, a, b) })
	r.Rows[0].Kind = "staking_shares"
	r.Rows[0].Unit = "delegation_shares"
	r.Rows[0].Amount = "-10.000000000000000000"
	financialCheck(r, a, b)
	r.Rows[0].Amount = "-9.000000000000000000"
	refused(t, func() { financialCheck(r, a, b) })
	r.Rows[0].Amount = "-10.000000000000000000"
	r.Rows[0].After = raw(stakeObject(a, row))
	refused(t, func() { financialCheck(r, a, b) })
}
