package core

import (
	"errors"
	"math"
	"reflect"
	"testing"

	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
)

func TestIndexedBlockWindowHistoricalGapsAndBounds(t *testing.T) {
	var queries [][2]int64
	w := indexedBlockWindow{load: func(start, end int64) ([]dbTypes.BlockIndexState, error) {
		queries = append(queries, [2]int64{start, end})
		if end-start >= dbTypes.BlockIndexWindowSize {
			t.Fatal("unbounded window")
		}
		var rows []dbTypes.BlockIndexState
		for h := start; h <= end; h++ {
			// Missing rows must be enqueued, not skipped by a max-height cursor.
			if h == 2 || h == 10001 {
				continue
			}
			rows = append(rows, dbTypes.BlockIndexState{Height: h, TxIndexed: true, BlockEventsIndexed: h != 3})
		}
		return rows, nil
	}}
	var work []EnqueueData
	for h := int64(1); h <= 20005; h++ {
		b, exists, err := w.lookup(h, 20005)
		if err != nil {
			t.Fatal(err)
		}
		if item := blockIndexWork(h, b, exists, true, true); item != nil {
			work = append(work, *item)
		}
		if len(w.rows) > int(dbTypes.BlockIndexWindowSize) {
			t.Fatal("unbounded retained rows")
		}
	}
	want := []EnqueueData{{2, true, true}, {3, true, false}, {10001, true, true}}
	if !reflect.DeepEqual(work, want) {
		t.Fatalf("work = %+v, want %+v", work, want)
	}
	if !reflect.DeepEqual(queries, [][2]int64{{1, 10000}, {10001, 20000}, {20001, 20005}}) {
		t.Fatalf("queries = %v", queries)
	}
	// A later tip extends the scan without retaining or replaying old windows.
	_, exists, err := w.lookup(20006, 20006)
	if err != nil || !exists || queries[3] != [2]int64{20006, 20006} {
		t.Fatalf("live follow: queries=%v exists=%v err=%v", queries, exists, err)
	}
}

func TestIndexedBlockWindowEmptyAndError(t *testing.T) {
	calls := 0
	w := indexedBlockWindow{load: func(start, end int64) ([]dbTypes.BlockIndexState, error) { calls++; return nil, nil }}
	for h := int64(1); h <= 10000; h++ {
		_, exists, err := w.lookup(h, 10000)
		if err != nil || exists {
			t.Fatalf("empty window: exists=%v err=%v", exists, err)
		}
	}
	if calls != 1 {
		t.Fatalf("empty window queried %d times", calls)
	}
	failure := errors.New("query failure")
	w.load = func(start, end int64) ([]dbTypes.BlockIndexState, error) { return nil, failure }
	if _, _, err := w.lookup(10001, 10001); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
}

func TestIndexedBlockWindowRangeEnds(t *testing.T) {
	for _, bounds := range [][2]int64{{7, 7}, {7, 99}, {math.MaxInt64 - 2, math.MaxInt64}, {math.MaxInt64 - 10000, math.MaxInt64}} {
		w := indexedBlockWindow{load: func(start, end int64) ([]dbTypes.BlockIndexState, error) {
			if start != bounds[0] || end > bounds[1] || end < start || end-start >= dbTypes.BlockIndexWindowSize {
				t.Fatalf("invalid query bounds [%d,%d] for %v", start, end, bounds)
			}
			return nil, nil
		}}
		if _, _, err := w.lookup(bounds[0], bounds[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBlockIndexWorkFlags(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, events := range []bool{false, true} {
			for _, txs := range []bool{false, true} {
				for _, doneEvents := range []bool{false, true} {
					for _, doneTxs := range []bool{false, true} {
						b := dbTypes.BlockIndexState{BlockEventsIndexed: doneEvents, TxIndexed: doneTxs}
						item := blockIndexWork(42, b, exists, events, txs)
						wantEvents, wantTxs := events && (!exists || !doneEvents), txs && (!exists || !doneTxs)
						if exists && !wantEvents && !wantTxs {
							if item != nil {
								t.Fatal("indexed row was enqueued")
							}
							continue
						}
						if item == nil || *item != (EnqueueData{42, wantEvents, wantTxs}) {
							t.Fatalf("unexpected work %+v", item)
						}
					}
				}
			}
		}
	}
}
