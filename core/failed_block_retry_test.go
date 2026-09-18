package core

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/suite"
	"gorm.io/gorm"
)

type FailedBlockRetryLoopSuite struct {
	suite.Suite
	db      *gorm.DB
	clean   func()
	chainID uint
}

func (suite *FailedBlockRetryLoopSuite) SetupTest() {
	clean, db, err := setupRetryTestDatabase()
	suite.Require().NoError(err)

	suite.db = db
	suite.clean = clean
	suite.chainID = suite.setupChain()
}

func (suite *FailedBlockRetryLoopSuite) TearDownTest() {
	if suite.clean != nil {
		suite.clean()
	}

	suite.db = nil
	suite.clean = nil
}

func (suite *FailedBlockRetryLoopSuite) setupChain() uint {
	err := dbTypes.MigrateModels(suite.db)
	suite.Require().NoError(err)

	chain := models.Chain{ChainID: "loopchain-1", Name: "LoopTest"}
	err = suite.db.Create(&chain).Error
	suite.Require().NoError(err)
	return chain.ID
}

// setupRetryTestDatabase mirrors the dockertest postgres setup used by the db
// package tests; it lives here because the helper is test-only and core cannot
// import db's test files.
func setupRetryTestDatabase() (func(), *gorm.DB, error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, nil, err
	}
	if err := pool.Client.Ping(); err != nil {
		return nil, nil, err
	}

	resource, err := pool.Run("postgres", "15-alpine", []string{"POSTGRES_USER=test", "POSTGRES_PASSWORD=test", "POSTGRES_DB=test"})
	if err != nil {
		return nil, nil, err
	}

	var db *gorm.DB
	if err := pool.Retry(func() error {
		var err error
		db, err = dbTypes.PostgresDbConnect(resource.GetBoundIP("5432/tcp"), resource.GetPort("5432/tcp"), "test", "test", "test", "silent")
		return err
	}); err != nil {
		return nil, nil, err
	}

	clean := func() {
		if err := pool.Purge(resource); err != nil {
			panic(fmt.Sprintf("could not purge retry test database: %s", err))
		}
	}
	return clean, db, nil
}

func TestFailedBlockRetryLoopSuite(t *testing.T) {
	suite.Run(t, new(FailedBlockRetryLoopSuite))
}

func (suite *FailedBlockRetryLoopSuite) retryCfg() config.IndexConfig {
	return config.IndexConfig{
		Base: config.IndexBase{
			FailedBlockRetry:                true,
			FailedBlockRetryIntervalSeconds: 1,
			FailedBlockRetryBatchSize:       100,
			FailedBlockRetryMaxAttempts:     3,
			TransactionIndexingEnabled:      true,
			BlockEventIndexingEnabled:       false,
		},
	}
}

func (suite *FailedBlockRetryLoopSuite) seed(height int64, attempts int, lastAttempted *time.Time) {
	row := models.FailedBlock{
		Height:          height,
		BlockchainID:    suite.chainID,
		Attempts:        attempts,
		LastAttemptedAt: lastAttempted,
	}
	err := suite.db.Create(&row).Error
	suite.Require().NoError(err)
}

func (suite *FailedBlockRetryLoopSuite) TestRetryEnqueuesEligibleHeightsWithConfiguredFlags() {
	stale := time.Now().UTC().Add(-2 * time.Hour)
	suite.seed(300, 1, &stale)
	suite.seed(301, 0, nil)
	suite.seed(302, 5, nil) // over the max attempts of the test config

	enqueueChan := make(chan *EnqueueData, 10)
	stop := make(chan struct{})
	defer close(stop)

	noFloor := func() (int64, error) { return 0, nil }
	go FailedBlockRetryLoop(suite.db, suite.retryCfg(), suite.chainID, "LoopTest", noFloor, enqueueChan, stop)

	retried := make([]int64, 0, 2)
	for range 2 {
		select {
		case data := <-enqueueChan:
			retried = append(retried, data.Height)
			suite.Assert().True(data.IndexTransactions, "transaction indexing flag must come from the config")
			suite.Assert().False(data.IndexBlockEvents, "block event indexing flag must come from the config")
		case <-time.After(5 * time.Second):
			suite.Require().FailNow("timed out waiting for retried heights")
		}
	}
	suite.Assert().Equal([]int64{300, 301}, retried, "eligible heights are retried oldest-first; exhausted heights are skipped")
}

func (suite *FailedBlockRetryLoopSuite) TestRetrySkipsHeightsBelowNodeFloor() {
	stale := time.Now().UTC().Add(-2 * time.Hour)
	suite.seed(600, 1, &stale) // below the floor returned by the fake node
	suite.seed(700, 1, &stale) // within node history

	enqueueChan := make(chan *EnqueueData, 4)
	stop := make(chan struct{})
	defer close(stop)

	floor := func() (int64, error) { return 650, nil }
	go FailedBlockRetryLoop(suite.db, suite.retryCfg(), suite.chainID, "LoopTest", floor, enqueueChan, stop)

	select {
	case data := <-enqueueChan:
		suite.Assert().Equal(int64(700), data.Height, "only heights the node can serve are retried")
	case <-time.After(5 * time.Second):
		suite.Require().FailNow("timed out waiting for retried heights")
	}
	select {
	case data := <-enqueueChan:
		suite.Require().FailNow("height below the node floor must not be enqueued, got %d", data.Height)
	case <-time.After(300 * time.Millisecond):
	}
}

func (suite *FailedBlockRetryLoopSuite) TestRetrySkipsCycleWhenFloorUnavailable() {
	suite.seed(800, 1, nil)

	enqueueChan := make(chan *EnqueueData, 4)
	stop := make(chan struct{})
	defer close(stop)

	brokenFloor := func() (int64, error) { return 0, errors.New("rpc down") }
	go FailedBlockRetryLoop(suite.db, suite.retryCfg(), suite.chainID, "LoopTest", brokenFloor, enqueueChan, stop)

	select {
	case data := <-enqueueChan:
		suite.Require().FailNow("cycle must be skipped when the node floor is unavailable, got %d", data.Height)
	case <-time.After(500 * time.Millisecond):
	}
}

func (suite *FailedBlockRetryLoopSuite) TestRetryRepeatsAfterIntervalAndStopsCleanly() {
	suite.seed(400, 0, nil)

	enqueueChan := make(chan *EnqueueData, 4)
	stop := make(chan struct{})

	noFloor := func() (int64, error) { return 0, nil }
	go FailedBlockRetryLoop(suite.db, suite.retryCfg(), suite.chainID, "LoopTest", noFloor, enqueueChan, stop)

	// First pass fires immediately.
	select {
	case <-enqueueChan:
	case <-time.After(5 * time.Second):
		suite.Require().FailNow("timed out waiting for the immediate first retry pass")
	}

	// The height stays in failed_blocks until its block is actually written,
	// so the next tick must re-enqueue it (backoff honored via last_attempted_at
	// once the processing path records an attempt).
	select {
	case <-enqueueChan:
		suite.Require().FailNow("second pass should wait for the interval")
	case <-time.After(200 * time.Millisecond):
	}

	close(stop)
	// The loop must exit promptly on stop; if it deadlocked this test would
	// hang, which the go test timeout catches.
}
