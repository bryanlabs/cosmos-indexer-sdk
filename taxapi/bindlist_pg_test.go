package taxapi

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

// bindListFixture holds more messages and blocks than one chunk. Block ids
// run against height, and two messages share each block, so the order of a
// read differs from the order of its ids.
func bindListFixture(t *testing.T) (*gorm.DB, int, int) {
	t.Helper()
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := inclusionsMigrate(db); err != nil {
		t.Fatal(err)
	}
	messages := 2*bindListChunk + 7
	blocks := (messages + 1) / 2
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'cosmos1proposer')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		fmt.Sprintf(`INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed,block_events_indexed)
SELECT g, 1, 1, 2000000 - g, timestamptz '2026-03-01T00:00:00Z' - g * interval '6 seconds', true, true FROM generate_series(1, %d) g`, blocks),
		"INSERT INTO block_event_types(id,type) VALUES(1,'complete_unbonding'),(2,'coin_received')",
		"INSERT INTO block_event_attribute_keys(id,key) VALUES(1,'delegator'),(2,'amount'),(3,'validator')",
		// Per block: an end-block unbonding, a begin-block receipt and a
		// second end-block event that every third block leaves without
		// attributes.
		fmt.Sprintf(`INSERT INTO block_events(id,block_id,lifecycle_position,index,block_event_type_id)
SELECT 3*g-2, g, 1, 0, 1 FROM generate_series(1, %[1]d) g
UNION ALL SELECT 3*g-1, g, 0, 0, 2 FROM generate_series(1, %[1]d) g
UNION ALL SELECT 3*g, g, 1, 1, 2 FROM generate_series(1, %[1]d) g`, blocks),
		fmt.Sprintf(`INSERT INTO block_event_attributes(block_event_id,index,block_event_attribute_key_id,value)
SELECT e, 0, 1, 'cosmos1delegator' FROM generate_series(1, %[1]d) e WHERE NOT (e %% 3 = 0 AND (e / 3) %% 3 = 0)
UNION ALL SELECT e, 1, 2, e || 'uatom' FROM generate_series(1, %[1]d) e WHERE NOT (e %% 3 = 0 AND (e / 3) %% 3 = 0)`, 3*blocks),
		fmt.Sprintf(`INSERT INTO txes(id,block_id,hash,code,memo)
SELECT g, (g + 1) / 2, 'BINDTX' || g, 0, '' FROM generate_series(1, %d) g`, messages),
		"INSERT INTO message_types(id,message_type) VALUES(1,'/cosmos.staking.v1beta1.MsgBeginRedelegate')",
		fmt.Sprintf(`INSERT INTO messages(id,tx_id,message_type_id,message_index)
SELECT g, g, 1, 0 FROM generate_series(1, %d) g`, messages),
		"INSERT INTO message_event_types(id,type) VALUES(1,'redelegate'),(2,'withdraw_rewards'),(3,'message'),(4,'transfer')",
		"INSERT INTO message_event_attribute_keys(id,key) VALUES(1,'source_validator'),(2,'amount'),(3,'delegator')",
		fmt.Sprintf(`INSERT INTO message_events(id,message_id,index,message_event_type_id)
SELECT 4*(g-1) + i, g, i - 1, i FROM generate_series(1, %d) g, generate_series(1, 4) i`, messages),
		fmt.Sprintf(`INSERT INTO message_event_attributes(message_event_id,index,message_event_attribute_key_id,value)
SELECT e, i - 1, i, 'v' || e || '/' || i FROM generate_series(1, %d) e, generate_series(1, 3) i`, 4*messages),
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, messages, blocks
}

// shuffledIDs is 1..n in a fixed random order with a few ids repeated.
func shuffledIDs[T uint | uint64](n int) []T {
	ids := make([]T, 0, n+3)
	for i := 1; i <= n; i++ {
		ids = append(ids, T(i))
	}
	ids = append(ids, 1, T(n), T(bindListChunk+1))
	r := rand.New(rand.NewSource(20261121))
	r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	return ids
}

// Each chunked read returns exactly what one read of every id returns (the
// fixture stays under the 65535 bind limit so the single read can run).
func TestChunkedWalletReadsMatchOneRead(t *testing.T) {
	db, messages, blocks := bindListFixture(t)
	s := &Server{db: db}

	t.Run("block lifecycle attributes", func(t *testing.T) {
		ids := shuffledIDs[uint](blocks)
		got, err := s.blockLifecycleAttrs(ids)
		if err != nil {
			t.Fatal(err)
		}
		var want []blockEventAttrRow
		if err := db.Table("block_events be").
			Select("be.id AS event_id, be.block_id, be.lifecycle_position, b.height, b.time_stamp, t.type, k.key, a.value").
			Joins("JOIN blocks b ON b.id = be.block_id").
			Joins("JOIN block_event_types t ON t.id = be.block_event_type_id").
			Joins("LEFT JOIN block_event_attributes a ON a.block_event_id = be.id").
			Joins("LEFT JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id").
			Where("be.block_id IN ?", ids).
			Order("be.block_id, be.lifecycle_position, be.index, a.index").
			Scan(&want).Error; err != nil {
			t.Fatal(err)
		}
		if len(want) < bindListChunk*5 || !reflect.DeepEqual(got, want) {
			t.Fatalf("chunked read %d rows, single read %d rows, equal %v", len(got), len(want), reflect.DeepEqual(got, want))
		}
	})

	t.Run("redelegation rows", func(t *testing.T) {
		ids := shuffledIDs[uint64](messages)
		unique := map[uint64]bool{}
		var distinct []uint64
		for _, id := range ids {
			if !unique[id] {
				unique[id] = true
				distinct = append(distinct, id)
			}
		}
		rel, rp := s.effectiveRel()
		got, err := s.redelegationRows(distinct, rel, rp)
		if err != nil {
			t.Fatal(err)
		}
		var want []ledgerAttrRow
		if err := db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.message_id IN @msgs AND t.type IN ('redelegate', 'withdraw_rewards', 'message')
ORDER BY b.height, me.message_id, me.index, a.index`, map[string]any{"msgs": distinct}).Scan(&want).Error; err != nil {
			t.Fatal(err)
		}
		if len(want) != messages*9 || !reflect.DeepEqual(got, want) {
			t.Fatalf("chunked read %d rows, single read %d rows, equal %v", len(got), len(want), reflect.DeepEqual(got, want))
		}
	})

	t.Run("incomplete refund event logs", func(t *testing.T) {
		ids := shuffledIDs[uint](messages)
		got, err := s.incompleteRefundEventLogs(ids)
		if err != nil {
			t.Fatal(err)
		}
		var attrs []struct {
			MessageID uint
			EventID   uint
			Type      string
			Key       string
			Value     string
		}
		if err := db.Table("message_events me").
			Select("me.message_id, me.id AS event_id, et.type, k.key, a.value").
			Joins("JOIN message_event_types et ON et.id=me.message_event_type_id").
			Joins("JOIN LATERAL (SELECT message_event_attribute_key_id, value, index FROM message_event_attributes WHERE message_event_id=me.id OFFSET 0) a ON true").
			Joins("JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").
			Where("me.message_id IN ?", ids).Order("me.message_id, me.index, a.index").Scan(&attrs).Error; err != nil {
			t.Fatal(err)
		}
		want := map[uint]*txlog.LogMessage{}
		var lastEvent uint
		for _, attr := range attrs {
			if want[attr.MessageID] == nil {
				want[attr.MessageID] = &txlog.LogMessage{}
			}
			log := want[attr.MessageID]
			if lastEvent != attr.EventID || len(log.Events) == 0 {
				log.Events = append(log.Events, txlog.LogMessageEvent{Type: attr.Type})
			}
			lastEvent = attr.EventID
			i := len(log.Events) - 1
			log.Events[i].Attributes = append(log.Events[i].Attributes, txlog.Attribute{Key: attr.Key, Value: attr.Value})
		}
		if len(want) != messages || !reflect.DeepEqual(got, want) {
			t.Fatalf("chunked read %d messages, single read %d messages, equal %v", len(got), len(want), reflect.DeepEqual(got, want))
		}
	})
}
