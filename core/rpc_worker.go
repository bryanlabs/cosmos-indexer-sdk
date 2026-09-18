package core

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	txTypes "github.com/cosmos/cosmos-sdk/types/tx"
	"gorm.io/gorm"
)

// Wrapper types for gathering full dataset.
type IndexerBlockEventData struct {
	BlockData                *ctypes.ResultBlock
	BlockResultsData         *rpc.CustomBlockResults
	BlockEventRequestsFailed bool
	GetTxsResponse           *txTypes.GetTxsEventResponse
	TxRequestsFailed         bool
	IndexBlockEvents         bool
	IndexTransactions        bool
}

// This function is responsible for making all RPC requests to the chain needed for later processing.
// The indexer relies on a number of RPC endpoints for full block data, including block event and transaction searches.
func BlockRPCWorker(wg *sync.WaitGroup, blockEnqueueChan chan *EnqueueData, chainID uint, chainStringID string, cfg *config.IndexConfig, chainClient *client.ChainClient, db *gorm.DB, outputChannel chan IndexerBlockEventData) {
	defer wg.Done()
	rpcClient := rpc.URIClient{
		Address: chainClient.Config.RPCAddr,
		Client:  &http.Client{},
	}

	for {
		// Get the next block to process
		block, open := <-blockEnqueueChan
		if !open {
			config.Log.Debugf("Block enqueue channel closed. Exiting RPC worker.")
			break
		}

		currentHeightIndexerData := IndexerBlockEventData{
			BlockEventRequestsFailed: false,
			TxRequestsFailed:         false,
			IndexBlockEvents:         block.IndexBlockEvents,
			IndexTransactions:        block.IndexTransactions,
		}

		// Get the block from the RPC
		blockData, err := rpc.GetBlock(chainClient, block.Height)
		if err != nil {
			// This is the only response we continue on. If we can't get the block, we can't index anything.
			config.Log.Errorf("Error getting block %v from RPC. Err: %v", block, err)
			if upsertErr := dbTypes.UpsertFailedEventBlock(db, block.Height, chainStringID, cfg.Probe.ChainName); upsertErr != nil {
				config.Log.Fatal("Failed to insert failed block event", upsertErr)
			}
			if upsertErr := dbTypes.UpsertFailedBlock(db, block.Height, chainStringID, cfg.Probe.ChainName, err); upsertErr != nil {
				config.Log.Fatal("Failed to insert failed block", upsertErr)
			}
			continue
		}

		currentHeightIndexerData.BlockData = blockData

		if block.IndexBlockEvents {
			bresults, err := rpc.GetBlockResultWithRetry(rpcClient, block.Height, cfg.Base.RequestRetryAttempts, cfg.Base.RequestRetryMaxWait)

			if err != nil {
				config.Log.Errorf("Error getting block results for block %v from RPC. Err: %v", block, err)
				err := dbTypes.UpsertFailedEventBlock(db, block.Height, chainStringID, cfg.Probe.ChainName)
				if err != nil {
					config.Log.Fatal("Failed to insert failed block event", err)
				}
				currentHeightIndexerData.BlockResultsData = nil
				currentHeightIndexerData.BlockEventRequestsFailed = true
			} else {
				bresults, err = NormalizeCustomBlockResults(bresults)
				if err != nil {
					config.Log.Errorf("Error normalizing block results for block %v from RPC. Err: %v", block, err)
					err := dbTypes.UpsertFailedEventBlock(db, block.Height, chainStringID, cfg.Probe.ChainName)
					if err != nil {
						config.Log.Fatal("Failed to insert failed block event", err)
					}
				} else {
					currentHeightIndexerData.BlockResultsData = bresults
				}
			}
		}

		if block.IndexTransactions {
			var txsEventResp *txTypes.GetTxsEventResponse
			var err error
			if !cfg.Base.SkipBlockByHeightRPCRequest {
				txsEventResp, err = rpc.GetTxsByBlockHeight(chainClient, block.Height)
			}

			if err != nil || cfg.Base.SkipBlockByHeightRPCRequest {
				// Attempt to get block results to attempt an in-app codec decode of transactions.
				if currentHeightIndexerData.BlockResultsData == nil {

					bresults, err := rpc.GetBlockResultWithRetry(rpcClient, block.Height, cfg.Base.RequestRetryAttempts, cfg.Base.RequestRetryMaxWait)

					if err != nil {
						config.Log.Errorf("Error getting txs for block %v from RPC. Err: %v", block, err)
						blockErr := err
						if upsertErr := dbTypes.UpsertFailedBlock(db, block.Height, chainStringID, cfg.Probe.ChainName, blockErr); upsertErr != nil {
							config.Log.Fatal("Failed to insert failed block", upsertErr)
						}
						currentHeightIndexerData.GetTxsResponse = nil
						currentHeightIndexerData.BlockResultsData = nil
						// Only set failed when we can't get the block results either.
						currentHeightIndexerData.TxRequestsFailed = true
					} else {
						bresults, err = NormalizeCustomBlockResults(bresults)
						if err != nil {
							config.Log.Errorf("Error normalizing block results for block %v from RPC. Err: %v", block, err)
							normalizeErr := err
							if upsertErr := dbTypes.UpsertFailedBlock(db, block.Height, chainStringID, cfg.Probe.ChainName, normalizeErr); upsertErr != nil {
								config.Log.Fatal("Failed to insert failed block", upsertErr)
							}
						} else {
							currentHeightIndexerData.BlockResultsData = bresults
						}
					}

				}
			} else {
				currentHeightIndexerData.GetTxsResponse = txsEventResp
			}
		}

		outputChannel <- currentHeightIndexerData
	}
}

func NormalizeCustomBlockResults(blockResults *rpc.CustomBlockResults) (*rpc.CustomBlockResults, error) {
	if len(blockResults.FinalizeBlockEvents) != 0 {
		beginBlockEvents := []abci.Event{}
		endBlockEvents := []abci.Event{}

		for _, event := range blockResults.FinalizeBlockEvents {
			eventAttrs := []abci.EventAttribute{}
			isBeginBlock := false
			isEndBlock := false
			for _, attr := range event.Attributes {
				if attr.Key == "mode" {
					if attr.Value == "BeginBlock" {
						isBeginBlock = true
					} else if attr.Value == "EndBlock" {
						isEndBlock = true
					}
				} else {
					eventAttrs = append(eventAttrs, attr)
				}
			}

			switch {
			case isBeginBlock && isEndBlock:
				return nil, fmt.Errorf("finalize block event has both BeginBlock and EndBlock mode")
			case !isBeginBlock && !isEndBlock:
				return nil, fmt.Errorf("finalize block event has neither BeginBlock nor EndBlock mode")
			case isBeginBlock:
				beginBlockEvents = append(beginBlockEvents, abci.Event{Type: event.Type, Attributes: eventAttrs})
			case isEndBlock:
				endBlockEvents = append(endBlockEvents, abci.Event{Type: event.Type, Attributes: eventAttrs})
			}
		}

		blockResults.BeginBlockEvents = append(blockResults.BeginBlockEvents, beginBlockEvents...)
		blockResults.EndBlockEvents = append(blockResults.EndBlockEvents, endBlockEvents...)
	}

	return blockResults, nil
}
