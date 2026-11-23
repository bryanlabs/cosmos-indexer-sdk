package taxapi

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// An exec that names no delegator is returned by the wallet's ledger read as
// an unmatched record, next to the wallet's own delegation in the same
// block, and is settled from the wallet's delegations either side of the
// block with that delegation taken off first.
func TestSettleUnnamedStakeFromLedgerReadPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres ledger verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)

	w := testAddress(t, 171)
	const v1 = "cosmosvaloper1settle"
	const h = 9700000
	for _, sql := range []string{
		tempSearchPath,
		tempChainsTable,
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, time_stamp timestamptz, tx_indexed boolean, block_events_indexed boolean)",
		tempTxesTable,
		tempMsgsTable,
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index bigint)",
		"CREATE TEMP TABLE message_event_types (id bigint, type text)",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, message_event_attribute_key_id bigint, value text, index bigint)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE block_events (id bigint, block_id bigint, block_event_type_id bigint, lifecycle_position bigint)",
		tempBlockEventTypes,
		"CREATE TEMP TABLE block_event_attributes (block_event_id bigint, block_event_attribute_key_id bigint, value text, index bigint)",
		tempBlockEventKeys,
		tempHubChainRow,
		fmt.Sprintf("INSERT INTO blocks VALUES (%[1]d,1,%[1]d,timestamptz '2022-01-10',true,true)", h),
		fmt.Sprintf("INSERT INTO txes VALUES (1,%[1]d,'X1',0),(2,%[1]d,'D1',0)", h),
		"INSERT INTO messages VALUES (1,1),(2,2)",
		"INSERT INTO message_event_types VALUES (1,'coin_spent'),(2,'coin_received'),(3,'message'),(4,'delegate'),(5,'unbond'),(6,'withdraw_rewards'),(7,'transfer')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'spender'),(2,'receiver'),(3,'amount'),(4,'sender'),(5,'module'),(6,'validator'),(7,'new_shares'),(8,'completion_time'),(9,'delegator'),(10,'recipient'),(11,'action')",
		// An exec by w that names no delegator.
		"INSERT INTO message_events VALUES (10,1,3,0),(11,1,4,1)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (10,11,'/cosmos.authz.v1beta1.MsgExec',0),(10,4,'%[1]s',1),(11,6,'%[2]s',0),(11,3,'50uatom',1)", w, v1),
		// w's own delegation of 30 in the same block.
		"INSERT INTO message_events VALUES (20,2,1,0),(21,2,3,1),(22,2,4,2)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (20,1,'%[1]s',0),(20,3,'30uatom',1),(21,5,'staking',0),(21,4,'%[1]s',1),(22,6,'%[2]s',0),(22,3,'30uatom',1),(22,7,'30.000000000000000000',2)", w, v1),
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	require.NoError(t, inclusionsMigrate(database))

	ids := []int64{1}
	for _, s := range []*Server{{db: database, native: DefaultNativeAsset}, {db: database, native: DefaultNativeAsset, blockReads: true}} {
		_, unattributed, err := s.ledgerEvents(ids, w, "", h-1, h)
		require.NoError(t, err)
		require.Equal(t, int64(h), unattributed, "block reads %t", s.blockReads)
		events, unmatched, err := s.ledgerStakeEvents(ids, w, "", h-1, h)
		require.NoError(t, err)
		require.Len(t, unmatched, 1, "block reads %t", s.blockReads)
		require.Equal(t, map[string]string{"validator": v1, "amount": "50uatom", "delegator": w}, unmatched[0].records()[0])

		settle := func(after int64) ([]ledgerEvent, int64) {
			var read []int64
			return settleUnnamedStakes(delegationsBy(map[int64]map[string]int64{h - 1: {v1: 100}, h: {v1: after}}, &read), w, "", "uatom", events, unmatched)
		}
		// Clean: 80 more, the 30 named and the 50 unnamed.
		owned, unattributed := settle(180)
		require.Zero(t, unattributed)
		require.Equal(t, unmatched, owned, "block reads %t", s.blockReads)
		// Zero: the named 30 explains it all.
		owned, unattributed = settle(130)
		require.Zero(t, unattributed)
		require.Empty(t, owned)
		// Unclean: 60 more is neither.
		_, unattributed = settle(160)
		require.Equal(t, int64(h), unattributed)
		// No archive.
		_, unattributed = settleUnnamedStakes(nil, w, "", "uatom", events, unmatched)
		require.Equal(t, int64(h), unattributed)
	}
}
