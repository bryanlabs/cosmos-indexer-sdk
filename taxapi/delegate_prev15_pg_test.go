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

// Before Gaia v15 a MsgDelegate's delegate event and a MsgUndelegate's unbond
// event name only the validator and the amount. The wallet's ledger reads
// must still give it both, once, through the wallet-driven reads the API uses
// and the block-driven reads the repair uses alike. A delegation that names
// its delegator (Gaia v15 and later) is read once, not twice.
func TestPreV15DelegateAndUnbondReadPostgres(t *testing.T) {
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

	w, other := testAddress(t, 151), testAddress(t, 152)
	const val = "cosmosvaloper1prev15"
	const (
		hDelegate = 9500000 // pre-v15 MsgDelegate by w
		hUnbond   = 9500010 // pre-v15 MsgUndelegate by w
		hOther    = 9500020 // pre-v15 MsgDelegate by someone else
		hNamed    = 9500030 // a delegate event that names its delegator
	)
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
		fmt.Sprintf("INSERT INTO blocks VALUES (%[1]d,1,%[1]d,timestamptz '2022-01-10',true,true),(%[2]d,1,%[2]d,timestamptz '2022-01-10',true,true),(%[3]d,1,%[3]d,timestamptz '2022-01-10',true,true),(%[4]d,1,%[4]d,timestamptz '2022-01-10',true,true)",
			hDelegate, hUnbond, hOther, hNamed),
		fmt.Sprintf("INSERT INTO txes VALUES (1,%d,'D1',0),(2,%d,'U1',0),(3,%d,'O1',0),(4,%d,'N1',0)", hDelegate, hUnbond, hOther, hNamed),
		"INSERT INTO messages VALUES (1,1),(2,2),(3,3),(4,4)",
		"INSERT INTO message_event_types VALUES (1,'coin_spent'),(2,'coin_received'),(3,'message'),(4,'delegate'),(5,'unbond'),(6,'withdraw_rewards')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'spender'),(2,'receiver'),(3,'amount'),(4,'sender'),(5,'module'),(6,'validator'),(7,'new_shares'),(8,'completion_time'),(9,'delegator')",
		// w delegates 1000: it spends the coins, and the delegate event
		// names no delegator.
		"INSERT INTO message_events VALUES (10,1,1,0),(11,1,3,1),(12,1,4,2)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (10,1,'%[1]s',0),(10,3,'1000uatom',1),(11,5,'staking',0),(11,4,'%[1]s',1),(12,6,'%[2]s',0),(12,3,'1000uatom',1),(12,7,'1000.000000000000000000',2)", w, val),
		// w unbonds 400 and is paid its rewards. SDK 0.42 to 0.45 merge the
		// message events: the distribution module's sender for the payout
		// comes before the staking delegator.
		"INSERT INTO message_events VALUES (20,2,6,0),(21,2,3,1),(22,2,5,2)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (20,6,'%[2]s',0),(20,3,'7uatom',1),(21,4,'%[3]s',0),(21,5,'staking',1),(21,4,'%[1]s',2),(22,6,'%[2]s',0),(22,3,'400uatom',1),(22,8,'2022-01-31T00:00:00Z',2)", w, val, distributionAddress(t)),
		// Someone else's delegation to the same validator.
		"INSERT INTO message_events VALUES (30,3,1,0),(31,3,3,1),(32,3,4,2)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (30,1,'%[1]s',0),(30,3,'9uatom',1),(31,5,'staking',0),(31,4,'%[1]s',1),(32,6,'%[2]s',0),(32,3,'9uatom',1)", other, val),
		// A delegate event that names w.
		"INSERT INTO message_events VALUES (40,4,1,0),(41,4,3,1),(42,4,4,2)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (40,1,'%[1]s',0),(40,3,'5uatom',1),(41,5,'staking',0),(41,4,'%[1]s',1),(42,6,'%[2]s',0),(42,9,'%[1]s',1),(42,3,'5uatom',2)", w, val),
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	require.NoError(t, inclusionsMigrate(database))

	s := &Server{db: database, native: DefaultNativeAsset}
	blocked := &Server{db: database, native: DefaultNativeAsset, blockReads: true}
	ids := []int64{1}
	staking := func(events []ledgerEvent) []string {
		var out []string
		for _, e := range events {
			if e.Type != "delegate" && e.Type != "unbond" {
				continue
			}
			for _, rec := range e.records() {
				out = append(out, fmt.Sprintf("%d %s %t %s %s", e.Height, e.Type, rec["delegator"] == w, rec["validator"], rec["amount"]))
			}
		}
		return out
	}
	want := []string{
		fmt.Sprintf("%d delegate true %s 1000uatom", hDelegate, val),
		fmt.Sprintf("%d unbond true %s 400uatom", hUnbond, val),
		fmt.Sprintf("%d delegate true %s 5uatom", hNamed, val),
	}
	for _, win := range [][2]int64{{hDelegate - 1, hNamed}, {hDelegate - 1, hDelegate}, {hUnbond - 1, hUnbond}, {hOther - 1, hOther}, {hNamed - 1, hNamed}} {
		events, unattributed, err := s.ledgerEvents(ids, w, "", win[0], win[1])
		require.NoError(t, err)
		require.Zero(t, unattributed)
		got := staking(events)
		var expect []string
		for _, line := range want {
			var h int64
			_, _ = fmt.Sscanf(line, "%d", &h)
			if h > win[0] && h <= win[1] {
				expect = append(expect, line)
			}
		}
		require.ElementsMatch(t, expect, got, "window %v", win)
		bEvents, bUnattributed, err := blocked.ledgerEvents(ids, w, "", win[0], win[1])
		require.NoError(t, err)
		require.Equal(t, unattributed, bUnattributed)
		require.ElementsMatch(t, got, staking(bEvents), "block reads, window %v", win)
	}
	// The other wallet gets its own delegation and none of w's.
	events, _, err := s.ledgerEvents(ids, other, "", hDelegate-1, hNamed)
	require.NoError(t, err)
	require.Equal(t, []string{fmt.Sprintf("%d delegate false %s 9uatom", hOther, val)}, staking(events))
}
