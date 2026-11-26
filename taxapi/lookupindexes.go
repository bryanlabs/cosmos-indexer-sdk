package taxapi

import "gorm.io/gorm"

// Covering indexes for the id lookups the commission fee and payout reads make
// on message_events, messages, txes and blocks. Each read finds rows by id and
// needs only the join columns, so these indexes answer it with an index-only
// scan instead of a heap fetch per row.
var lookupIndexes = []string{
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_blocks_id_cover ON blocks (id) INCLUDE (height, time_stamp, chain_id)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_txes_id_block_id ON txes (id) INCLUDE (block_id)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_id_tx_id ON messages (id) INCLUDE (tx_id)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_message_events_id_cover ON message_events (id) INCLUDE (message_id, message_event_type_id)`,
}

// LookupIndexesMigrate creates the covering lookup indexes. A no-op once
// built; on a large database the message_events build takes most of an hour,
// so build them by hand before deploying there.
func LookupIndexesMigrate(db *gorm.DB) error {
	for _, sql := range lookupIndexes {
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return nil
}
