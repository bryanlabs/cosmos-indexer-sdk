package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

type IndexConfig struct {
	Database Database
	Base     IndexBase
	Log      log
	Probe    Probe
	Flags    flags
}

type IndexBase struct {
	throttlingBase
	retryBase
	ReindexMessageType              string `mapstructure:"reindex-message-type"`
	ReattemptFailedBlocks           bool   `mapstructure:"reattempt-failed-blocks"`
	FailedBlockRetry                bool   `mapstructure:"failed-block-retry"`
	FailedBlockRetryIntervalSeconds int    `mapstructure:"failed-block-retry-interval-seconds"`
	FailedBlockRetryBatchSize       int    `mapstructure:"failed-block-retry-batch-size"`
	FailedBlockRetryMaxAttempts     int    `mapstructure:"failed-block-retry-max-attempts"`
	StartBlock                      int64  `mapstructure:"start-block"`
	EndBlock                        int64  `mapstructure:"end-block"`
	BlockInputFile                  string `mapstructure:"block-input-file"`
	ReIndex                         bool   `mapstructure:"reindex"`
	RPCWorkers                      int64  `mapstructure:"rpc-workers"`
	SkipBlockByHeightRPCRequest     bool   `mapstructure:"skip-block-by-height-rpc-request"`
	BlockTimer                      int64  `mapstructure:"block-timer"`
	WaitForChain                    bool   `mapstructure:"wait-for-chain"`
	WaitForChainDelay               int64  `mapstructure:"wait-for-chain-delay"`
	TransactionIndexingEnabled      bool   `mapstructure:"index-transactions"`
	ExitWhenCaughtUp                bool   `mapstructure:"exit-when-caught-up"`
	BlockEventIndexingEnabled       bool   `mapstructure:"index-block-events"`
	FilterFile                      string `mapstructure:"filter-file"`
	Dry                             bool   `mapstructure:"dry"`
}

// Flags for specific, deeper indexing behavior
type flags struct {
	IndexTxMessageRaw        bool `mapstructure:"index-tx-message-raw"`
	IndexEmptyTransactions   bool `mapstructure:"index-empty-transactions"`
	BlockEventsBase64Encoded bool `mapstructure:"block-events-base64-encoded"`
	IndexMessageEvents       bool `mapstructure:"index-message-events"`
}

func SetupIndexSpecificFlags(conf *IndexConfig, cmd *cobra.Command) {
	// chain indexing
	cmd.PersistentFlags().Int64Var(&conf.Base.StartBlock, "base.start-block", 0, "block to start indexing at (use -1 to resume from highest block indexed)")
	cmd.PersistentFlags().Int64Var(&conf.Base.EndBlock, "base.end-block", -1, "block to stop indexing at (use -1 to index indefinitely")
	cmd.PersistentFlags().StringVar(&conf.Base.BlockInputFile, "base.block-input-file", "", "A file location containing a JSON list of block heights to index. Will override start and end block flags.")
	cmd.PersistentFlags().BoolVar(&conf.Base.ReIndex, "base.reindex", false, "if true, this will re-attempt to index blocks we have already indexed (defaults to false)")
	cmd.PersistentFlags().BoolVar(&conf.Base.ReattemptFailedBlocks, "base.reattempt-failed-blocks", false, "re-enqueue failed blocks for reattempts at startup.")
	cmd.PersistentFlags().BoolVar(&conf.Base.FailedBlockRetry, "base.failed-block-retry", false, "periodically re-enqueue failed blocks for reprocessing while indexing.")
	cmd.PersistentFlags().IntVar(&conf.Base.FailedBlockRetryIntervalSeconds, "base.failed-block-retry-interval-seconds", 600, "seconds between failed-block retry cycles.")
	cmd.PersistentFlags().IntVar(&conf.Base.FailedBlockRetryBatchSize, "base.failed-block-retry-batch-size", 100, "maximum failed blocks re-enqueued per retry cycle.")
	cmd.PersistentFlags().IntVar(&conf.Base.FailedBlockRetryMaxAttempts, "base.failed-block-retry-max-attempts", 10, "retry attempts per failed block before it is reported as stuck.")
	cmd.PersistentFlags().StringVar(&conf.Base.ReindexMessageType, "base.reindex-message-type", "", "a Cosmos message type URL. When set, the block enqueue method will reindex all blocks between start and end block that contain this message type.")
	// block event indexing
	cmd.PersistentFlags().BoolVar(&conf.Base.TransactionIndexingEnabled, "base.index-transactions", false, "enable transaction indexing?")
	cmd.PersistentFlags().BoolVar(&conf.Base.BlockEventIndexingEnabled, "base.index-block-events", false, "enable block beginblocker and endblocker event indexing?")
	// filter configs
	cmd.PersistentFlags().StringVar(&conf.Base.FilterFile, "base.filter-file", "", "path to a file containing a JSON config of block event and message type filters to apply to beginblocker events, endblocker events and TX messages")
	// other base setting
	cmd.PersistentFlags().BoolVar(&conf.Base.Dry, "base.dry", false, "index the chain but don't insert data in the DB.")
	cmd.PersistentFlags().Int64Var(&conf.Base.RPCWorkers, "base.rpc-workers", 1, "the number of concurrent RPC request workers to spin up.")
	cmd.PersistentFlags().BoolVar(&conf.Base.SkipBlockByHeightRPCRequest, "base.skip-block-by-height-rpc-request", false, "skip the /block?height=<height> RPC request and only attempt the /block_results RPC request. Sometimes pruned nodes will not have return results for the block RPC request, but still return results for the block_result request.")
	cmd.PersistentFlags().BoolVar(&conf.Base.WaitForChain, "base.wait-for-chain", false, "wait for chain to be in sync?")
	cmd.PersistentFlags().Int64Var(&conf.Base.WaitForChainDelay, "base.wait-for-chain-delay", 10, "seconds to wait between each check for node to catch up to the chain")
	cmd.PersistentFlags().Int64Var(&conf.Base.BlockTimer, "base.block-timer", 10000, "print out how long it takes to process this many blocks")
	cmd.PersistentFlags().BoolVar(&conf.Base.ExitWhenCaughtUp, "base.exit-when-caught-up", false, "Gets the latest block at runtime and exits when this block has been reached.")
	cmd.PersistentFlags().Int64Var(&conf.Base.RequestRetryAttempts, "base.request-retry-attempts", 0, "number of RPC query retries to make")
	cmd.PersistentFlags().Uint64Var(&conf.Base.RequestRetryMaxWait, "base.request-retry-max-wait", 30, "max retry incremental backoff wait time in seconds")

	// flags
	cmd.PersistentFlags().BoolVar(&conf.Flags.IndexTxMessageRaw, "flags.index-tx-message-raw", false, "if true, this will index the raw message bytes. This will significantly increase the size of the database.")
	cmd.PersistentFlags().BoolVar(&conf.Flags.IndexEmptyTransactions, "flags.index-empty-transactions", true, "if true, this will index transactions that have no messages. Setting this to false when filtering TX message types will result in no transactions being indexed if all message types are filtered out.")
	cmd.PersistentFlags().BoolVar(&conf.Flags.BlockEventsBase64Encoded, "flags.block-events-base64-encoded", false, "if true, decode the block event attributes and keys as base64. Some versions of CometBFT encode the block event attributes and keys as base64 in the response from RPC.")
	cmd.PersistentFlags().BoolVar(&conf.Flags.IndexMessageEvents, "flags.index-message-events", true, "if true, skip indexing message events if they are uneeded. This will save space in the database.")
}

func (conf *IndexConfig) Validate() error {
	err := validateDatabaseConf(conf.Database)
	if err != nil {
		return err
	}

	probeConf := conf.Probe

	probeConf, err = validateProbeConf(probeConf)
	if err != nil {
		return err
	}

	conf.Probe = probeConf

	err = validateThrottlingConf(conf.Base.throttlingBase)
	if err != nil {
		return err
	}

	err = conf.validateBlockInputValues()

	if err != nil {
		return err
	}

	if conf.Base.BlockInputFile != "" {
		if _, err := os.Stat(conf.Base.BlockInputFile); os.IsNotExist(err) {
			return fmt.Errorf("base.block-input-file %s does not exist", conf.Base.BlockInputFile)
		}
	}

	if conf.Base.FilterFile != "" {
		// check if file exists
		if _, err := os.Stat(conf.Base.FilterFile); os.IsNotExist(err) {
			return fmt.Errorf("base.filter-file %s does not exist", conf.Base.FilterFile)
		}
	}

	return nil
}

func (conf *IndexConfig) validateBlockInputValues() error {
	if !conf.Base.TransactionIndexingEnabled && !conf.Base.BlockEventIndexingEnabled {
		return errors.New("must enable at least one of base.index-transactions or base.index-block-events")
	}

	if conf.Base.BlockInputFile != "" {
		return nil
	}

	if conf.Base.StartBlock < 0 {
		return errors.New("start block cannot be negative")
	}

	if conf.Base.StartBlock == 0 {
		return errors.New("must provide a positive start block or block input file")
	}

	if conf.Base.EndBlock == 0 || conf.Base.EndBlock < -1 {
		return errors.New("must provide an end block or -1 to index indefinitely")
	}

	if conf.Base.EndBlock != -1 && conf.Base.StartBlock > conf.Base.EndBlock {
		return errors.New("start block must be less than or equal to end block")
	}

	return nil
}

func CheckSuperfluousIndexKeys(keys []string) []string {
	validKeys := make(map[string]struct{})

	addDatabaseConfigKeys(validKeys)
	addLogConfigKeys(validKeys)
	addProbeConfigKeys(validKeys)

	// add base keys
	for _, key := range getValidConfigKeys(IndexBase{}, "base") {
		validKeys[key] = struct{}{}
	}

	for _, key := range getValidConfigKeys(throttlingBase{}, "base") {
		validKeys[key] = struct{}{}
	}

	for _, key := range getValidConfigKeys(retryBase{}, "base") {
		validKeys[key] = struct{}{}
	}

	for _, key := range getValidConfigKeys(flags{}, "flags") {
		validKeys[key] = struct{}{}
	}

	// Check keys
	ignoredKeys := make([]string, 0)
	for _, key := range keys {
		if _, ok := validKeys[key]; !ok {
			ignoredKeys = append(ignoredKeys, key)
		}
	}

	return ignoredKeys
}
