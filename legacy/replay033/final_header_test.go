package main

import (
	"fmt"
	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/types"
	"testing"
)

func TestFinalHeaderEveryBinding(t *testing.T) {
	for _, name := range []string{"equal", "empty_results", "app", "parent", "results", "validators", "nextvalidators", "proposer", "height", "capture", "absent"} {
		t.Run(name, func(t *testing.T) {
			v := types.NewValidator(ed25519.PubKeyEd25519{}, 1)
			current := types.NewValidatorSet([]*types.Validator{v})
			next := current.Copy()
			c := cached{height: 11, block: &types.Block{Header: types.Header{Height: 11, AppHash: []byte{1}, LastBlockID: types.BlockID{Hash: []byte{2}}, LastResultsHash: []byte{3}, ValidatorsHash: current.Hash(), NextValidatorsHash: next.Hash(), ProposerAddress: v.Address}}}
			results := []byte{3}
			switch name {
			case "empty_results":
				results = nil
				c.block.LastResultsHash = nil
			case "app":
				c.block.AppHash = []byte{4}
			case "parent":
				c.block.LastBlockID.Hash = []byte{4}
			case "results":
				c.block.LastResultsHash = []byte{4}
			case "validators":
				c.block.ValidatorsHash = []byte{4}
			case "nextvalidators":
				c.block.NextValidatorsHash = []byte{4}
			case "proposer":
				c.block.ProposerAddress = []byte{4}
			case "height":
				c.block.Height = 12
			case "capture":
				c.err = fmt.Errorf("invalid")
			case "absent":
				c.block = nil
			}
			e := verifyFinalHeader(c, 10, []byte{1}, []byte{2}, results, current, next)
			good := name == "equal" || name == "empty_results"
			if (e == nil) != good {
				t.Fatal("wrong final-header decision")
			}
		})
	}
}
