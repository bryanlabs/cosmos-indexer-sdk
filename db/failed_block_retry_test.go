package db

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/suite"
	"gorm.io/gorm"
)

type FailedBlockRetrySuite struct {
	suite.Suite
	db      *gorm.DB
	clean   func()
	chainID uint
}

func (suite *FailedBlockRetrySuite) SetupTest() {
	clean, db, err := SetupTestDatabase()
	suite.Require().NoError(err)

	suite.db = db
	suite.clean = clean
}

func (suite *FailedBlockRetrySuite) TearDownTest() {
	if suite.clean != nil {
		suite.clean()
	}

	suite.db = nil
	suite.clean = nil
}

func (suite *FailedBlockRetrySuite) setupChain() uint {
	err := MigrateModels(suite.db)
	suite.Require().NoError(err)

	chain := models.Chain{ChainID: "testchain-retry", Name: "RetryTest"}
	err = suite.db.Create(&chain).Error
	suite.Require().NoError(err)
	return chain.ID
}

func (suite *FailedBlockRetrySuite) seedFailed(height int64, attempts int, lastAttempted *time.Time, lastError string) {
	row := models.FailedBlock{
		Height:          height,
		BlockchainID:    suite.chainID,
		Attempts:        attempts,
		LastError:       lastError,
		LastAttemptedAt: lastAttempted,
	}
	err := suite.db.Create(&row).Error
	suite.Require().NoError(err)
}

func TestFailedBlockRetrySuite(t *testing.T) {
	suite.Run(t, new(FailedBlockRetrySuite))
}

func (suite *FailedBlockRetrySuite) TestUpsertFailedBlockTracksAttemptsAndError() {
	suite.chainID = suite.setupChain()

	firstErr := errors.New("rpc connection refused")
	err := UpsertFailedBlock(suite.db, 500, "testchain-retry", "RetryTest", firstErr)
	suite.Require().NoError(err)

	var row models.FailedBlock
	err = suite.db.Where("height = ?", 500).First(&row).Error
	suite.Require().NoError(err)
	suite.Assert().Equal(1, row.Attempts)
	suite.Assert().Equal("rpc connection refused", row.LastError)
	suite.Assert().NotNil(row.LastAttemptedAt)

	secondErr := fmt.Errorf("tx message could not be processed: %s", "unknown type")
	err = UpsertFailedBlock(suite.db, 500, "testchain-retry", "RetryTest", secondErr)
	suite.Require().NoError(err)

	err = suite.db.Where("height = ?", 500).First(&row).Error
	suite.Require().NoError(err)
	suite.Assert().Equal(2, row.Attempts)
	suite.Assert().Equal(secondErr.Error(), row.LastError)

	// A nil error must still count the attempt without clobbering the stored
	// error with an empty string.
	err = UpsertFailedBlock(suite.db, 500, "testchain-retry", "RetryTest", nil)
	suite.Require().NoError(err)
	err = suite.db.Where("height = ?", 500).First(&row).Error
	suite.Require().NoError(err)
	suite.Assert().Equal(3, row.Attempts)
	suite.Assert().Equal(secondErr.Error(), row.LastError)
}

func (suite *FailedBlockRetrySuite) TestFailedBlockRetryHeightsFiltering() {
	suite.chainID = suite.setupChain()

	stale := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC().Add(-time.Minute)

	suite.seedFailed(100, 1, &stale, "old failure")  // eligible, oldest height
	suite.seedFailed(110, 0, nil, "")                // eligible, never attempted
	suite.seedFailed(120, 1, &recent, "backing off") // not eligible: attempted too recently
	suite.seedFailed(130, 5, &stale, "exhausted")    // not eligible: at max attempts

	heights, err := FailedBlockRetryHeights(suite.db, suite.chainID, time.Hour, 5, 100)
	suite.Require().NoError(err)
	suite.Assert().Equal([]int64{100, 110}, heights)

	heights, err = FailedBlockRetryHeights(suite.db, suite.chainID, time.Hour, 1, 100)
	suite.Require().NoError(err)
	suite.Assert().Equal([]int64{110}, heights, "max attempts 1 excludes every height that already used its attempt")

	heights, err = FailedBlockRetryHeights(suite.db, suite.chainID, time.Hour, 5, 1)
	suite.Require().NoError(err)
	suite.Assert().Equal([]int64{100}, heights, "limit applies oldest-first")

	// Unrelated chain rows must never be returned.
	err = UpsertFailedBlock(suite.db, 900, "other-chain", "Other", errors.New("other chain failure"))
	suite.Require().NoError(err)
	heights, err = FailedBlockRetryHeights(suite.db, suite.chainID, time.Hour, 5, 100)
	suite.Require().NoError(err)
	suite.Assert().Equal([]int64{100, 110}, heights)
}

func (suite *FailedBlockRetrySuite) TestStuckFailedBlockCount() {
	suite.chainID = suite.setupChain()

	suite.seedFailed(200, 12, nil, "stuck one")
	suite.seedFailed(210, 10, nil, "stuck two")
	suite.seedFailed(220, 3, nil, "still trying")

	total, samples, err := StuckFailedBlockCount(suite.db, suite.chainID, 10, 1)
	suite.Require().NoError(err)
	suite.Assert().Equal(int64(2), total)
	suite.Assert().Equal([]int64{200}, samples)

	total, samples, err = StuckFailedBlockCount(suite.db, suite.chainID, 10, 10)
	suite.Require().NoError(err)
	suite.Assert().Equal(int64(2), total)
	suite.Assert().Equal([]int64{200, 210}, samples)

	total, samples, err = StuckFailedBlockCount(suite.db, suite.chainID, 50, 10)
	suite.Require().NoError(err)
	suite.Assert().Equal(int64(0), total)
	suite.Assert().Empty(samples)
}

func (suite *FailedBlockRetrySuite) TestIndexNewBlockClearsFailedBlockRow() {
	suite.chainID = suite.setupChain()

	address := models.Address{Address: "cosmos1proposer"}
	err := suite.db.Create(&address).Error
	suite.Require().NoError(err)

	err = UpsertFailedBlock(suite.db, 600, "testchain-retry", "RetryTest", errors.New("transient failure"))
	suite.Require().NoError(err)

	block := models.Block{
		ChainID:             suite.chainID,
		Height:              600,
		TimeStamp:           time.Now(),
		ProposerConsAddress: address,
		TxIndexed:           true,
	}
	_, _, err = IndexNewBlock(suite.db, block, []TxDBWrapper{}, config.IndexConfig{})
	suite.Require().NoError(err)

	var count int64
	err = suite.db.Model(&models.FailedBlock{}).Where("height = ?", 600).Count(&count).Error
	suite.Require().NoError(err)
	suite.Assert().Equal(int64(0), count, "a successful block write must remove its failed_blocks row")
}
