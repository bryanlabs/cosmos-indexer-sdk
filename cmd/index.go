package cmd

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/core"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	indexerPackage "github.com/DefiantLabs/cosmos-indexer/indexer"
	"github.com/DefiantLabs/cosmos-indexer/probe"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/spf13/cobra"
)

var (
	indexer        indexerPackage.Indexer
	oldHelpCommand func(cmd *cobra.Command, args []string)
)

func init() {
	indexer.Config = &config.IndexConfig{}
	config.SetupLogFlags(&indexer.Config.Log, indexCmd)
	config.SetupDatabaseFlags(&indexer.Config.Database, indexCmd)
	config.SetupProbeFlags(&indexer.Config.Probe, indexCmd)
	config.SetupThrottlingFlag(&indexer.Config.Base.Throttling, indexCmd)
	config.SetupIndexSpecificFlags(indexer.Config, indexCmd)

	oldHelpCommand = indexCmd.HelpFunc()
	indexCmd.SetHelpFunc(HelpOverride)
	rootCmd.AddCommand(indexCmd)
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Indexes the blockchain according to the configuration defined.",
	Long: `Indexes the Cosmos-based blockchain according to the configurations found on the command line
	or in the specified config file. Indexes taxable events into a database for easy querying. It is
	highly recommended to keep this command running as a background service to keep your index up to date.`,
	PreRunE: setupIndex,
	Run:     index,
}

func HelpOverride(cmd *cobra.Command, args []string) {
	oldHelpCommand(indexCmd, nil)
	safeCleanupSetupExit(&indexer)
}

// GetBuiltinIndexer returns the indexer instance for the index command. Usable for customizing pre-run setup.
func GetBuiltinIndexer() *indexerPackage.Indexer {
	if indexer.PostSetupDatasetChannel == nil {
		indexer.PostSetupDatasetChannel = make(chan *indexerPackage.PostSetupDataset, 1)
	}

	return &indexer
}

func safeCleanupSetupExit(indexer *indexerPackage.Indexer) {
	close(indexer.PostSetupDatasetChannel)

	if indexer.PreExitCustomFunction != nil {
		err := indexer.PreExitCustomFunction(&indexerPackage.PreExitCustomDataset{
			Config: *indexer.Config,
			DB:     indexer.DB,
			DryRun: indexer.DryRun,
		})
		if err != nil {
			config.Log.Fatal("Failed to run pre-exit custom function", err)
		}
	}
}

// setupIndex loads the configuration from file and command line flags, validates the configuration, and sets up the logger and database connection.
func setupIndex(cmd *cobra.Command, args []string) error {
	if indexer.PostSetupDatasetChannel == nil {
		indexer.PostSetupDatasetChannel = make(chan *indexerPackage.PostSetupDataset, 1)
	}

	BindFlags(cmd, viperConf)

	err := indexer.Config.Validate()
	if err != nil {
		safeCleanupSetupExit(&indexer)
		return err
	}

	ignoredKeys := config.CheckSuperfluousIndexKeys(viperConf.AllKeys())

	if len(ignoredKeys) > 0 {
		config.Log.Warnf("Warning, the following invalid keys will be ignored: %v", ignoredKeys)
	}

	setupLogger(indexer.Config.Log.Level, indexer.Config.Log.Path, indexer.Config.Log.Pretty)

	// 0 is an invalid starting block, set it to 1
	if indexer.Config.Base.StartBlock == 0 {
		indexer.Config.Base.StartBlock = 1
	}

	// If DB has not been preset, connect to the database and migrate using the default configuration settings
	if indexer.DB == nil {
		db, err := ConnectToDBAndMigrate(indexer.Config.Database)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Could not establish connection to the database", err)
		}

		indexer.DB = db
	} else {
		err = dbTypes.MigrateModels(indexer.DB)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Error running DB migrations", err)
		}
	}

	indexer.DryRun = indexer.Config.Base.Dry

	indexer.BlockEventFilterRegistries = indexerPackage.BlockEventFilterRegistries{
		BeginBlockEventFilterRegistry: &filter.StaticBlockEventFilterRegistry{},
		EndBlockEventFilterRegistry:   &filter.StaticBlockEventFilterRegistry{},
	}

	if indexer.Config.Base.FilterFile != "" {
		f, err := os.Open(indexer.Config.Base.FilterFile)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatalf("Failed to open block event filter file %s: %s", indexer.Config.Base.FilterFile, err)
		}

		b, err := io.ReadAll(f)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to parse block event filter config", err)
		}

		var fileMessageTypeFilters []filter.MessageTypeFilter

		indexer.BlockEventFilterRegistries.BeginBlockEventFilterRegistry.BlockEventFilters,
			indexer.BlockEventFilterRegistries.BeginBlockEventFilterRegistry.RollingWindowEventFilters,
			indexer.BlockEventFilterRegistries.EndBlockEventFilterRegistry.BlockEventFilters,
			indexer.BlockEventFilterRegistries.EndBlockEventFilterRegistry.RollingWindowEventFilters,
			fileMessageTypeFilters,
			err = config.ParseJSONFilterConfig(b)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to parse block event filter config", err)
		}

		indexer.MessageTypeFilters = append(indexer.MessageTypeFilters, fileMessageTypeFilters...)
	}

	if len(indexer.CustomModels) != 0 {
		err = dbTypes.MigrateInterfaces(indexer.DB, indexer.CustomModels)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to migrate custom models", err)
		}
	}

	if len(indexer.CustomBeginBlockParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomBlockEventParsers(indexer.DB, indexer.CustomBeginBlockParserTrackers)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to migrate custom block event parsers", err)
		}
	}

	if len(indexer.CustomEndBlockParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomBlockEventParsers(indexer.DB, indexer.CustomEndBlockParserTrackers)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to migrate custom block event parsers", err)
		}
	}

	if len(indexer.CustomMessageParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomMessageParsers(indexer.DB, indexer.CustomMessageParserTrackers)
		if err != nil {
			safeCleanupSetupExit(&indexer)
			config.Log.Fatal("Failed to migrate custom message parsers", err)
		}

	}

	return nil
}

// SetupIndexer sets up the "indexer" package Indexer instance with the configuration, database, and chain client
func setupIndexer() *indexerPackage.Indexer {
	var err error

	config.SetChainConfig(indexer.Config.Probe.AccountPrefix)

	indexer.ChainClient, err = probe.GetProbeClient(indexer.Config.Probe, indexer.CustomModuleBasics, indexer.CustomMsgTypeRegistry)

	if err != nil {
		config.Log.Fatal("Failed to create probe client", err)
	}

	// Depending on the app configuration, wait for the chain to catch up
	chainCatchingUp, err := rpc.IsCatchingUp(indexer.ChainClient)
	for indexer.Config.Base.WaitForChain && chainCatchingUp && err == nil {
		// Wait between status checks, don't spam the node with requests
		config.Log.Debug("Chain is still catching up, please wait or disable check in config.")
		time.Sleep(time.Second * time.Duration(indexer.Config.Base.WaitForChainDelay))
		chainCatchingUp, err = rpc.IsCatchingUp(indexer.ChainClient)

		// This EOF error pops up from time to time and is unpredictable
		// It is most likely an error on the node, we would need to see any error logs on the node side
		// Try one more time
		if err != nil && strings.HasSuffix(err.Error(), "EOF") {
			time.Sleep(time.Second * time.Duration(indexer.Config.Base.WaitForChainDelay))
			chainCatchingUp, err = rpc.IsCatchingUp(indexer.ChainClient)
		}
	}
	if err != nil {
		close(indexer.PostSetupDatasetChannel)
		config.Log.Fatal("Error querying chain status.", err)
	}

	if indexer.PostSetupDatasetChannel != nil {
		indexer.PostSetupDatasetChannel <- &indexerPackage.PostSetupDataset{
			Config:      indexer.Config,
			DryRun:      indexer.DryRun,
			ChainClient: indexer.ChainClient,
		}
	}

	close(indexer.PostSetupDatasetChannel)

	if indexer.PostSetupCustomFunction != nil {
		err = indexer.PostSetupCustomFunction(indexerPackage.PostSetupCustomDataset{
			ChainClient: indexer.ChainClient,
			Config:      *indexer.Config,
			DB:          indexer.DB,
		})
		if err != nil {
			config.Log.Fatal("Failed to run post setup custom function", err)
		}
	}

	return &indexer
}

func index(cmd *cobra.Command, args []string) {
	// Setup the indexer with config, db, and cl
	idxr := setupIndexer()
	dbConn, err := idxr.DB.DB()
	if err != nil {
		config.Log.Fatal("Failed to connect to DB", err)
	}
	defer dbConn.Close()

	// blockChans are just the block heights; limit max jobs in the queue, otherwise this queue would contain one
	// item (block height) for every block on the entire blockchain we're indexing. Furthermore, once the queue
	// is close to empty, we will spin up a new thread to fill it up with new jobs.
	blockEnqueueChan := make(chan *core.EnqueueData, 10000)

	// This channel represents query job results for the RPC queries to Cosmos Nodes. Every time an RPC query
	// completes, the query result will be sent to this channel (for later processing by a different thread).
	// Realistically, I expect that RPC queries will be slower than our relational DB on the local network.
	// If RPC queries are faster than DB inserts this buffer will fill up.
	// We will periodically check the buffer size to monitor performance so we can optimize later.
	rpcQueryThreads := int(idxr.Config.Base.RPCWorkers)
	if rpcQueryThreads == 0 {
		rpcQueryThreads = 4
	} else if rpcQueryThreads > 64 {
		rpcQueryThreads = 64
	}

	var wg sync.WaitGroup // This group is to ensure we are done processing transactions and events before returning

	chain := models.Chain{
		ChainID: idxr.Config.Probe.ChainID,
		Name:    idxr.Config.Probe.ChainName,
	}

	dbChainID, err := dbTypes.GetDBChainID(idxr.DB, chain)
	if err != nil {
		config.Log.Fatal("Failed to add/create chain in DB", err)
	}

	// This block consolidates all base RPC requests into one worker.
	// Workers read from the enqueued blocks and query blockchain data from the RPC server.
	var blockRPCWaitGroup sync.WaitGroup
	blockRPCWorkerDataChan := make(chan core.IndexerBlockEventData, 10)
	for i := 0; i < rpcQueryThreads; i++ {
		blockRPCWaitGroup.Add(1)
		go core.BlockRPCWorker(&blockRPCWaitGroup, blockEnqueueChan, dbChainID, idxr.Config.Probe.ChainID, idxr.Config, idxr.ChainClient, idxr.DB, blockRPCWorkerDataChan)
	}

	go func() {
		blockRPCWaitGroup.Wait()
		close(blockRPCWorkerDataChan)
	}()

	// Block BeginBlocker and EndBlocker indexing requirements. Indexes block events that took place in the BeginBlock and EndBlock state transitions
	blockEventsDataChan := make(chan *indexerPackage.BlockEventsDBData, 4*rpcQueryThreads)
	txDataChan := make(chan *indexerPackage.DBData, 4*rpcQueryThreads)

	wg.Add(1)
	go idxr.ProcessBlocks(&wg, core.HandleFailedBlock, blockRPCWorkerDataChan, blockEventsDataChan, txDataChan, dbChainID, indexer.BlockEventFilterRegistries)

	wg.Add(1)
	go idxr.DoDBUpdates(&wg, txDataChan, blockEventsDataChan, dbChainID)

	// Automatic failed-block backfill: periodically re-enqueue heights recorded
	// in failed_blocks. Successful reprocessing deletes the row inside the
	// block-write transaction; repeated failures are backoff-limited and
	// reported once they exceed the attempt budget. Only started in continuous
	// indexing mode: bounded runs (block input file, exit-when-caught-up, end
	// block) close the enqueue channel when the range completes, which the
	// retry loop does not survive. Those runs can use reattempt-failed-blocks.
	failedBlockRetryStop := make(chan struct{})
	continuousIndexing := idxr.Config.Base.EndBlock == -1 && !idxr.Config.Base.ExitWhenCaughtUp && idxr.Config.Base.BlockInputFile == "" && idxr.Config.Base.ReindexMessageType == ""
	if idxr.Config.Base.FailedBlockRetry && continuousIndexing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeHeightFloor := func() (int64, error) {
				earliest, _, err := rpc.GetEarliestAndLatestBlockHeights(idxr.ChainClient)
				return earliest, err
			}
			core.FailedBlockRetryLoop(idxr.DB, *idxr.Config, dbChainID, idxr.Config.Probe.ChainName, nodeHeightFloor, blockEnqueueChan, failedBlockRetryStop)
		}()
	} else if idxr.Config.Base.FailedBlockRetry {
		config.Log.Warn("base.failed-block-retry is enabled but indexing is bounded (end-block, exit-when-caught-up, block-input-file or reindex-message-type); failed-block retry will not run")
	}

	switch {
	// If block enqueue function has been explicitly set, use that
	case idxr.BlockEnqueueFunction != nil:
	// Default block enqueue functions based on config values
	case idxr.Config.Base.ReindexMessageType != "":
		idxr.BlockEnqueueFunction, err = core.GenerateMsgTypeEnqueueFunction(idxr.DB, *idxr.Config, dbChainID, idxr.Config.Base.ReindexMessageType)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	case idxr.Config.Base.BlockInputFile != "":
		idxr.BlockEnqueueFunction, err = core.GenerateBlockFileEnqueueFunction(idxr.DB, *idxr.Config, idxr.ChainClient, dbChainID, idxr.Config.Base.BlockInputFile)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	default:
		idxr.BlockEnqueueFunction, err = core.GenerateDefaultEnqueueFunction(idxr.DB, *idxr.Config, idxr.ChainClient, dbChainID)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	}

	err = idxr.BlockEnqueueFunction(blockEnqueueChan)
	if err != nil {
		config.Log.Fatal("Block enqueue failed", err)
	}

	// Stop the failed-block retry loop before closing the enqueue channel so it
	// cannot send on a closed channel.
	close(failedBlockRetryStop)
	close(blockEnqueueChan)

	wg.Wait()

	if indexer.PreExitCustomFunction != nil {
		err = indexer.PreExitCustomFunction(&indexerPackage.PreExitCustomDataset{
			Config: *idxr.Config,
			DB:     indexer.DB,
			DryRun: indexer.DryRun,
		})
		if err != nil {
			config.Log.Fatal("Failed to run pre-exit custom function", err)
		}
	}
}
