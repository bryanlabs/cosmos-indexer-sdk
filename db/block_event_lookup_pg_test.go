package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/require"
)

func TestIndexBlockEventsReusesStoredTypesAndKeysPostgres(t *testing.T) {
	database := publicationDatabase(t)
	for _, statement := range []string{
		"CREATE TEMP TABLE failed_event_blocks (id bigserial PRIMARY KEY, height bigint, blockchain_id bigint)",
		"CREATE TEMP TABLE block_event_types (id bigserial PRIMARY KEY, type text UNIQUE)",
		"CREATE TEMP TABLE block_event_attribute_keys (id bigserial PRIMARY KEY, key text UNIQUE)",
		`CREATE TEMP TABLE block_events (id bigserial PRIMARY KEY, "index" bigint, lifecycle_position bigint,
 block_id bigint REFERENCES blocks, block_event_type_id bigint REFERENCES block_event_types, UNIQUE (block_id, lifecycle_position, "index"))`,
		`CREATE TEMP TABLE block_event_attributes (id bigserial PRIMARY KEY, block_event_id bigint REFERENCES block_events,
 value text, "index" bigint, block_event_attribute_key_id bigint REFERENCES block_event_attribute_keys, UNIQUE (block_event_id, "index"))`,
	} {
		require.NoError(t, database.Exec(statement).Error)
	}
	// Unique names keep the process-wide ID caches from answering for the table.
	suffix := fmt.Sprint(time.Now().UnixNano())
	storedType, newType := "transfer-"+suffix, "burn-"+suffix
	storedKey, newKey := "amount-"+suffix, "burner-"+suffix
	require.NoError(t, database.Exec("INSERT INTO block_event_types (type) VALUES (?)", storedType).Error)
	require.NoError(t, database.Exec("INSERT INTO block_event_attribute_keys (key) VALUES (?)", storedKey).Error)

	block := models.Block{Height: 100, ChainID: 1, TimeStamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), ProposerConsAddress: models.Address{Address: "proposer"}}
	event := func(index uint64, eventType string, keys ...string) BlockEventDBWrapper {
		wrapper := BlockEventDBWrapper{BlockEvent: models.BlockEvent{Index: index, LifecyclePosition: models.BeginBlockEvent, BlockEventType: models.BlockEventType{Type: eventType}}}
		for i, key := range keys {
			wrapper.Attributes = append(wrapper.Attributes, models.BlockEventAttribute{Index: uint64(i), Value: key + "-value", BlockEventAttributeKey: models.BlockEventAttributeKey{Key: key}})
		}
		return wrapper
	}
	wrapper := &BlockDBWrapper{
		Block:            &block,
		BeginBlockEvents: []BlockEventDBWrapper{event(0, storedType, storedKey), event(1, newType, storedKey, newKey)},
		UniqueBlockEventTypes: map[string]models.BlockEventType{
			storedType: {Type: storedType}, newType: {Type: newType},
		},
		UniqueBlockEventAttributeKeys: map[string]models.BlockEventAttributeKey{
			storedKey: {Key: storedKey}, newKey: {Key: newKey},
		},
	}
	_, err := IndexBlockEvents(database, false, wrapper, "test")
	require.NoError(t, err)

	require.Equal(t, int64(2), publicationCount(t, database, "block_event_types"))
	require.Equal(t, int64(2), publicationCount(t, database, "block_event_attribute_keys"))
	var mismatched int64
	require.NoError(t, database.Raw(`SELECT
 (SELECT count(*) FROM block_events e JOIN block_event_types t ON t.id = e.block_event_type_id
   WHERE t.type <> CASE e."index" WHEN 0 THEN ? ELSE ? END)
 + (SELECT count(*) FROM block_event_attributes a JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
   WHERE a.value <> k.key || '-value')`, storedType, newType).Scan(&mismatched).Error)
	require.Zero(t, mismatched)
	require.Equal(t, int64(3), publicationCount(t, database, "block_event_attributes"))
}
