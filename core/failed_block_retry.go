package core

import (
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

// failedBlockRetrySendTimeout bounds how long the retry loop waits for room in
// the enqueue channel before deferring the remaining heights to the next cycle.
// The head-indexing enqueue function keeps that channel near capacity, so the
// retry loop must yield instead of stalling head indexing.
const failedBlockRetrySendTimeout = time.Minute

// FailedBlockRetryLoop periodically re-enqueues blocks recorded in
// failed_blocks so transient failures are recovered without operator
// intervention. Successful reprocessing removes the failed_blocks row inside
// the same transaction that writes the block (see IndexNewBlock), so no
// special-casing is needed here. Blocks that keep failing accumulate Attempts
// until they exceed maxAttempts, then they are reported as stuck and skipped.
func FailedBlockRetryLoop(db *gorm.DB, cfg config.IndexConfig, chainID uint, chainName string, enqueueChan chan<- *EnqueueData, stop <-chan struct{}) {
	interval := time.Duration(cfg.Base.FailedBlockRetryIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batchSize := cfg.Base.FailedBlockRetryBatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	maxAttempts := cfg.Base.FailedBlockRetryMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 10
	}

	// First pass runs immediately so a restarted indexer behaves like the
	// legacy reattempt-failed-blocks startup sweep.
	retryFailedBlocks(db, cfg, chainID, chainName, enqueueChan, interval, batchSize, maxAttempts)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			retryFailedBlocks(db, cfg, chainID, chainName, enqueueChan, interval, batchSize, maxAttempts)
		}
	}
}

func retryFailedBlocks(db *gorm.DB, cfg config.IndexConfig, chainID uint, chainName string, enqueueChan chan<- *EnqueueData, interval time.Duration, batchSize, maxAttempts int) {
	heights, err := dbTypes.FailedBlockRetryHeights(db, chainID, interval, maxAttempts, batchSize)
	if err != nil {
		config.Log.Errorf("Failed block retry: could not query failed blocks. Err: %v", err)
		return
	}
	if len(heights) == 0 {
		return
	}

	config.Log.Infof("Failed block retry: re-enqueuing %d failed block(s) for reprocessing, oldest height %d", len(heights), heights[0])
	enqueued := 0
	for _, height := range heights {
		select {
		case enqueueChan <- &EnqueueData{
			Height:            height,
			IndexBlockEvents:  cfg.Base.BlockEventIndexingEnabled,
			IndexTransactions: cfg.Base.TransactionIndexingEnabled,
		}:
			enqueued++
		case <-time.After(failedBlockRetrySendTimeout):
			config.Log.Warnf("Failed block retry: enqueue channel stayed full for %s; deferring %d remaining height(s) to the next cycle", failedBlockRetrySendTimeout, len(heights)-enqueued)
			return
		}
	}
	config.Log.Infof("Failed block retry: enqueued %d failed block(s)", enqueued)

	stuckTotal, stuckSamples, err := dbTypes.StuckFailedBlockCount(db, chainID, maxAttempts, 10)
	if err != nil {
		config.Log.Errorf("Failed block retry: could not count stuck failed blocks. Err: %v", err)
		return
	}
	if stuckTotal > 0 {
		config.Log.Errorf("Failed block retry: %d failed block(s) exhausted %d attempts and will not be retried automatically; investigate or raise base.failed-block-retry-max-attempts. Sample heights: %v", stuckTotal, maxAttempts, stuckSamples)
	}
}
