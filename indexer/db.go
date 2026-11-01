package indexer

import (
	"fmt"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
)

// doDBUpdates will read the data out of the db data chan that had been processed by the workers
// if this is a dry run, we will simply empty the channel and track progress
// otherwise we will index the data in the DB.
// it will also read rewars data and index that.
func (indexer *Indexer) DoDBUpdates(wg *sync.WaitGroup, txDataChan chan *DBData, blockEventsDataChan chan *BlockEventsDBData, dbChainID uint) {
	blocksProcessed := 0
	dbWrites := 0
	dbReattempts := 0
	timeStart := time.Now()
	defer wg.Done()

	for {
		// break out of loop once all channels are fully consumed
		if txDataChan == nil && blockEventsDataChan == nil {
			config.Log.Info("DB updates complete")
			break
		}

		select {
		// read tx data from the data chan
		case data, ok := <-txDataChan:
			if !ok {
				txDataChan = nil
				continue
			}
			dbWrites++
			// While debugging we'll sometimes want to turn off INSERTS to the DB
			// Note that this does not turn off certain reads or DB connections.
			indexedBlock := data.block
			indexedDataset := data.txDBWrappers

			if !indexer.DryRun {
				var err error
				config.Log.Info(fmt.Sprintf("Indexing %v TXs from block %d", len(data.txDBWrappers), data.block.Height))
				indexedBlock, indexedDataset, err = dbTypes.IndexBlockData(indexer.DB, data.block, data.txDBWrappers, *indexer.Config, indexer.CustomMessageParserTrackers)
				if err != nil {
					// Do a single reattempt on failure
					dbReattempts++
					indexedBlock, indexedDataset, err = dbTypes.IndexBlockData(indexer.DB, data.block, data.txDBWrappers, *indexer.Config, indexer.CustomMessageParserTrackers)
					if err != nil {
						config.Log.Fatal(fmt.Sprintf("Error indexing block %v.", data.block.Height), err)
					}
				}

				config.Log.Info(fmt.Sprintf("Finished indexing %v TXs from block %d", len(data.txDBWrappers), data.block.Height))
			} else {
				config.Log.Info(fmt.Sprintf("Processing block %d (dry run, block data will not be stored in DB).", data.block.Height))
			}

			if indexer.PostIndexCustomMessageFunction != nil {
				config.Log.Info(fmt.Sprintf("Running PostIndexCustomMessageFunction for block %d", data.block.Height))

				dataset := &PostIndexCustomMessageDataset{
					Config:         *indexer.Config,
					DB:             indexer.DB,
					DryRun:         indexer.DryRun,
					IndexedDataset: &indexedDataset,
					MessageParser:  indexer.CustomMessageParserTrackers,
					IndexedBlock:   indexedBlock,
				}

				err := indexer.PostIndexCustomMessageFunction(dataset)
				if err != nil {
					config.Log.Fatal(fmt.Sprintf("Error running PostIndexCustomMessageFunction for block %d", data.block.Height), err)
				}
			}

			// Just measuring how many blocks/second we can process
			if indexer.Config.Base.BlockTimer > 0 {
				blocksProcessed++
				if blocksProcessed%int(indexer.Config.Base.BlockTimer) == 0 {
					totalTime := time.Since(timeStart)
					config.Log.Info(fmt.Sprintf("Processing %d blocks took %f seconds. %d total blocks have been processed.\n", indexer.Config.Base.BlockTimer, totalTime.Seconds(), blocksProcessed))
					timeStart = time.Now()
				}
				if float64(dbReattempts)/float64(dbWrites) > .1 {
					config.Log.Fatalf("More than 10%% of the last %v DB writes have failed.", dbWrites)
				}
			}
		case eventData, ok := <-blockEventsDataChan:
			if !ok {
				blockEventsDataChan = nil
				continue
			}
			dbWrites++
			numEvents := len(eventData.blockDBWrapper.BeginBlockEvents) + len(eventData.blockDBWrapper.EndBlockEvents)
			config.Log.Info(fmt.Sprintf("Indexing %v Block Events from block %d", numEvents, eventData.blockDBWrapper.Block.Height))
			identifierLoggingString := fmt.Sprintf("block %d", eventData.blockDBWrapper.Block.Height)

			indexedDataset, err := dbTypes.IndexBlockEvents(indexer.DB, indexer.DryRun, eventData.blockDBWrapper, identifierLoggingString)
			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Error indexing block events for %s.", identifierLoggingString), err)
			}

			err = dbTypes.IndexCustomBlockEvents(*indexer.Config, indexer.DB, indexer.DryRun, indexedDataset, identifierLoggingString, indexer.CustomBeginBlockParserTrackers, indexer.CustomEndBlockParserTrackers)
			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Error indexing custom block events for %s.", identifierLoggingString), err)
			}

			config.Log.Info(fmt.Sprintf("Finished indexing %v Block Events from block %d", numEvents, eventData.blockDBWrapper.Block.Height))
		}
	}
}
