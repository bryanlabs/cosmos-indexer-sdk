package db

import (
	"errors"
	"fmt"
	"sort"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// PostgresDbConnect connects to the database according to the passed in parameters
func PostgresDbConnect(host string, port string, database string, user string, password string, level string) (*gorm.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable", host, port, database, user, password)
	gormLogLevel := logger.Silent

	if level == "info" {
		gormLogLevel = logger.Info
	}
	return gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(gormLogLevel)})
}

// MigrateModels runs the gorm automigrations with all the db models. This will migrate as needed and do nothing if nothing has changed.
func MigrateModels(db *gorm.DB) error {
	if err := migrateChainModels(db); err != nil {
		return err
	}

	if err := migrateBlockModels(db); err != nil {
		return err
	}

	if err := migrateDenomModels(db); err != nil {
		return err
	}

	if err := migrateTXModels(db); err != nil {
		return err
	}

	if err := migrateParserModels(db); err != nil {
		return err
	}

	return nil
}

func migrateChainModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Chain{},
	)
}

func migrateBlockModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Block{},
		&models.BlockEvent{},
		&models.BlockEventType{},
		&models.BlockEventAttribute{},
		&models.BlockEventAttributeKey{},
		&models.FailedBlock{},
		&models.FailedEventBlock{},
	)
}

func migrateDenomModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Denom{},
	)
}

func migrateTXModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Tx{},
		&models.Fee{},
		&models.Address{},
		&models.MessageType{},
		&models.Message{},
		&models.FailedTx{},
		&models.FailedMessage{},
		&models.MessageEvent{},
		&models.MessageEventType{},
		&models.MessageEventAttribute{},
		&models.MessageEventAttributeKey{},
	)
}

func migrateParserModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.BlockEventParser{},
		&models.BlockEventParserError{},
		&models.MessageParser{},
		&models.MessageParserError{},
	)
}

func MigrateInterfaces(db *gorm.DB, interfaces []any) error {
	return db.AutoMigrate(interfaces...)
}

func GetDBChainID(db *gorm.DB, chain models.Chain) (uint, error) {
	if err := db.Where("chain_id = ?", chain.ChainID).FirstOrCreate(&chain).Error; err != nil {
		config.Log.Error("Error getting/creating chain DB object.", err)
		return chain.ID, err
	}
	return chain.ID, nil
}

func GetHighestIndexedBlock(db *gorm.DB, chainID uint) models.Block {
	var block models.Block
	// this can potentially be optimized by getting max first and selecting it (this gets translated into a select * limit 1)
	db.Table("blocks").Where("chain_id = ?::int AND tx_indexed = true AND time_stamp != '0001-01-01T00:00:00.000Z'", chainID).Order("height desc").First(&block)
	return block
}

func GetBlocksFromStart(db *gorm.DB, chainID uint, startHeight int64, endHeight int64) ([]models.Block, error) {
	var blocks []models.Block

	initialWhere := db.Where("chain_id = ?::int AND time_stamp != '0001-01-01T00:00:00.000Z' AND height >= ?", chainID, startHeight)

	if endHeight != -1 {
		initialWhere = initialWhere.Where("height <= ?", endHeight)
	}

	if err := initialWhere.Find(&blocks).Error; err != nil {
		return nil, err
	}

	return blocks, nil
}

func GetHighestEventIndexedBlock(db *gorm.DB, chainID uint) (models.Block, error) {
	var block models.Block
	// this can potentially be optimized by getting max first and selecting it (this gets translated into a select * limit 1)
	err := db.Table("blocks").Where("chain_id = ?::int AND block_events_indexed = true AND time_stamp != '0001-01-01T00:00:00.000Z'", chainID).Order("height desc").First(&block).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return block, nil
	}

	return block, err
}

func UpsertFailedBlock(db *gorm.DB, blockHeight int64, chainID string, chainName string) error {
	return db.Transaction(func(dbTransaction *gorm.DB) error {
		failedBlock := models.FailedBlock{Height: blockHeight, Chain: models.Chain{ChainID: chainID, Name: chainName}}

		if err := dbTransaction.Where(&failedBlock.Chain).FirstOrCreate(&failedBlock.Chain).Error; err != nil {
			config.Log.Error("Error creating chain DB object.", err)
			return err
		}

		if err := dbTransaction.Where(&failedBlock).FirstOrCreate(&failedBlock).Error; err != nil {
			config.Log.Error("Error creating failed block DB object.", err)
			return err
		}
		return nil
	})
}

func UpsertFailedEventBlock(db *gorm.DB, blockHeight int64, chainID string, chainName string) error {
	return db.Transaction(func(dbTransaction *gorm.DB) error {
		failedEventBlock := models.FailedEventBlock{Height: blockHeight, Chain: models.Chain{ChainID: chainID, Name: chainName}}

		if err := dbTransaction.Where(&failedEventBlock.Chain).FirstOrCreate(&failedEventBlock.Chain).Error; err != nil {
			config.Log.Error("Error creating chain DB object.", err)
			return err
		}

		if err := dbTransaction.Where(&failedEventBlock).FirstOrCreate(&failedEventBlock).Error; err != nil {
			config.Log.Error("Error creating failed event block DB object.", err)
			return err
		}
		return nil
	})
}

func IndexNewBlock(db *gorm.DB, block models.Block, txs []TxDBWrapper, indexerConfig config.IndexConfig) (models.Block, []TxDBWrapper, error) {
	// consider optimizing the transaction, but how? Ordering matters due to foreign key constraints
	// Order required: Block -> (For each Tx: Signer Address -> Tx -> (For each Message: Message -> Taxable Events))
	// Also, foreign key relations are struct value based so create needs to be called first to get right foreign key ID
	err := db.Transaction(func(dbTransaction *gorm.DB) error {
		// remove from failed blocks if exists
		if err := dbTransaction.
			Exec("DELETE FROM failed_blocks WHERE height = ? AND blockchain_id = ?", block.Height, block.ChainID).
			Error; err != nil {
			config.Log.Error("Error updating failed block.", err)
			return err
		}

		consAddress, err := FindOrCreateAddressByAddress(dbTransaction, block.ProposerConsAddress.Address)
		// create cons address if it doesn't exist
		if err != nil {
			config.Log.Error("Error getting/creating cons address DB object.", err)
			return err
		}

		// create block if it doesn't exist
		block.ProposerConsAddressID = consAddress.ID
		block.ProposerConsAddress = consAddress
		block.TxIndexed = true
		if err := dbTransaction.
			Preload("Chain").
			Where(models.Block{Height: block.Height, ChainID: block.ChainID}).
			Assign(models.Block{TxIndexed: true, TimeStamp: block.TimeStamp}).
			FirstOrCreate(&block).Error; err != nil {
			config.Log.Error("Error getting/creating block DB object.", err)
			return err
		}

		// pull txes and insert them
		uniqueTxes := make(map[string]models.Tx)
		uniqueAddress := make(map[string]models.Address)

		denomMap := make(map[string]models.Denom)

		for _, tx := range txs {
			if !indexerConfig.Flags.IndexEmptyTransactions && len(tx.Messages) == 0 {
				continue
			}

			tx.Tx.BlockID = block.ID
			tx.Tx.Block = block
			uniqueTxes[tx.Tx.Hash] = tx.Tx
			if len(tx.Tx.SignerAddresses) != 0 {
				for _, signerAddress := range tx.Tx.SignerAddresses {
					uniqueAddress[signerAddress.Address] = signerAddress
				}
			}
			for feeIndex, fee := range tx.Tx.Fees {
				uniqueAddress[fee.PayerAddress.Address] = fee.PayerAddress

				denom := fee.Denomination

				if _, ok := denomMap[denom.Base]; !ok {
					denom, err = FindOrCreateDenomByBase(dbTransaction, denom.Base)
					if err != nil {
						config.Log.Error("Error getting/creating denom DB object.", err)
						return err
					}
					denomMap[denom.Base] = denom
				} else {
					denom = denomMap[denom.Base]
				}

				tx.Tx.Fees[feeIndex].DenominationID = denom.ID
				tx.Tx.Fees[feeIndex].Denomination = denom
			}
		}

		var addressesSlice []models.Address
		for _, address := range uniqueAddress {
			addressesSlice = append(addressesSlice, address)
		}
		// Lock rows in a stable order so concurrent indexers cannot deadlock.
		sort.Slice(addressesSlice, func(i, j int) bool { return addressesSlice[i].Address < addressesSlice[j].Address })

		if len(addressesSlice) != 0 {
			if err := dbTransaction.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "address"}},
				DoUpdates: clause.AssignmentColumns([]string{"address"}),
			}).Create(addressesSlice).Error; err != nil {
				config.Log.Error("Error getting/creating addresses.", err)
				return err
			}
		}

		for _, address := range addressesSlice {
			uniqueAddress[address.Address] = address
		}

		var txesSlice []models.Tx
		for _, tx := range uniqueTxes {

			var signerAddressID uint

			if len(tx.SignerAddresses) != 0 {
				for addressIndex := range tx.SignerAddresses {
					signerAddressID = uniqueAddress[tx.SignerAddresses[addressIndex].Address].ID
					tx.SignerAddresses[addressIndex] = uniqueAddress[tx.SignerAddresses[addressIndex].Address]
					tx.SignerAddresses[addressIndex].ID = signerAddressID
				}
			}

			for feeIndex := range tx.Fees {
				tx.Fees[feeIndex].PayerAddressID = uniqueAddress[tx.Fees[feeIndex].PayerAddress.Address].ID
				tx.Fees[feeIndex].PayerAddress = uniqueAddress[tx.Fees[feeIndex].PayerAddress.Address]
			}
			txesSlice = append(txesSlice, tx)
		}

		if len(txesSlice) != 0 {
			if err := dbTransaction.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "hash"}},
				DoUpdates: clause.AssignmentColumns([]string{"code", "block_id"}),
			}).Create(txesSlice).Error; err != nil {
				config.Log.Error("Error getting/creating txes.", err)
				return err
			}
		}

		for _, tx := range txesSlice {
			uniqueTxes[tx.Hash] = tx
		}

		// Create unique message types and post-process them into the messages
		fullUniqueBlockMessageTypes, err := indexMessageTypes(dbTransaction, txs)
		if err != nil {
			return err
		}

		fullUniqueBlockMessageEventTypes := make(map[string]models.MessageEventType)
		if indexerConfig.Flags.IndexMessageEvents {
			fullUniqueBlockMessageEventTypes, err = indexMessageEventTypes(dbTransaction, txs)
			if err != nil {
				return err
			}
		}

		fullUniqueBlockMessageEventAttributeKeys := make(map[string]models.MessageEventAttributeKey)
		if indexerConfig.Flags.IndexMessageEvents {
			fullUniqueBlockMessageEventAttributeKeys, err = indexMessageEventAttributeKeys(dbTransaction, txs)
			if err != nil {
				return err
			}
		}

		// This complex set of loops is to ensure that foreign key relations are created and attached to downstream models before batch insertion is executed.
		// We are trading off in-app performance for batch insertion here and should consider complexity increase vs performance increase.
		for txIndex, tx := range txs {
			tx.Tx = uniqueTxes[tx.Tx.Hash]
			txs[txIndex].Tx = tx.Tx
			var messagesSlice []*models.Message
			for messageIndex := range tx.Messages {
				tx.Messages[messageIndex].Message.TxID = tx.Tx.ID
				tx.Messages[messageIndex].Message.Tx = tx.Tx
				tx.Messages[messageIndex].Message.MessageTypeID = fullUniqueBlockMessageTypes[tx.Messages[messageIndex].Message.MessageType.MessageType].ID

				tx.Messages[messageIndex].Message.MessageType = fullUniqueBlockMessageTypes[tx.Messages[messageIndex].Message.MessageType.MessageType]

				if indexerConfig.Flags.IndexMessageEvents {
					for eventIndex := range tx.Messages[messageIndex].MessageEvents {
						tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.MessageEventTypeID = fullUniqueBlockMessageEventTypes[tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.MessageEventType.Type].ID
						tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.MessageEventType = fullUniqueBlockMessageEventTypes[tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.MessageEventType.Type]

						for attributeIndex := range tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes {
							tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEventAttributeKeyID = fullUniqueBlockMessageEventAttributeKeys[tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEventAttributeKey.Key].ID
							tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEventAttributeKey = fullUniqueBlockMessageEventAttributeKeys[tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEventAttributeKey.Key]
						}
					}
				}

				if !indexerConfig.Flags.IndexTxMessageRaw {
					tx.Messages[messageIndex].Message.MessageBytes = nil
				}

				messagesSlice = append(messagesSlice, &tx.Messages[messageIndex].Message)
			}

			if len(messagesSlice) != 0 {
				if err := dbTransaction.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "tx_id"}, {Name: "message_index"}},
					DoUpdates: clause.AssignmentColumns([]string{"message_type_id", "message_bytes"}),
				}).Create(messagesSlice).Error; err != nil {
					config.Log.Error("Error getting/creating messages.", err)
					return err
				}
			}

			if indexerConfig.Flags.IndexMessageEvents {
				var messagesEventsSlice []*models.MessageEvent
				for messageIndex := range tx.Messages {
					for eventIndex := range tx.Messages[messageIndex].MessageEvents {
						tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.MessageID = tx.Messages[messageIndex].Message.ID
						tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.Message = tx.Messages[messageIndex].Message

						messagesEventsSlice = append(messagesEventsSlice, &tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent)
					}
				}

				if len(messagesEventsSlice) != 0 {
					if err := dbTransaction.Clauses(clause.OnConflict{
						Columns:   []clause.Column{{Name: "message_id"}, {Name: "index"}},
						DoUpdates: clause.AssignmentColumns([]string{"message_event_type_id"}),
					}).Create(messagesEventsSlice).Error; err != nil {
						config.Log.Error("Error getting/creating message events.", err)
						return err
					}
				}

				var messagesEventsAttributesSlice []*models.MessageEventAttribute
				for messageIndex := range tx.Messages {
					for eventIndex := range tx.Messages[messageIndex].MessageEvents {
						for attributeIndex := range tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes {
							tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEventID = tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent.ID
							tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex].MessageEvent = tx.Messages[messageIndex].MessageEvents[eventIndex].MessageEvent

							messagesEventsAttributesSlice = append(messagesEventsAttributesSlice, &tx.Messages[messageIndex].MessageEvents[eventIndex].Attributes[attributeIndex])
						}
					}
				}

				if len(messagesEventsAttributesSlice) != 0 {
					if err := dbTransaction.Clauses(clause.OnConflict{
						Columns:   []clause.Column{{Name: "message_event_id"}, {Name: "index"}},
						DoUpdates: clause.AssignmentColumns([]string{"value", "message_event_attribute_key_id"}),
					}).Create(messagesEventsAttributesSlice).Error; err != nil {
						config.Log.Error("Error getting/creating message event attributes.", err)
						return err
					}
				}
			}
		}

		return nil
	})

	// Contract: ensure that block and txs have been loaded with the indexed data before returning
	return block, txs, err
}

func indexMessageTypes(db *gorm.DB, txs []TxDBWrapper) (map[string]models.MessageType, error) {
	fullUniqueBlockMessageTypes := make(map[string]models.MessageType)
	for _, tx := range txs {
		for messageTypeKey, messageType := range tx.UniqueMessageTypes {
			fullUniqueBlockMessageTypes[messageTypeKey] = messageType
		}
	}

	var messageTypesSlice []models.MessageType
	for _, messageType := range fullUniqueBlockMessageTypes {
		messageTypesSlice = append(messageTypesSlice, messageType)
	}
	// Lock rows in a stable order so concurrent indexers cannot deadlock.
	sort.Slice(messageTypesSlice, func(i, j int) bool { return messageTypesSlice[i].MessageType < messageTypesSlice[j].MessageType })

	if len(messageTypesSlice) != 0 {
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "message_type"}},
			DoUpdates: clause.AssignmentColumns([]string{"message_type"}),
		}).Create(messageTypesSlice).Error; err != nil {
			config.Log.Error("Error getting/creating message types.", err)
			return nil, err
		}
	}

	for _, messageType := range messageTypesSlice {
		fullUniqueBlockMessageTypes[messageType.MessageType] = messageType
	}

	return fullUniqueBlockMessageTypes, nil
}

func indexMessageEventTypes(db *gorm.DB, txs []TxDBWrapper) (map[string]models.MessageEventType, error) {
	fullUniqueBlockMessageEventTypes := make(map[string]models.MessageEventType)

	for _, tx := range txs {
		for messageEventTypeKey, messageEventType := range tx.UniqueMessageEventTypes {
			fullUniqueBlockMessageEventTypes[messageEventTypeKey] = messageEventType
		}
	}

	var messageTypesSlice []models.MessageEventType
	for _, messageType := range fullUniqueBlockMessageEventTypes {
		messageTypesSlice = append(messageTypesSlice, messageType)
	}
	// Lock rows in a stable order so concurrent indexers cannot deadlock.
	sort.Slice(messageTypesSlice, func(i, j int) bool { return messageTypesSlice[i].Type < messageTypesSlice[j].Type })

	if len(messageTypesSlice) != 0 {
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "type"}},
			DoUpdates: clause.AssignmentColumns([]string{"type"}),
		}).Create(messageTypesSlice).Error; err != nil {
			config.Log.Error("Error getting/creating message event types.", err)
			return nil, err
		}
	}

	for _, messageType := range messageTypesSlice {
		fullUniqueBlockMessageEventTypes[messageType.Type] = messageType
	}

	return fullUniqueBlockMessageEventTypes, nil
}

func indexMessageEventAttributeKeys(db *gorm.DB, txs []TxDBWrapper) (map[string]models.MessageEventAttributeKey, error) {
	fullUniqueMessageEventAttributeKeys := make(map[string]models.MessageEventAttributeKey)

	for _, tx := range txs {
		for messageEventAttributeKey, messageEventAttribute := range tx.UniqueMessageAttributeKeys {
			fullUniqueMessageEventAttributeKeys[messageEventAttributeKey] = messageEventAttribute
		}
	}

	var messageEventAttributeKeysSlice []models.MessageEventAttributeKey
	for _, messageEventAttributeKey := range fullUniqueMessageEventAttributeKeys {
		messageEventAttributeKeysSlice = append(messageEventAttributeKeysSlice, messageEventAttributeKey)
	}
	// Lock rows in a stable order so concurrent indexers cannot deadlock.
	sort.Slice(messageEventAttributeKeysSlice, func(i, j int) bool {
		return messageEventAttributeKeysSlice[i].Key < messageEventAttributeKeysSlice[j].Key
	})

	if len(messageEventAttributeKeysSlice) != 0 {
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"key"}),
		}).Create(messageEventAttributeKeysSlice).Error; err != nil {
			config.Log.Error("Error getting/creating message event attribute keys.", err)
			return nil, err
		}
	}

	for _, messageEventAttributeKey := range messageEventAttributeKeysSlice {
		fullUniqueMessageEventAttributeKeys[messageEventAttributeKey.Key] = messageEventAttributeKey
	}

	return fullUniqueMessageEventAttributeKeys, nil
}

func IndexCustomMessages(conf config.IndexConfig, db *gorm.DB, dryRun bool, blockDBWrapper []TxDBWrapper, messageParserTrackers map[string]models.MessageParser) error {
	return db.Transaction(func(dbTransaction *gorm.DB) error {
		for _, tx := range blockDBWrapper {
			for _, message := range tx.Messages {
				if len(message.MessageParsedDatasets) != 0 {
					for _, parsedData := range message.MessageParsedDatasets {

						// Pre clear old errors
						if parsedData.Parser != nil {
							err := DeleteCustomMessageParserError(db, message.Message, messageParserTrackers[(*parsedData.Parser).Identifier()])
							if err != nil {
								config.Log.Error("Error clearing block event error.", err)
								return err
							}
						}

						if parsedData.Error == nil && parsedData.Data != nil && parsedData.Parser != nil {

							// to avoid an import cyrcle, this intermediate type was required. It may be possible to remove it by changing the belongs-to relation to a one-to-many relation on the two models.
							combinedEventsWithAttribues := []parsers.MessageEventWithAttributes{}
							for _, event := range message.MessageEvents {
								attrs := event.Attributes
								combinedEventsWithAttribues = append(combinedEventsWithAttribues, parsers.MessageEventWithAttributes{Event: event.MessageEvent, Attributes: attrs})
							}
							err := (*parsedData.Parser).IndexMessage(parsedData.Data, dbTransaction, message.Message, combinedEventsWithAttribues, conf)
							if err != nil {
								config.Log.Error("Error indexing message.", err)
								return err
							}
						} else if parsedData.Error != nil {
							err := CreateMessageParserError(db, message.Message, messageParserTrackers[(*parsedData.Parser).Identifier()], parsedData.Error)
							if err != nil {
								config.Log.Error("Error inserting message parser error.", err)
								return err
							}
						}
					}
				}
			}
		}

		return nil
	})
}
