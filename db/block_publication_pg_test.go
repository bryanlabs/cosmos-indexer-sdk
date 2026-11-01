package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Embed the unused ParseMessage method: these tests inject already parsed data
// at the same writer boundary used by DoDBUpdates.
type publicationParser struct {
	parsers.MessageParser
	fail       bool
	failCommit bool
	calls      int
}

func (p *publicationParser) Identifier() string { return "atomic-publication-test" }

func (p *publicationParser) InvalidateMessage(database *gorm.DB, message models.Message) error {
	return database.Exec("DELETE FROM pg_temp.taxable_events WHERE message_id = ?", message.ID).Error
}

func (p *publicationParser) IndexMessage(_ *any, database *gorm.DB, message models.Message, _ []parsers.MessageEventWithAttributes, _ config.IndexConfig) error {
	p.calls++
	if err := p.InvalidateMessage(database, message); err != nil {
		return err
	}
	if err := database.Exec("INSERT INTO pg_temp.taxable_events (message_id, amount) VALUES (?, 22)", message.ID).Error; err != nil {
		return err
	}
	if p.fail {
		return database.Exec("SELECT 1 / 0").Error
	}
	if p.failCommit {
		return database.Exec("INSERT INTO pg_temp.commit_guard (block_id) VALUES (999999)").Error
	}
	return nil
}

func publicationDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres atomic publication tests")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)
	// No AutoMigrate, public schema reads, or persistent DDL/data. Every table
	// touched by the actual pipeline is connection-local, with real FK checks.
	for _, statement := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE chains (id bigserial PRIMARY KEY, chain_id text UNIQUE, name text)",
		"CREATE TEMP TABLE addresses (id bigserial PRIMARY KEY, address text UNIQUE)",
		"CREATE TEMP TABLE denoms (id bigserial PRIMARY KEY, base text UNIQUE)",
		`CREATE TEMP TABLE blocks (id bigserial PRIMARY KEY, height bigint, chain_id bigint REFERENCES chains,
 time_stamp timestamptz, proposer_cons_address_id bigint REFERENCES addresses,
 tx_indexed boolean DEFAULT false, block_events_indexed boolean DEFAULT false, UNIQUE (height, chain_id))`,
		`CREATE TEMP TABLE failed_blocks (id bigserial PRIMARY KEY, height bigint, blockchain_id bigint REFERENCES chains,
 attempts bigint DEFAULT 0, last_attempted_at timestamptz, last_error text)`,
		"CREATE TEMP TABLE txes (id bigserial PRIMARY KEY, hash text UNIQUE, code bigint, block_id bigint REFERENCES blocks, memo text)",
		`CREATE TEMP TABLE tx_signer_addresses (tx_id bigint REFERENCES txes, address_id bigint REFERENCES addresses,
 PRIMARY KEY (tx_id, address_id))`,
		`CREATE TEMP TABLE fees (id bigserial PRIMARY KEY, tx_id bigint REFERENCES txes, denomination_id bigint REFERENCES denoms,
 amount numeric(78,0), payer_address_id bigint REFERENCES addresses, UNIQUE (tx_id, denomination_id))`,
		"CREATE TEMP TABLE message_types (id bigserial PRIMARY KEY, message_type text UNIQUE)",
		`CREATE TEMP TABLE messages (id bigserial PRIMARY KEY, tx_id bigint REFERENCES txes, message_index bigint,
 message_type_id bigint REFERENCES message_types, message_bytes bytea, opaque boolean DEFAULT false, UNIQUE (tx_id, message_index))`,
		"CREATE TEMP TABLE message_event_types (id bigserial PRIMARY KEY, type text UNIQUE)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigserial PRIMARY KEY, key text UNIQUE)",
		`CREATE TEMP TABLE message_events (id bigserial PRIMARY KEY, message_id bigint REFERENCES messages,
 "index" bigint, message_event_type_id bigint REFERENCES message_event_types, UNIQUE (message_id, "index"))`,
		`CREATE TEMP TABLE message_event_attributes (id bigserial PRIMARY KEY, message_event_id bigint REFERENCES message_events,
 "index" bigint, message_event_attribute_key_id bigint REFERENCES message_event_attribute_keys, value text,
 UNIQUE (message_event_id, "index"))`,
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, message_id bigint REFERENCES messages, amount numeric)",
		"CREATE TEMP TABLE commit_guard (block_id bigint REFERENCES blocks DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TEMP TABLE message_parser_errors (id bigserial PRIMARY KEY, message_id bigint REFERENCES messages, message_parser_id bigint, error text)",
		"INSERT INTO pg_temp.chains (id, chain_id, name) VALUES (1, 'atomic-test', 'Atomic test')",
	} {
		require.NoError(t, database.Exec(statement).Error)
	}
	return database
}

func publicationInput(key string, parser *publicationParser) (models.Block, []TxDBWrapper, config.IndexConfig, map[string]models.MessageParser) {
	var data any = "parsed"
	var implementation parsers.MessageParser = parser
	messageType := models.MessageType{MessageType: key}
	eventType := models.MessageEventType{Type: key}
	attributeKey := models.MessageEventAttributeKey{Key: key}
	block := models.Block{Height: 100, ChainID: 1, TimeStamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), ProposerConsAddress: models.Address{Address: "proposer"}}
	txs := []TxDBWrapper{{
		Tx:                         models.Tx{Hash: "atomic-tx", SignerAddresses: []models.Address{{Address: "signer"}}, Fees: []models.Fee{{Amount: decimal.NewFromInt(22), Denomination: models.Denom{Base: "uatom"}, PayerAddress: models.Address{Address: "new-payer"}}}},
		UniqueMessageTypes:         map[string]models.MessageType{key: messageType},
		UniqueMessageEventTypes:    map[string]models.MessageEventType{key: eventType},
		UniqueMessageAttributeKeys: map[string]models.MessageEventAttributeKey{key: attributeKey},
		Messages: []MessageDBWrapper{{
			Message:               models.Message{MessageType: messageType, MessageBytes: []byte("new raw")},
			MessageEvents:         []MessageEventDBWrapper{{MessageEvent: models.MessageEvent{MessageEventType: eventType}, Attributes: []models.MessageEventAttribute{{MessageEventAttributeKey: attributeKey, Value: "new evidence"}}}},
			MessageParsedDatasets: []parsers.MessageParsedData{{Data: &data, Parser: &implementation}},
		}},
	}}
	conf := config.IndexConfig{}
	conf.Flags.IndexMessageEvents = true
	conf.Flags.IndexTxMessageRaw = true
	return block, txs, conf, map[string]models.MessageParser{parser.Identifier(): {ID: 7}}
}

func publicationCount(t *testing.T, database *gorm.DB, table string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, database.Table("pg_temp."+table).Count(&count).Error)
	return count
}

func publicationSnapshot(t *testing.T, database *gorm.DB) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, table := range []string{"chains", "addresses", "denoms", "blocks", "failed_blocks", "txes", "tx_signer_addresses", "fees", "messages", "message_types", "message_event_types", "message_event_attribute_keys", "message_events", "message_event_attributes", "taxable_events", "message_parser_errors"} {
		var rows string
		require.NoError(t, database.Raw("SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]'::jsonb)::text FROM pg_temp."+table+" t").Scan(&rows).Error)
		state[table] = rows
	}
	return state
}

func assertNoPublicationIDs(t *testing.T, key string) {
	t.Helper()
	for _, cache := range []*idCache{messageTypeIDs, messageEventTypeIDs, attributeKeyIDs} {
		_, present := cache.get(key)
		require.False(t, present, "rolled back IDs must never enter the process cache")
	}
}

func TestIndexBlockDataAtomicPostgres(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay=%v", replay), func(t *testing.T) {
			database := publicationDatabase(t)
			key := fmt.Sprintf("atomic-%s-%d", t.Name(), time.Now().UnixNano())
			parser := &publicationParser{}
			block, txs, conf, trackers := publicationInput(key, parser)
			if replay {
				oldBlock, oldTxs, oldConf, oldTrackers := publicationInput(key+"-old", parser)
				oldTxs[0].Tx.Fees[0].Amount = decimal.NewFromInt(11)
				oldTxs[0].Tx.Fees[0].PayerAddress.Address = "old-payer"
				oldTxs[0].Messages[0].Message.MessageBytes = []byte("old raw")
				indexed, _, err := IndexBlockData(database, oldBlock, oldTxs, oldConf, oldTrackers)
				require.NoError(t, err)
				require.NotZero(t, indexed.ID)
				require.NoError(t, database.Exec("UPDATE pg_temp.taxable_events SET amount=11").Error)
				require.NoError(t, database.Exec("INSERT INTO pg_temp.message_parser_errors (message_id,message_parser_id,error) SELECT id,7,'old diagnostic' FROM pg_temp.messages").Error)
			}
			require.NoError(t, database.Exec("INSERT INTO pg_temp.failed_blocks (height,blockchain_id) VALUES (100,1)").Error)
			parser.fail = true
			priorPublication := publicationSnapshot(t, database)
			before := cloneBlockTransactions(txs)
			callsBeforeFailure := parser.calls
			_, returned, err := IndexBlockData(database, block, txs, conf, trackers)
			require.Equal(t, callsBeforeFailure+1, parser.calls, "fault must reach the second-stage writer")
			require.ErrorContains(t, err, "division by zero")
			require.Equal(t, priorPublication, publicationSnapshot(t, database), "raw evidence, fees, financial rows, errors and completion must all roll back together")
			require.Equal(t, before, txs, "failed raw-stage ID assignments must not mutate retry input")
			require.Equal(t, before, returned)
			assertNoPublicationIDs(t, key)
			require.Equal(t, int64(1), publicationCount(t, database, "failed_blocks"))
			heights, err := FailedBlockRetryHeights(database, 1, time.Minute, 5, 1, 10)
			require.NoError(t, err)
			require.Equal(t, []int64{100}, heights, "restart retry selection must retain the failed height")
			window, err := GetBlockIndexWindow(database, 1, 100, 100)
			require.NoError(t, err)
			if replay {
				require.Len(t, window, 1)
				require.True(t, window[0].TxIndexed, "only the coherent prior publication remains visible")
				var raw models.Message
				require.NoError(t, database.Select("message_bytes").First(&raw).Error)
				require.Equal(t, []byte("old raw"), raw.MessageBytes)
				var fee, financial string
				require.NoError(t, database.Raw("SELECT amount::text FROM pg_temp.fees").Scan(&fee).Error)
				require.NoError(t, database.Raw("SELECT amount::text FROM pg_temp.taxable_events").Scan(&financial).Error)
				require.Equal(t, "11", fee)
				require.Equal(t, "11", financial)
				require.Equal(t, int64(1), publicationCount(t, database, "message_parser_errors"))
			} else {
				require.Empty(t, window, "a new failed block must not be published complete")
				for _, table := range []string{"blocks", "txes", "fees", "messages", "message_types", "message_event_types", "message_event_attribute_keys", "message_events", "message_event_attributes", "taxable_events", "message_parser_errors"} {
					require.Zero(t, publicationCount(t, database, table), table)
				}
			}
			// Retry the exact same input after the second-stage fault. Real FK
			// constraints reject any phantom cached IDs from the aborted insert.
			parser.fail = false
			indexed, dataset, err := IndexBlockData(database, block, txs, conf, trackers)
			require.NoError(t, err)
			require.True(t, indexed.TxIndexed)
			require.NotZero(t, dataset[0].Messages[0].Message.ID)
			require.Zero(t, publicationCount(t, database, "failed_blocks"))
			require.Zero(t, publicationCount(t, database, "message_parser_errors"))
			require.Equal(t, int64(1), publicationCount(t, database, "taxable_events"))
			var published struct{ Raw, Fee, Financial, Payer, Evidence string }
			require.NoError(t, database.Raw(`SELECT convert_from(m.message_bytes,'UTF8') AS raw, f.amount::text AS fee,
 te.amount::text AS financial, a.address AS payer, mea.value AS evidence FROM pg_temp.messages m
 JOIN pg_temp.taxable_events te ON te.message_id=m.id JOIN pg_temp.fees f ON f.tx_id=m.tx_id
 JOIN pg_temp.addresses a ON a.id=f.payer_address_id JOIN pg_temp.message_events me ON me.message_id=m.id
 JOIN pg_temp.message_event_attributes mea ON mea.message_event_id=me.id`).Scan(&published).Error)
			require.Equal(t, "new raw", published.Raw)
			require.Equal(t, "22", published.Fee)
			require.Equal(t, "22", published.Financial)
			require.Equal(t, "new-payer", published.Payer)
			require.Equal(t, "new evidence", published.Evidence)
			for _, cache := range []*idCache{messageTypeIDs, messageEventTypeIDs, attributeKeyIDs} {
				_, present := cache.get(key)
				require.True(t, present, "successful outer commit should warm lookup caches")
			}
			// Incomplete classification is a committed outcome, not a DB failure:
			// remove prior financial rows and atomically publish its diagnostic.
			txs[0].Messages[0].MessageParsedDatasets[0].Error = errors.New("classification incomplete: synthetic unresolved evidence")
			require.NoError(t, database.Exec("INSERT INTO pg_temp.failed_blocks (height,blockchain_id) VALUES (100,1)").Error)
			_, _, err = IndexBlockData(database, block, txs, conf, trackers)
			require.NoError(t, err)
			require.Zero(t, publicationCount(t, database, "taxable_events"))
			require.Equal(t, int64(1), publicationCount(t, database, "message_parser_errors"))
			require.Zero(t, publicationCount(t, database, "failed_blocks"))
			var diagnostic string
			require.NoError(t, database.Raw("SELECT error FROM pg_temp.message_parser_errors").Scan(&diagnostic).Error)
			require.Contains(t, diagnostic, "classification incomplete")
		})
	}
}

func TestIndexBlockDataCallerRollbackPostgres(t *testing.T) {
	database := publicationDatabase(t)
	key := fmt.Sprintf("outer-rollback-%d", time.Now().UnixNano())
	parser := &publicationParser{}
	block, txs, conf, trackers := publicationInput(key, parser)
	abort := errors.New("caller rolls back after savepoint release")
	err := database.Transaction(func(transaction *gorm.DB) error {
		_, _, err := IndexBlockData(transaction, block, txs, conf, trackers)
		require.NoError(t, err)
		assertNoPublicationIDs(t, key)
		return abort
	})
	require.ErrorIs(t, err, abort)
	assertNoPublicationIDs(t, key)
	require.Zero(t, publicationCount(t, database, "blocks"))
	_, _, err = IndexBlockData(database, block, txs, conf, trackers)
	require.NoError(t, err, "retry after caller rollback must not reuse nonexistent lookup IDs")
}

func TestIndexBlockDataCommitFailurePostgres(t *testing.T) {
	database := publicationDatabase(t)
	key := fmt.Sprintf("commit-failure-%d", time.Now().UnixNano())
	parser := &publicationParser{failCommit: true}
	block, txs, conf, trackers := publicationInput(key, parser)
	prior := publicationSnapshot(t, database)
	_, _, err := IndexBlockData(database, block, txs, conf, trackers)
	require.ErrorContains(t, err, "violates foreign key constraint")
	require.Equal(t, 1, parser.calls, "the parser should succeed before the outer commit fails")
	require.Equal(t, prior, publicationSnapshot(t, database))
	assertNoPublicationIDs(t, key)
	parser.failCommit = false
	_, _, err = IndexBlockData(database, block, txs, conf, trackers)
	require.NoError(t, err, "a failed outer commit must not poison subsequent lookup IDs")
}
