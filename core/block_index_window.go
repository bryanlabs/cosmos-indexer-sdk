package core

import (
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
)

// indexedBlockWindow is consumed in ascending height order. Replacing rows on
// rollover releases the previous window, including entries skipped as indexed.
type indexedBlockWindow struct {
	rows []dbTypes.BlockIndexState
	next int
	end  int64
	load func(start, end int64) ([]dbTypes.BlockIndexState, error)
}

func (w *indexedBlockWindow) lookup(height, last int64) (dbTypes.BlockIndexState, bool, error) {
	if height > w.end {
		end := last
		if last-height >= dbTypes.BlockIndexWindowSize {
			end = height + dbTypes.BlockIndexWindowSize - 1
		}
		rows, err := w.load(height, end)
		if err != nil {
			return dbTypes.BlockIndexState{}, false, err
		}
		w.rows, w.next, w.end = rows, 0, end
	}
	for w.next < len(w.rows) && w.rows[w.next].Height < height {
		w.next++
	}
	if w.next < len(w.rows) && w.rows[w.next].Height == height {
		block := w.rows[w.next]
		w.next++
		return block, true, nil
	}
	return dbTypes.BlockIndexState{}, false, nil
}

func blockIndexWork(height int64, block dbTypes.BlockIndexState, exists, events, transactions bool) *EnqueueData {
	if exists {
		events = events && !block.BlockEventsIndexed
		transactions = transactions && !block.TxIndexed
		if !events && !transactions {
			return nil
		}
	}
	return &EnqueueData{Height: height, IndexBlockEvents: events, IndexTransactions: transactions}
}
