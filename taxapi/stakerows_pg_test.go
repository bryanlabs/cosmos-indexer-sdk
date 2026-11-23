package taxapi

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// twiceDated adds to an early fixture an exec m dated at 600000 and 610000
// whose records the single read attributes differently as one group or two:
// its first delegation, to vB, comes before the reward withdrawal from vB
// that names another delegator, so it matches only a withdrawal left from
// the dating before. With between, a message dated at 605000 sits between
// the two datings, and the single read takes them apart.
func twiceDated(t *testing.T, database *gorm.DB, w string, between bool) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		q := fmt.Sprintf(sql, args...)
		require.NoError(t, database.Exec(q).Error, q)
	}
	for _, h := range []int64{500_000, 600_000, 605_000, 610_000} {
		exec("INSERT INTO blocks SELECT %[1]d,1,%[1]d,timestamptz '2022-01-10' + %[1]d * interval '6 seconds',true,true WHERE NOT EXISTS (SELECT 1 FROM blocks WHERE id = %[1]d)", h)
	}
	x := testAddress(t, 174)
	exec("INSERT INTO txes VALUES (9000001,500000,'M1',0)")
	exec("INSERT INTO messages VALUES (9000001,9000001)")
	exec("INSERT INTO message_events VALUES (90000010,9000001,4,0),(90000011,9000001,6,1),(90000012,9000001,6,2)")
	exec("INSERT INTO message_event_attributes VALUES (90000010,6,'cosmosvaloper1vb',0),(90000010,3,'5uatom',1),(90000011,9,'%[1]s',0),(90000011,6,'cosmosvaloper1vb',1),(90000012,9,'%[2]s',0),(90000012,6,'cosmosvaloper1va',1)", x, w)
	for i, p := range []int64{600_000, 610_000} {
		exec("INSERT INTO tax_tx_inclusions (chain, height, tx_index, hash, tx_id, block_id, code, effective, recorded_at) VALUES ('cosmoshub-4',%d,%d,'M1',9000001,%d,0,true,now())", p, i, p)
	}
	if between {
		exec("INSERT INTO txes VALUES (9000002,605000,'N1',0)")
		exec("INSERT INTO messages VALUES (9000002,9000002)")
		exec("INSERT INTO message_events VALUES (90000020,9000002,4,0)")
		exec("INSERT INTO message_event_attributes VALUES (90000020,6,'cosmosvaloper1vc',0),(90000020,3,'3uatom',1)")
	}
}

func fixtureMessages(t *testing.T, database *gorm.DB) []uint64 {
	t.Helper()
	var msgs []uint64
	require.NoError(t, database.Raw("SELECT id FROM messages ORDER BY id").Scan(&msgs).Error)
	return msgs
}

// attributeStakeRows gives, chunk by chunk, exactly what attributeStakes
// gives on the single read of every message, in the same order: messages
// dated once, a message dated twice with and without another message
// between its datings, and every chunk edge.
func TestAttributeStakeRowsMatchesTheSingleReadPostgres(t *testing.T) {
	blocks := []earlyBlock{
		{h: 960_000, unnamed: 50, named: 30, matched: 12, clean: true},
		{h: 10, unnamed: 20, matched: 25, placeMatched: true, clean: true, placed: []int64{940_000, 600_000}},
		{h: 900_000, unnamed: 30, foreign: 11, clean: true},
		{h: 700_000, unnamed: 15, matched: 25, foreign: 4, foreignFirst: true, clean: true},
		{h: 400_000, unnamed: 9, clean: false},
	}
	for _, between := range []bool{false, true} {
		database, w, _ := earlyFixture(t, blocks)
		twiceDated(t, database, w, between)
		msgs := fixtureMessages(t, database)
		rel, rp := (&Server{db: database}).effectiveRel()
		rows, err := (&Server{db: database}).redelegationRows(msgs, rel, rp)
		require.NoError(t, err)
		want, wantLeft := attributeStakes(rows, w)
		// The single read takes m's two datings as one group without a
		// message between them, and as two with one.
		left := 0
		for _, ev := range wantLeft {
			if ev.Msg == 9_000_001 {
				left++
			}
		}
		if between {
			require.Equal(t, 2, left, "each dating of m keeps its delegation to vB unmatched")
		} else {
			require.Equal(t, 1, left, "the second dating of m matches the withdrawal left by the first")
		}
		wantJSON, err := json.Marshal([][]ledgerEvent{want, wantLeft})
		require.NoError(t, err)
		for _, size := range []int{1, 2, 3, 4, 5, 0} {
			s := &Server{db: database, native: DefaultNativeAsset, stakeChunk: size}
			got, gotLeft, err := s.attributeStakeRows(msgs, rel, rp, w)
			require.NoError(t, err)
			require.Equal(t, want, got, "between %t chunk %d", between, size)
			require.Equal(t, wantLeft, gotLeft, "between %t chunk %d", between, size)
			gotJSON, err := json.Marshal([][]ledgerEvent{got, gotLeft})
			require.NoError(t, err)
			require.Equal(t, string(wantJSON), string(gotJSON), "between %t chunk %d", between, size)
		}
		// The messages in a different order, as the candidate read may give
		// them: chunks then hold other sets.
		rev := append([]uint64(nil), msgs...)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		for _, size := range []int{1, 2, 3} {
			s := &Server{db: database, native: DefaultNativeAsset, stakeChunk: size}
			got, gotLeft, err := s.attributeStakeRows(rev, rel, rp, w)
			require.NoError(t, err)
			require.Equal(t, want, got, "reversed, between %t chunk %d", between, size)
			require.Equal(t, wantLeft, gotLeft, "reversed, between %t chunk %d", between, size)
		}
	}
}

// ledgerStakeEvents, and the walk's stop from it, are the same at every
// chunk size.
func TestLedgerStakeEventsChunkedPostgres(t *testing.T) {
	database, w, states := earlyFixture(t, []earlyBlock{
		{h: 950_000, unnamed: 50, named: 30, clean: true},
		{h: 900_000, unnamed: 40, matched: 20, clean: true},
		{h: 10, unnamed: 20, clean: true, placed: []int64{940_000, 600_000}, dirty: 600_000},
		{h: 650_000, unnamed: 10, clean: false},
	})
	twiceDated(t, database, w, true)
	ref := &Server{db: database, native: DefaultNativeAsset, stakeChunk: 1 << 30}
	for _, lowest := range []int64{1, 599_999, 600_000, 605_000, 609_999, 610_000, 899_999} {
		want, wantLeft, err := ref.ledgerStakeEvents([]int64{1}, w, "", lowest, earlyTip)
		require.NoError(t, err)
		var wr []int64
		wantStop := walkStop(t, ref, w, lowest, delegationsBy(states, &wr))
		for _, size := range []int{1, 2, 3} {
			s := &Server{db: database, native: DefaultNativeAsset, stakeChunk: size}
			got, gotLeft, err := s.ledgerStakeEvents([]int64{1}, w, "", lowest, earlyTip)
			require.NoError(t, err)
			require.Equal(t, want, got, "lowest %d chunk %d", lowest, size)
			require.Equal(t, wantLeft, gotLeft, "lowest %d chunk %d", lowest, size)
			var gr []int64
			require.Equal(t, wantStop, walkStop(t, s, w, lowest, delegationsBy(states, &gr)), "lowest %d chunk %d", lowest, size)
		}
	}
}
