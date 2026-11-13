package main

import (
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"testing"
)

func TestGenesisProtobufJSON(t *testing.T) {
	a := sdk.AccAddress(make([]byte, 20)).String()
	v := sdk.ValAddress(make([]byte, 20)).String()
	doc := map[string]interface{}{"chain_id": "cosmoshub-4", "app_state": map[string]interface{}{"staking": map[string]interface{}{
		"validators":            []interface{}{map[string]interface{}{"operator_address": v, "tokens": "100", "delegator_shares": "100.000000000000000000", "status": "BOND_STATUS_BONDED", "jailed": false, "consensus_pubkey": map[string]interface{}{"@type": "/cosmos.crypto.ed25519.PubKey", "key": "ignored"}}},
		"delegations":           []interface{}{map[string]interface{}{"delegator_address": a, "validator_address": v, "shares": "10.000000000000000000"}},
		"unbonding_delegations": []interface{}{map[string]interface{}{"delegator_address": a, "validator_address": v, "entries": []interface{}{map[string]interface{}{"creation_height": "0", "completion_time": "1970-01-01T00:00:10Z", "initial_balance": "5", "balance": "4"}}}},
	}}}
	raw, _ := json.Marshal(doc)
	s, _, err := decodeGenesis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Validators) != 1 || len(s.Delegations) != 1 || len(s.Unbondings) != 1 || s.Unbondings[0].Entries[0].CreationHeight != 0 {
		t.Fatal("protobuf JSON not fully decoded")
	}
	before := fixture()
	before.Unbondings[0].Entries[0].CreationHeight = 0
	r, err := compare(before, s)
	if err != nil || r.ValidatorDifferences+r.DelegationDifferences+r.UnbondingDifferences != 0 {
		t.Fatal("protobuf JSON state differs")
	}
}
func TestUniverseRejectsIncomplete(t *testing.T) {
	for _, s := range []string{`{}`, `{"chain":"cosmoshub-3","through":5200790,"heights_stored":5200789,"validators":[]}`} {
		if _, err := readUniverse([]byte(s)); err == nil {
			t.Fatal("incomplete universe accepted")
		}
	}
}
