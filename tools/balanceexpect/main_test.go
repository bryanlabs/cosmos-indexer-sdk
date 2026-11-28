package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFromGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "genesis.json")
	doc := `{"initial_height":"5200791","app_state":{
"bank":{"balances":[{"address":"cosmos1a","coins":[{"denom":"uatom","amount":"10"}]},{"address":"cosmos1b","coins":[{"denom":"uatom","amount":"7"}]}]},
"staking":{"validators":[{"operator_address":"cosmosvaloper1v","tokens":"999","delegator_shares":"1000.000000000000000000"}],
"delegations":[{"delegator_address":"cosmos1a","validator_address":"cosmosvaloper1v","shares":"500.000000000000000000"},{"delegator_address":"cosmos1a","validator_address":"cosmosvaloper1v","shares":"1.000000000000000000"}],
"unbonding_delegations":[{"delegator_address":"cosmos1a","validator_address":"cosmosvaloper1v","entries":[{"balance":"3"},{"balance":"4"}]}]}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	h, got, err := fromGenesis(path, map[string]bool{"cosmos1a": true, "cosmos1c": true})
	if err != nil {
		t.Fatal(err)
	}
	if h != 5200790 {
		t.Fatalf("height %d", h)
	}
	a := got["cosmos1a"]
	// 500 shares at 999/1000 is 499.5, truncated; 1 share is 0.999, truncated.
	if a.Liquid["uatom"] != "10" || a.Delegations["cosmosvaloper1v"] != "499" || a.Unbonding["cosmosvaloper1v"] != "7" {
		t.Fatalf("wallet a %+v", a)
	}
	if _, ok := got["cosmos1b"]; ok {
		t.Fatal("unwanted wallet read")
	}
	if c := got["cosmos1c"]; len(c.Liquid) != 0 || c.Height != 5200790 {
		t.Fatalf("empty wallet %+v", c)
	}
}
