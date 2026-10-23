package db

import (
	"fmt"

	"gorm.io/gorm"
)

// BlockIndexWindowSize bounds both the queried height range and returned rows.
const BlockIndexWindowSize int64 = 10000

// BlockIndexState contains only the fields the enqueue skip decision needs.
type BlockIndexState struct {
	Height             int64
	TxIndexed          bool
	BlockEventsIndexed bool
}

func blockIndexWindowQuery(db *gorm.DB, chainID uint, start, end int64) *gorm.DB {
	return db.Table("blocks").
		Select("height, tx_indexed, block_events_indexed").
		Where("chain_id = ?::int AND time_stamp != '0001-01-01T00:00:00.000Z' AND height >= ? AND height <= ?", chainID, start, end).
		Order("height asc").Limit(int(BlockIndexWindowSize))
}

// GetBlockIndexWindow preserves the historical timestamp filter without loading
// full block models or retaining an entire chain's indexing history.
func GetBlockIndexWindow(db *gorm.DB, chainID uint, start, end int64) ([]BlockIndexState, error) {
	if start < 1 || end < start || end-start >= BlockIndexWindowSize {
		return nil, fmt.Errorf("invalid block indexing window [%d, %d]", start, end)
	}
	var blocks []BlockIndexState
	err := blockIndexWindowQuery(db, chainID, start, end).Find(&blocks).Error
	return blocks, err
}
