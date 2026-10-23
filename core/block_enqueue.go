package core

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/cosmos-indexer/util"
	"github.com/DefiantLabs/probe/client"
	"gorm.io/gorm"
)

var EnqueueFunctions = map[string]func(chan int64) error{}

type EnqueueData struct {
	Height            int64
	IndexBlockEvents  bool
	IndexTransactions bool
}

func GenerateBlockFileEnqueueFunction(db *gorm.DB, cfg config.IndexConfig, client *client.ChainClient, chainID uint, blockInputFile string) (func(chan *EnqueueData) error, error) {
	return func(blockChan chan *EnqueueData) error {
		plan, err := os.ReadFile(blockInputFile)
		if err != nil {
			config.Log.Errorf("Error reading block input file. Err: %v", err)
			return err
		}
		var blocksToIndex []uint64
		err = json.Unmarshal(plan, &blocksToIndex)
		if err != nil {
			errString := err.Error()

			switch {
			case errString == "json: cannot unmarshal string into Go value of type int":
				config.Log.Errorf("Error parsing block input file. Err: Found non-integer value in block array")
				return err
			case errString == "cannot unmarshal object into Go value of type []uint64":
				config.Log.Errorf("Error parsing block input file. Err: Found object that could not be parsed into an array of integers")
				return err
			case strings.Contains(errString, "cannot unmarshal number"):
				config.Log.Errorf("Error parsing block input file. Err: Found number that could not be parsed into Go unsigned integer")
				return err
			default:
				config.Log.Errorf("Error parsing block input file. Err: %v", err)
				return err
			}
		}

		// sort the data array
		blocksToIndex = util.RemoveDuplicatesFromUint64Slice(blocksToIndex)
		sort.Slice(blocksToIndex, func(i, j int) bool { return blocksToIndex[i] < blocksToIndex[j] })

		// Get latest block height and check to see if we are trying to index blocks outside range
		earliestBlock, latestBlock, err := rpc.GetEarliestAndLatestBlockHeights(client)
		if err != nil {
			config.Log.Fatal("Error getting blockchain latest height. Err: %v", err)
		}

		unindexableBlockHeights := []uint64{}
		blockInRange := []uint64{}
		for _, block := range blocksToIndex {
			if block > uint64(latestBlock) || block < uint64(earliestBlock) {
				unindexableBlockHeights = append(unindexableBlockHeights, block)
			} else {
				blockInRange = append(blockInRange, block)
			}
		}

		if len(unindexableBlockHeights) != 0 {
			config.Log.Warnf("The following blocks are past the blockchain earliest height (%d) and latest height (%d) and will be skipped: %v", earliestBlock, latestBlock, unindexableBlockHeights)
		}

		if len(blockInRange) == 0 {
			config.Log.Infof("No blocks to index within blockchain earliest height (%d) and latest height (%d), exiting", earliestBlock, latestBlock)
			return nil
		}

		// Add jobs to the queue to be processed
		for _, height := range blockInRange {
			if cfg.Base.Throttling != 0 {
				time.Sleep(time.Second * time.Duration(cfg.Base.Throttling))
			}
			config.Log.Debugf("Sending block %v to be indexed.", height)
			// Add the new block to the queue
			blockChan <- &EnqueueData{
				IndexBlockEvents:  cfg.Base.BlockEventIndexingEnabled,
				IndexTransactions: cfg.Base.TransactionIndexingEnabled,
				Height:            int64(height),
			}
		}
		return nil
	}, nil
}

func GenerateMsgTypeEnqueueFunction(db *gorm.DB, cfg config.IndexConfig, chainID uint, msgType string) (func(chan *EnqueueData) error, error) {
	// get the block range
	startBlock := cfg.Base.StartBlock
	endBlock := cfg.Base.EndBlock
	if endBlock == -1 {
		heighestBlock := dbTypes.GetHighestIndexedBlock(db, chainID)
		endBlock = heighestBlock.Height
	}

	rows, err := db.Raw(`SELECT height FROM blocks
							JOIN txes ON txes.block_id = blocks.id
							JOIN messages ON messages.tx_id = txes.id
							JOIN message_types ON message_types.id = messages.message_type_id
							AND message_types.message_type = ?
							WHERE height >= ? AND height <= ? AND chain_id = ?::int;
							`, msgType, startBlock, endBlock, chainID).Rows()
	if err != nil {
		config.Log.Errorf("Error checking DB for blocks to reindex. Err: %v", err)
		return nil, err
	}

	return func(blockChan chan *EnqueueData) error {
		defer rows.Close()
		for rows.Next() {
			var block int64
			err = db.ScanRows(rows, &block)
			if err != nil {
				config.Log.Fatal("Error getting block height. Err: %v", err)
			}
			config.Log.Debugf("Sending block %v to be re-indexed.", block)

			if cfg.Base.Throttling != 0 {
				time.Sleep(time.Second * time.Duration(cfg.Base.Throttling))
			}

			// Add the new block to the queue
			blockChan <- &EnqueueData{
				IndexBlockEvents:  cfg.Base.BlockEventIndexingEnabled,
				IndexTransactions: cfg.Base.TransactionIndexingEnabled,
				Height:            block,
			}
		}

		return nil
	}, nil
}

// The default enqueue function will enqueue blocks according to the configuration passed in. It has a few default cases detailed here:
// Based on whether transaction indexing or block event indexing are enabled, it will choose a start block based on passed in config values.
// If reindexing is disabled, it will not reindex blocks that have already been indexed. This means it may skip around finding blocks that have not been
// indexed according to the current configuration.
// If failed block reattempts are enabled, it will enqueue those according to the passed in configuration as well.
func GenerateDefaultEnqueueFunction(db *gorm.DB, cfg config.IndexConfig, client *client.ChainClient, chainID uint) (func(chan *EnqueueData) error, error) {
	var failedBlockEnqueueData []*EnqueueData
	if cfg.Base.ReattemptFailedBlocks {
		var failedEventBlocks []models.FailedEventBlock
		var failedBlocks []models.FailedBlock

		uniqueBlockFailures := make(map[int64]*EnqueueData)
		if cfg.Base.BlockEventIndexingEnabled {
			err := db.Table("failed_event_blocks").Where("blockchain_id = ?::int", chainID).Order("height asc").Scan(&failedEventBlocks).Error
			if err != nil {
				config.Log.Error("Error retrieving failed event blocks for reenqueue", err)
				return nil, err
			}
		}

		if cfg.Base.TransactionIndexingEnabled {
			err := db.Table("failed_blocks").Where("blockchain_id = ?::int", chainID).Order("height asc").Scan(&failedBlocks).Error
			if err != nil {
				config.Log.Error("Error retrieving failed blocks for reenqueue", err)
				return nil, err
			}
		}

		for _, failedEventBlock := range failedEventBlocks {
			uniqueBlockFailures[failedEventBlock.Height] = &EnqueueData{
				Height:            failedEventBlock.Height,
				IndexBlockEvents:  true,
				IndexTransactions: false,
			}
		}

		for _, failedBlock := range failedBlocks {
			if _, ok := uniqueBlockFailures[failedBlock.Height]; ok {
				uniqueBlockFailures[failedBlock.Height].IndexTransactions = true
			} else {
				uniqueBlockFailures[failedBlock.Height] = &EnqueueData{
					Height:            failedBlock.Height,
					IndexBlockEvents:  false,
					IndexTransactions: true,
				}
			}
		}

		for _, block := range uniqueBlockFailures {
			failedBlockEnqueueData = append(failedBlockEnqueueData, block)
		}

		sort.Slice(failedBlockEnqueueData, func(i, j int) bool { return failedBlockEnqueueData[i].Height < failedBlockEnqueueData[j].Height })
	}

	startBlock := cfg.Base.StartBlock
	endBlock := cfg.Base.EndBlock
	var latestBlock int64 = math.MaxInt64
	reindexing := cfg.Base.ReIndex

	if startBlock <= 0 {
		startBlock = 1
	}

	if !reindexing {
		config.Log.Info("Reindexing is disabled, skipping blocks that have already been indexed")
	} else {
		config.Log.Info("Reindexing is enabled starting from initial start height")
	}

	return func(blockChan chan *EnqueueData) error {
		window := indexedBlockWindow{load: func(start, end int64) ([]dbTypes.BlockIndexState, error) {
			return dbTypes.GetBlockIndexWindow(db, chainID, start, end)
		}}

		if len(failedBlockEnqueueData) > 0 && cfg.Base.ReattemptFailedBlocks {
			config.Log.Info("Re-enqueuing failed blocks")
			for _, block := range failedBlockEnqueueData {

				switch {
				case block.IndexBlockEvents && block.IndexTransactions:
					config.Log.Infof("Re-attempting failed block %v for both block events and transactions", block.Height)
				case block.IndexBlockEvents:
					config.Log.Infof("Re-attempting failed block: %v for block events", block.Height)
				case block.IndexTransactions:
					config.Log.Infof("Re-attempting failed block: %v for transactions", block.Height)
				}

				if block.IndexBlockEvents || block.IndexTransactions {
					blockChan <- block
					if cfg.Base.Throttling != 0 {
						time.Sleep(time.Second * time.Duration(cfg.Base.Throttling))
					}
				}
			}
			config.Log.Info("All failed blocks have been re-enqueued for processing")
		} else if cfg.Base.ReattemptFailedBlocks {
			config.Log.Info("No failed blocks to re-enqueue")
		}

		currBlock := startBlock

		for {
			// The program is configured to stop running after a set block height.
			// Generally this will only be done while debugging or if a particular block was incorrectly processed.
			if endBlock != -1 && currBlock > endBlock {
				config.Log.Info("Hit the last block we're allowed to index, exiting enqueue func.")
				return nil
			} else if cfg.Base.ExitWhenCaughtUp && currBlock > latestBlock {
				config.Log.Info("Hit the last block we're allowed to index, exiting enqueue func.")
				return nil
			}

			// The job queue is running out of jobs to process, see if the blockchain has produced any new blocks we haven't indexed yet.
			if len(blockChan) <= cap(blockChan)/4 {
				// This is the latest block height available on the Node.

				var err error
				latestBlock, err = rpc.GetLatestBlockHeightWithRetry(client, cfg.Base.RequestRetryAttempts, cfg.Base.RequestRetryMaxWait)
				if err != nil {
					config.Log.Error("Error getting blockchain latest height. Err: %v", err)
					return err
				}

				// Throttling in case of hitting public APIs
				if cfg.Base.Throttling != 0 {
					time.Sleep(time.Second * time.Duration(cfg.Base.Throttling))
				}

				// Already at the latest block, wait for the next block to be available.
				for currBlock < latestBlock && (currBlock <= endBlock || endBlock == -1) && len(blockChan) != cap(blockChan) {
					var block dbTypes.BlockIndexState
					var blockExists bool
					if !reindexing {
						last := latestBlock - 1
						if endBlock != -1 && endBlock < last {
							last = endBlock
						}
						block, blockExists, err = window.lookup(currBlock, last)
						if err != nil {
							return err
						}
					}
					work := blockIndexWork(currBlock, block, blockExists, cfg.Base.BlockEventIndexingEnabled, cfg.Base.TransactionIndexingEnabled)
					if work == nil {
						currBlock++
						continue
					}
					blockChan <- work
					currBlock++

					if cfg.Base.Throttling != 0 {
						time.Sleep(time.Second * time.Duration(cfg.Base.Throttling))
					}
				}
			}
		}
	}, nil
}
