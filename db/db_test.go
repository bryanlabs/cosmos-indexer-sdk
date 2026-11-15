package db

import (
	"log"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/suite"
	"gorm.io/gorm"
)

// TODO: Optimize tests to use a single database instance, clean database after each test, and teardown database after all tests are done

type DBTestSuite struct {
	suite.Suite
	db    *gorm.DB
	clean func()
}

func (suite *DBTestSuite) SetupTest() {
	clean, db, err := SetupTestDatabase()
	suite.Require().NoError(err)

	suite.db = db
	suite.clean = clean
}

func (suite *DBTestSuite) TearDownTest() {
	if suite.clean != nil {
		suite.clean()
	}

	suite.db = nil
	suite.clean = nil
}

func (suite *DBTestSuite) TestMigrateModels() {
	err := MigrateModels(suite.db)
	suite.Require().NoError(err)
}

func (suite *DBTestSuite) TestGetDBChainID() {
	err := MigrateModels(suite.db)
	suite.Require().NoError(err)

	initChain := models.Chain{
		ChainID: "testchain-1",
	}

	err = suite.db.Create(&initChain).Error
	suite.Require().NoError(err)

	chainID, err := GetDBChainID(suite.db, initChain)
	suite.Require().NoError(err)
	suite.Assert().NotZero(chainID)
}

func SetupTestDatabase() (func(), *gorm.DB, error) {
	// TODO: allow environment overrides to skip creating mock database
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, nil, err
	}

	err = pool.Client.Ping()
	if err != nil {
		return nil, nil, err
	}

	resource, err := pool.Run("postgres", "15-alpine", []string{"POSTGRES_USER=test", "POSTGRES_PASSWORD=${PRESERVATION_REDACTED}", "POSTGRES_DB=test"})
	if err != nil {
		return nil, nil, err
	}

	var db *gorm.DB
	if err := pool.Retry(func() error {
		var err error
		db, err = PostgresDbConnect(resource.GetBoundIP("5432/tcp"), resource.GetPort("5432/tcp"), "test", "test", "test", "debug")
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}

	clean := func() {
		if err := pool.Purge(resource); err != nil {
			log.Fatalf("Could not purge resource: %s", err)
		}
	}

	return clean, db, nil
}

func createMockBlock(mockDb *gorm.DB, chain models.Chain, address models.Address, height int64, txIndexed bool, eventIndexed bool) (models.Block, error) {
	block := models.Block{
		Chain:               chain,
		Height:              height,
		TimeStamp:           time.Now(),
		TxIndexed:           txIndexed,
		BlockEventsIndexed:  eventIndexed,
		ProposerConsAddress: address,
	}

	err := mockDb.Create(&block).Error
	return block, err
}

func (suite *DBTestSuite) TestGetHighestBlockFunctions() {
	err := MigrateModels(suite.db)
	suite.Require().NoError(err)

	initChain := models.Chain{
		ChainID: "testchain-1",
	}

	err = suite.db.Create(&initChain).Error
	suite.Require().NoError(err)

	initConsAddress := models.Address{
		Address: "testchainaddress",
	}

	err = suite.db.Create(&initConsAddress).Error
	suite.Require().NoError(err)

	block1, err := createMockBlock(suite.db, initChain, initConsAddress, 1, true, true)
	suite.Require().NoError(err)

	txBlock := GetHighestIndexedBlock(suite.db, initChain.ID)
	eventBlock, err := GetHighestEventIndexedBlock(suite.db, initChain.ID)
	suite.Require().NoError(err)

	suite.Assert().Equal(block1.Height, txBlock.Height)
	suite.Assert().Equal(block1.Height, eventBlock.Height)

	_, err = createMockBlock(suite.db, initChain, initConsAddress, 2, false, false)
	suite.Require().NoError(err)

	txBlock = GetHighestIndexedBlock(suite.db, initChain.ID)
	eventBlock, err = GetHighestEventIndexedBlock(suite.db, initChain.ID)
	suite.Require().NoError(err)

	suite.Assert().Equal(block1.Height, txBlock.Height)
	suite.Assert().Equal(block1.Height, eventBlock.Height)

	block3, err := createMockBlock(suite.db, initChain, initConsAddress, 3, true, true)
	suite.Require().NoError(err)

	txBlock = GetHighestIndexedBlock(suite.db, initChain.ID)
	eventBlock, err = GetHighestEventIndexedBlock(suite.db, initChain.ID)
	suite.Require().NoError(err)

	suite.Assert().Equal(block3.Height, txBlock.Height)
	suite.Assert().Equal(block3.Height, eventBlock.Height)
}

func TestDBSuite(t *testing.T) {
	suite.Run(t, new(DBTestSuite))
}
