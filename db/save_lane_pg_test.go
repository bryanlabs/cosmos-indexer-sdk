package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	indexdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	sdk "github.com/cosmos/cosmos-sdk/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	distribution "github.com/cosmos/cosmos-sdk/x/distribution/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	transfer "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Explicitly separate from production and from COSMOS_TAX_TEST_DSN. This suite
// creates and drops a schema, and accepts only an isolated loopback PostgreSQL.
func saveLaneDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_DB_SAVE_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_DB_SAVE_TEST_DSN to an isolated loopback PostgreSQL")
	}
	parsed, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "::1"}, parsed.Host)
	require.Equal(t, uint16(16439), parsed.Port, "reserved isolated test server, never the production forward")
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	schema := fmt.Sprintf("dbsave_%d", time.Now().UnixNano())
	require.NoError(t, database.Exec("CREATE SCHEMA "+schema).Error)
	require.NoError(t, database.Exec("SET search_path TO "+schema).Error)
	t.Cleanup(func() {
		require.NoError(t, database.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, indexdb.MigrateModels(database))
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}))
	require.NoError(t, database.Create(&models.Chain{ID: 1, ChainID: "synthetic-2022", Name: "Synthetic 2022"}).Error)
	require.NoError(t, database.Create(&models.MessageParser{ID: 7, Identifier: "save-lane-tax"}).Error)
	return database
}

type saveLaneBlock struct {
	block models.Block
	txs   []indexdb.TxDBWrapper
}

// Writer-boundary synthetic 2022 mix, not an archive replay or a measured chain
// frequency distribution. Parse real SDK bank/reward/delegate messages before
// timing. Include failed/empty txs, multi-message txs, multi-event classifications,
// opaque retained evidence, incomplete classifications, and a 200-tx block.
func saveLaneInput(t *testing.T, blocks int) []saveLaneBlock {
	t.Helper()
	prefix := t.Name()
	var parser parsers.MessageParser = &tax.Parser{ID: "save-lane-tax"}
	out := make([]saveLaneBlock, blocks)
	for b := range out {
		block := models.Block{Height: int64(9475110 + b), ChainID: 1, TimeStamp: time.Date(2022, 1, 1, 0, 0, b*7, 0, time.UTC), ProposerConsAddress: models.Address{Address: prefix + "-proposer"}}
		out[b].block = block
		count := []int{0, 1, 4, 12, 32, 80, 200}[b%7]
		for x := 0; x < count; x++ {
			tx := indexdb.TxDBWrapper{Tx: models.Tx{Hash: fmt.Sprintf("%s-%d-%d", prefix, b, x), Memo: "synthetic 2022", SignerAddresses: []models.Address{{Address: prefix + "-signer"}}, Fees: []models.Fee{{Amount: decimal.NewFromInt(2500), Denomination: models.Denom{Base: "uatom"}, PayerAddress: models.Address{Address: prefix + "-payer"}}}}, UniqueMessageTypes: map[string]models.MessageType{}, UniqueMessageEventTypes: map[string]models.MessageEventType{}, UniqueMessageAttributeKeys: map[string]models.MessageEventAttributeKey{}}
			if x%19 == 18 {
				tx.Tx.Code = 5
				out[b].txs = append(out[b].txs, tx)
				continue
			}
			messages := 1
			if x%7 == 0 {
				messages = 3
			}
			for m := 0; m < messages; m++ {
				var msg sdk.Msg
				var log txlog.LogMessage
				switch (x + m) % 6 {
				case 0:
					msg = &bank.MsgSend{FromAddress: prefix + "-sender", ToAddress: prefix + "-receiver", Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 123), sdk.NewInt64Coin("uother", 3))}
				case 1:
					msg = &distribution.MsgWithdrawDelegatorReward{DelegatorAddress: prefix + "-delegator", ValidatorAddress: prefix + "-validator"}
					log.Events = []txlog.LogMessageEvent{{Type: "withdraw_rewards", Attributes: []txlog.Attribute{{Key: "amount", Value: "42uatom"}, {Key: "validator", Value: prefix + "-validator"}}}}
				case 2:
					msg = &staking.MsgDelegate{DelegatorAddress: prefix + "-delegator", ValidatorAddress: prefix + "-validator", Amount: sdk.NewInt64Coin("uatom", 1000)}
					log.Events = []txlog.LogMessageEvent{{Type: "delegate", Attributes: []txlog.Attribute{{Key: "amount", Value: "1000uatom"}, {Key: "validator", Value: prefix + "-validator"}}}}
				case 3:
					msg = &bank.MsgMultiSend{Inputs: []bank.Input{{Address: prefix + "-sender"}}, Outputs: []bank.Output{{Address: prefix + "-receiver", Coins: sdk.NewCoins(sdk.NewInt64Coin("uatom", 99))}, {Address: prefix + "-other", Coins: sdk.NewCoins(sdk.NewInt64Coin("uatom", 24))}}}
				case 4:
					msg = &transfer.MsgTransfer{SourcePort: "transfer", SourceChannel: "channel-0", Sender: prefix + "-sender", Receiver: prefix + "-remote", Token: sdk.NewInt64Coin("uatom", 100)}
				case 5:
					msg = &distribution.MsgSetWithdrawAddress{DelegatorAddress: prefix + "-delegator", WithdrawAddress: prefix + "-receiver"}
				}
				data, parseErr := parser.ParseMessage(msg, &log, config.IndexConfig{})
				require.NoError(t, parseErr)
				mt := models.MessageType{MessageType: prefix + sdk.MsgTypeURL(msg)}
				et := models.MessageEventType{Type: prefix + "-transfer"}
				ak := models.MessageEventAttributeKey{Key: prefix + "-amount"}
				message := indexdb.MessageDBWrapper{Message: models.Message{MessageIndex: m, MessageType: mt, MessageBytes: []byte(fmt.Sprintf("raw-%d-%d-%d", b, x, m))}, MessageEvents: []indexdb.MessageEventDBWrapper{{MessageEvent: models.MessageEvent{MessageEventType: et}, Attributes: []models.MessageEventAttribute{{MessageEventAttributeKey: ak, Value: "123uatom"}}}}, MessageParsedDatasets: []parsers.MessageParsedData{{Data: data, Parser: &parser}}}
				if (x+m)%6 == 5 {
					message.MessageParsedDatasets = nil
				}
				// Ordered repeated attribute keys and multiple events exercise the actual
				// FK assignment path, not only a single scalar parent reference.
				template := message.MessageEvents[0]
				message.MessageEvents = nil
				for e := 0; e < 3; e++ {
					event := indexdb.MessageEventDBWrapper{MessageEvent: models.MessageEvent{Index: uint64(e), MessageEventType: et}}
					for a := 0; a < 6; a++ {
						attr := template.Attributes[0]
						attr.Index = uint64(a)
						attr.Value = fmt.Sprintf("%d-%d-%d-%d-%d", b, x, m, e, a)
						event.Attributes = append(event.Attributes, attr)
					}
					message.MessageEvents = append(message.MessageEvents, event)
				}
				if x%23 == 22 {
					if len(message.MessageParsedDatasets) == 0 {
						message.MessageParsedDatasets = []parsers.MessageParsedData{{Data: data, Parser: &parser}}
					}
					message.Message.Opaque = true
					message.PreserveOpaqueEvidence = true
					message.MessageParsedDatasets[0].Error = errors.New("classification incomplete: synthetic opaque evidence")
				}
				tx.Messages = append(tx.Messages, message)
				tx.UniqueMessageTypes[mt.MessageType] = mt
				tx.UniqueMessageEventTypes[et.Type] = et
				tx.UniqueMessageAttributeKeys[ak.Key] = ak
			}
			out[b].txs = append(out[b].txs, tx)
		}
	}
	return out
}

func saveLaneConfig() config.IndexConfig {
	conf := config.IndexConfig{}
	conf.Flags.IndexMessageEvents = true
	conf.Flags.IndexTxMessageRaw = true
	conf.Flags.IndexEmptyTransactions = true
	return conf
}

func saveLaneWrite(t *testing.T, database *gorm.DB, input []saveLaneBlock, conf config.IndexConfig, events bool) error {
	t.Helper()
	trackers := map[string]models.MessageParser{"save-lane-tax": {ID: 7}}
	for _, item := range input {
		_, _, err := indexdb.IndexBlockData(database, item.block, item.txs, conf, trackers)
		if err != nil {
			return err
		}
		if events {
			block := item.block
			wrapper := &indexdb.BlockDBWrapper{Block: &block}
			indexed, err := indexdb.IndexBlockEvents(database, false, wrapper, "")
			if err != nil {
				return err
			}
			if err := indexdb.IndexCustomBlockEvents(conf, database, false, indexed, "", nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

var saveLaneTables = []string{"chains", "addresses", "denoms", "blocks", "failed_blocks", "failed_event_blocks", "txes", "tx_signer_addresses", "fees", "message_types", "messages", "failed_txes", "failed_messages", "message_events", "message_event_types", "message_event_attributes", "message_event_attribute_keys", "message_parsers", "message_parser_errors", "block_events", "block_event_types", "block_event_attributes", "block_event_attribute_keys", "block_event_parsers", "block_event_parser_errors", "taxable_events"}

// Hash every field of every persisted row. Replace generated IDs/FKs with full
// referenced rows and sort, because Go's existing tx map traversal allocates
// nondeterministic surrogate IDs. Evidence indices, multiplicity, flags, error
// strings and every financial field remain in the fingerprint.
func saveLaneSnapshot(t *testing.T, database *gorm.DB) (string, map[string]int) {
	t.Helper()
	all := map[string][]map[string]any{}
	ids := map[string]map[string]map[string]any{}
	counts := map[string]int{}
	for _, table := range saveLaneTables {
		var raw string
		require.NoError(t, database.Raw("SELECT COALESCE(jsonb_agg(to_jsonb(t)), '[]'::jsonb)::text FROM "+table+" t").Scan(&raw).Error)
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&allRows{target: &all, table: table}))
		counts[table] = len(all[table])
		ids[table] = map[string]map[string]any{}
		for _, row := range all[table] {
			if id, ok := row["id"]; ok {
				ids[table][fmt.Sprint(id)] = row
			}
		}
	}
	foreign := map[string]string{"chain_id": "chains", "blockchain_id": "chains", "proposer_cons_address_id": "addresses", "block_id": "blocks", "tx_id": "txes", "address_id": "addresses", "payer_address_id": "addresses", "denomination_id": "denoms", "message_type_id": "message_types", "message_id": "messages", "message_event_id": "message_events", "message_event_type_id": "message_event_types", "message_event_attribute_key_id": "message_event_attribute_keys", "message_parser_id": "message_parsers", "block_event_id": "block_events", "block_event_type_id": "block_event_types", "block_event_attribute_key_id": "block_event_attribute_keys", "block_event_parser_id": "block_event_parsers"}
	var normalize func(string, map[string]any) map[string]any
	normalize = func(table string, row map[string]any) map[string]any {
		result := map[string]any{}
		for key, value := range row {
			if key == "id" {
				continue
			}
			target, ok := foreign[key]
			if ok && !(table == "chains" && key == "chain_id") && value != nil {
				reference, found := ids[target][fmt.Sprint(value)]
				require.True(t, found, "missing FK %s.%s=%v", table, key, value)
				value = normalize(target, reference)
			}
			result[key] = value
		}
		return result
	}
	hash := sha256.New()
	tableFingerprints := map[string]string{}
	for _, table := range saveLaneTables {
		rows := []string{}
		for _, row := range all[table] {
			encoded, err := json.Marshal(normalize(table, row))
			require.NoError(t, err)
			rows = append(rows, string(encoded))
		}
		sort.Strings(rows)
		fmt.Fprintln(hash, table)
		tableHash := sha256.New()
		fmt.Fprintln(tableHash, table)
		for _, row := range rows {
			fmt.Fprintln(hash, row)
			fmt.Fprintln(tableHash, row)
		}
		tableFingerprints[table] = hex.EncodeToString(tableHash.Sum(nil))
	}
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	if prefix := os.Getenv("COSMOS_SNAPSHOT_FINGERPRINT_FILE"); prefix != "" {
		encoded, err := json.MarshalIndent(map[string]any{"fingerprint": fingerprint, "tables": tableFingerprints, "counts": counts}, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(prefix+"."+fingerprint+".json", encoded, 0600))
	}
	return fingerprint, counts
}

// Decode through a small adapter to keep JSON numbers exact, including heights.
type allRows struct {
	target *map[string][]map[string]any
	table  string
}

func (a *allRows) UnmarshalJSON(data []byte) error {
	var rows []map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		return err
	}
	(*a.target)[a.table] = rows
	return nil
}

type saveLaneTrace struct {
	logger.Interface
	counts map[string]int
}

func (l *saveLaneTrace) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	fields := strings.Fields(sql)
	if len(fields) > 0 {
		l.counts[strings.ToUpper(fields[0])]++
	}
}

// Not testing.B: a fixed workload and explicit elapsed duration permit identical
// baseline/candidate test binaries and retain full post-write fingerprints.
func TestDBSaveMeasuredWorkloadPostgres(t *testing.T) {
	database := saveLaneDatabase(t)
	input := saveLaneInput(t, 28)
	trace := &saveLaneTrace{Interface: logger.Default.LogMode(logger.Silent), counts: map[string]int{}}
	measured := database.Session(&gorm.Session{Logger: trace})
	if os.Getenv("COSMOS_DB_SAVE_SQL_LOG") == "1" {
		require.NoError(t, database.Exec("SET log_statement TO 'all'").Error)
	}
	started := time.Now()
	require.NoError(t, saveLaneWrite(t, measured, input, saveLaneConfig(), true))
	elapsed := time.Since(started)
	if os.Getenv("COSMOS_DB_SAVE_SQL_LOG") == "1" {
		require.NoError(t, database.Exec("SET log_statement TO 'none'").Error)
	}
	fingerprint, counts := saveLaneSnapshot(t, database)
	t.Logf("MEASURE elapsed_ns=%d statements=%v rows=%v fingerprint=%s", elapsed.Nanoseconds(), trace.counts, counts, fingerprint)
	// Replay identical input. Surrogate sequences may advance, persisted rows may not.
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), true))
	again, againCounts := saveLaneSnapshot(t, database)
	require.Equal(t, counts, againCounts)
	require.Equal(t, fingerprint, again, "idempotent retry must preserve every semantic field")
}

func TestDBSaveClearFailureRollbackPostgres(t *testing.T) {
	for _, sqlFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("sql=%v", sqlFailure), func(t *testing.T) {
			database := saveLaneDatabase(t)
			input := saveLaneInput(t, 2)[1:]
			require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
			require.NoError(t, database.Exec("INSERT INTO message_parser_errors(message_id,message_parser_id,error) SELECT id,7,'prior diagnostic' FROM messages").Error)
			before, _ := saveLaneSnapshot(t, database)
			injected := errors.New("injected after diagnostic deletion")
			if sqlFailure {
				require.NoError(t, database.Exec("CREATE FUNCTION reject_diagnostic_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected SQL delete failure'; END $$").Error)
				require.NoError(t, database.Exec("CREATE TRIGGER reject_delete BEFORE DELETE ON message_parser_errors FOR EACH ROW EXECUTE FUNCTION reject_diagnostic_delete()").Error)
			} else {
				require.NoError(t, database.Callback().Delete().After("gorm:delete").Register("save_lane_delete_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "message_parser_errors" {
						tx.AddError(injected)
					}
				}))
			}
			err := saveLaneWrite(t, database, input, saveLaneConfig(), false)
			if sqlFailure {
				require.ErrorContains(t, err, "injected SQL delete failure")
			} else {
				require.ErrorIs(t, err, injected)
			}
			after, _ := saveLaneSnapshot(t, database)
			require.Equal(t, before, after, "failed preclear must roll back raw evidence, diagnostics and classifications")
			if sqlFailure {
				require.NoError(t, database.Exec("DROP TRIGGER reject_delete ON message_parser_errors").Error)
			} else {
				require.NoError(t, database.Callback().Delete().Remove("save_lane_delete_failure"))
			}
			require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
			retry, _ := saveLaneSnapshot(t, database)
			require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
			twice, _ := saveLaneSnapshot(t, database)
			require.Equal(t, retry, twice)
		})
	}
}

func TestDBSaveAttributeFailureRollbackPostgres(t *testing.T) {
	database := saveLaneDatabase(t)
	input := saveLaneInput(t, 2)[1:]
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
	before, _ := saveLaneSnapshot(t, database)
	input[0].txs[0].Tx.Fees[0].Amount = decimal.NewFromInt(999)
	input[0].txs[0].Messages[0].Message.MessageBytes = []byte("replacement raw")
	input[0].txs[0].Messages[0].MessageEvents[0].Attributes[0].Value = "replacement evidence"
	injected := errors.New("injected after attribute upsert")
	require.NoError(t, database.Callback().Create().After("gorm:create").Register("save_lane_attribute_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "message_event_attributes" {
			tx.AddError(injected)
		}
	}))
	require.ErrorIs(t, saveLaneWrite(t, database, input, saveLaneConfig(), false), injected)
	after, _ := saveLaneSnapshot(t, database)
	require.Equal(t, before, after, "attribute-stage failure must roll back all earlier raw and fee writes")
	require.NoError(t, database.Callback().Create().Remove("save_lane_attribute_failure"))
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
	retry, _ := saveLaneSnapshot(t, database)
	require.NotEqual(t, before, retry)
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
	again, _ := saveLaneSnapshot(t, database)
	require.Equal(t, retry, again)
}

func TestDBSaveRepeatedInclusionAndStaleSuffixPostgres(t *testing.T) {
	database := saveLaneDatabase(t)
	input := saveLaneInput(t, 3)[1:]
	// Same hash at two heights, and twice in the later block. Existing serial
	// message upserts and parser calls must keep their last-inclusion semantics.
	input[1].txs = []indexdb.TxDBWrapper{input[0].txs[0], input[0].txs[0]}
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
	var height int64
	require.NoError(t, database.Raw("SELECT b.height FROM txes t JOIN blocks b ON b.id=t.block_id").Scan(&height).Error)
	require.Equal(t, input[1].block.Height, height)
	before, counts := saveLaneSnapshot(t, database)
	require.Equal(t, 1, counts["txes"])
	require.NoError(t, saveLaneWrite(t, database, input, saveLaneConfig(), false))
	after, _ := saveLaneSnapshot(t, database)
	require.Equal(t, before, after)
	for i := range input[1].txs {
		for m := range input[1].txs[i].Messages {
			if len(input[1].txs[i].Messages[m].MessageParsedDatasets) == 0 {
				continue
			}
			var empty any = []tax.TaxableEvent{}
			input[1].txs[i].Messages[m].MessageParsedDatasets[0].Data = &empty
		}
	}
	require.NoError(t, saveLaneWrite(t, database, input[1:], saveLaneConfig(), false))
	emptyFingerprint, emptyCounts := saveLaneSnapshot(t, database)
	require.Zero(t, emptyCounts["taxable_events"], "empty successful reclassification removes every stale suffix")
	t.Logf("REPEATED fingerprint=%s empty_fingerprint=%s", before, emptyFingerprint)
}

func TestDBSaveDisabledEvidencePostgres(t *testing.T) {
	database := saveLaneDatabase(t)
	input := saveLaneInput(t, 4)[3:]
	require.NoError(t, saveLaneWrite(t, database, input, config.IndexConfig{}, false))
	fingerprint, counts := saveLaneSnapshot(t, database)
	require.Zero(t, counts["message_events"])
	require.Zero(t, counts["message_event_attributes"])
	require.Positive(t, counts["taxable_events"])
	var raw int64
	require.NoError(t, database.Table("messages").Where("message_bytes IS NOT NULL").Count(&raw).Error)
	require.Zero(t, raw)
	t.Logf("DISABLED fingerprint=%s", fingerprint)
}

func TestDBSavePartialFlagsPostgres(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			database := saveLaneDatabase(t)
			conf := config.IndexConfig{}
			conf.Flags.IndexTxMessageRaw = mask&1 != 0
			conf.Flags.IndexMessageEvents = mask&2 != 0
			conf.Flags.IndexEmptyTransactions = mask&4 != 0
			input := saveLaneInput(t, 5)[4:]
			require.NoError(t, saveLaneWrite(t, database, input, conf, false))
			before, counts := saveLaneSnapshot(t, database)
			require.Positive(t, counts["taxable_events"])
			var flags struct{ TxIndexed, BlockEventsIndexed bool }
			require.NoError(t, database.Table("blocks").Take(&flags).Error)
			require.True(t, flags.TxIndexed)
			require.False(t, flags.BlockEventsIndexed)
			var opaque int64
			require.NoError(t, database.Table("messages").Where("opaque AND message_bytes IS NOT NULL").Count(&opaque).Error)
			require.Positive(t, opaque, "opaque evidence overrides optional raw flag")
			var bytes int64
			require.NoError(t, database.Table("messages").Where("NOT opaque AND message_bytes IS NOT NULL").Count(&bytes).Error)
			if conf.Flags.IndexTxMessageRaw {
				require.Positive(t, bytes)
			} else {
				require.Zero(t, bytes)
			}
			require.NoError(t, saveLaneWrite(t, database, input, conf, false))
			after, _ := saveLaneSnapshot(t, database)
			require.Equal(t, before, after)
			t.Logf("FLAGS mask=%d fingerprint=%s rows=%v", mask, before, counts)
		})
	}
}
