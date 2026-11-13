package main

import (
	"encoding/base64"
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"testing"
	"time"
)

func fixture() snapshot {
	a := sdk.AccAddress(make([]byte, 20))
	v := sdk.ValAddress(make([]byte, 20))
	return snapshot{Validators: []staking.Validator{{OperatorAddress: v, Tokens: sdk.NewInt(100), DelegatorShares: sdk.NewDec(100), Status: sdk.Bonded}}, Delegations: []staking.Delegation{{DelegatorAddress: a, ValidatorAddress: v, Shares: sdk.NewDec(10)}}, Unbondings: []staking.UnbondingDelegation{{DelegatorAddress: a, ValidatorAddress: v, Entries: []staking.UnbondingDelegationEntry{{CreationHeight: 17, CompletionTime: time.Unix(10, 0).UTC(), InitialBalance: sdk.NewInt(5), Balance: sdk.NewInt(4)}}}}}
}
func TestCompare(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*snapshot)
		bad    bool
	}{
		{"equal", func(s *snapshot) {}, false},
		{"changed shares", func(s *snapshot) { s.Delegations[0].Shares = sdk.NewDec(9) }, true},
		{"changed rate", func(s *snapshot) { s.Validators[0].Tokens = sdk.NewInt(99) }, true},
		{"missing delegation", func(s *snapshot) { s.Delegations = nil }, true},
		{"extra delegation", func(s *snapshot) {
			d := s.Delegations[0]
			d.DelegatorAddress = append(sdk.AccAddress(nil), d.DelegatorAddress...)
			d.DelegatorAddress[0] = 1
			s.Delegations = append(s.Delegations, d)
		}, true},
		{"changed unbond balance", func(s *snapshot) { s.Unbondings[0].Entries[0].Balance = sdk.NewInt(3) }, true},
		{"changed completion", func(s *snapshot) { s.Unbondings[0].Entries[0].CompletionTime = time.Unix(11, 0) }, true},
		{"changed creation height", func(s *snapshot) { s.Unbondings[0].Entries[0].CreationHeight = 2 }, true},
		{"missing unbond", func(s *snapshot) { s.Unbondings = nil }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := fixture(), fixture()
			a.Unbondings[0].Entries[0].CreationHeight = 17
			tc.mutate(&b)
			r, e := compare(a, b)
			if e != nil {
				t.Fatal(e)
			}
			bad := r.ValidatorDifferences+r.DelegationDifferences+r.UnbondingDifferences > 0
			if bad != tc.bad {
				t.Fatalf("mismatch classification: %+v", r)
			}
		})
	}
}
func TestDuplicateRefused(t *testing.T) {
	a, b := fixture(), fixture()
	b.Delegations = append(b.Delegations, b.Delegations[0])
	if _, err := compare(a, b); err == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestSubspaceRefusesWrongHeightPrefixAndDuplicates(t *testing.T) {
	mk := func(height string, p []sdk.KVPair) []byte {
		v := codec037().MustMarshalBinaryLengthPrefixed(p)
		b, _ := json.Marshal(map[string]interface{}{"result": map[string]interface{}{"response": map[string]interface{}{"height": height, "code": 0, "value": base64.StdEncoding.EncodeToString(v)}}})
		return b
	}
	p := sdk.KVPair{Key: []byte{0x31, 1}, Value: []byte{1}}
	if _, e := decodeReply(mk("5200790", []sdk.KVPair{p}), []byte{0x31}); e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{mk("5200791", []sdk.KVPair{p}), mk("5200790", []sdk.KVPair{p, p}), mk("5200790", []sdk.KVPair{{Key: []byte{0x32}}})} {
		if _, e := decodeReply(b, []byte{0x31}); e == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
}
