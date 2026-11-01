package core

import (
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/rpc"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
)

// ValidateBlockTxResults fails closed before any block can be marked tx-indexed.
func ValidateBlockTxResults(block *ctypes.ResultBlock, results *rpc.CustomBlockResults) error {
	if block == nil || block.Block == nil || results == nil {
		return fmt.Errorf("missing block or transaction results")
	}
	if block.Block.Height != results.Height {
		return fmt.Errorf("block/result height mismatch: %d/%d", block.Block.Height, results.Height)
	}
	if len(block.Block.Txs) != len(results.TxsResults) {
		return fmt.Errorf("block transaction/result count mismatch: %d/%d", len(block.Block.Txs), len(results.TxsResults))
	}
	for i, result := range results.TxsResults {
		if result == nil {
			return fmt.Errorf("missing transaction result at index %d", i)
		}
	}
	return nil
}
