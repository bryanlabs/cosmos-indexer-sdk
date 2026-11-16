package main

import (
	"bytes"
	"fmt"
	"github.com/tendermint/tendermint/types"
)

// The caller has just committed the target and advanced validator history.
// This only reads the following cached header. It never executes its block,
// changes application state, or emits an extra completed-height marker.
func verifyFinalHeader(c cached, height int64, appHash, parent, results []byte, current, next *types.ValidatorSet) error {
	if c.err != nil || c.block == nil || c.height != height+1 || c.block.Height != height+1 {
		return fmt.Errorf("next cached block unavailable or invalid")
	}
	b := c.block
	if len(appHash) == 0 || !bytes.Equal(b.LastBlockID.Hash, parent) || !bytes.Equal(b.AppHash, appHash) || !bytes.Equal(b.LastResultsHash, results) {
		return fmt.Errorf("final target state, parent or results differ from next header")
	}
	if current == nil || next == nil || !bytes.Equal(b.ValidatorsHash, current.Hash()) || !bytes.Equal(b.NextValidatorsHash, next.Hash()) {
		return fmt.Errorf("next header validator set differs")
	}
	if _, v := current.GetByAddress(b.ProposerAddress); v == nil {
		return fmt.Errorf("next header proposer is not in the validator set")
	}
	return nil
}
