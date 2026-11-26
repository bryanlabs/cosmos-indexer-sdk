package taxapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const heavyBlocksTime = "ALTER TABLE blocks ADD COLUMN time_stamp timestamptz"

const heavyMessageEvents = "CREATE TABLE message_events (id bigint, message_id bigint)"

const heavyMessageAttrs = "CREATE TABLE message_event_attributes (message_event_id bigint, value text)"

func flowHeavyFixture(t *testing.T) *Server {
	t.Helper()
	s := flowPGServer(t)
	for _, sql := range []string{
		heavyBlocksTime,
		"UPDATE blocks SET time_stamp = '2025-02-01T00:00:00Z'",
		"INSERT INTO blocks VALUES (60, 1, 6000, '2025-05-01T00:00:00Z')",
		heavyMessageEvents,
		heavyMessageAttrs,
		"CREATE TABLE block_events (id bigint, block_id bigint)",
		"CREATE TABLE block_event_attributes (block_event_id bigint, value text)",
		"CREATE TABLE tax_balance_deferred (chain text, address text, rows bigint, created_at timestamptz)",
		"INSERT INTO message_events VALUES (1, 1)",
		"INSERT INTO block_events VALUES (10, 1), (20, 60)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	return s
}

func flowHeavyRows(t *testing.T, s *Server, addr string, message, earlyBlock, lateBlock int) {
	t.Helper()
	require.NoError(t, s.db.Exec("INSERT INTO message_event_attributes SELECT 1, ? FROM generate_series(1, ?)", addr, message).Error)
	require.NoError(t, s.db.Exec("INSERT INTO block_event_attributes SELECT 10, ? FROM generate_series(1, ?)", addr, earlyBlock).Error)
	require.NoError(t, s.db.Exec("INSERT INTO block_event_attributes SELECT 20, ? FROM generate_series(1, ?)", addr, lateBlock).Error)
}

// The heavy book gives the counts a server without sharing reads, for
// wallets under the cutoff, over it for life only, and over it before end.
func TestFlowHeavyBookMatchesPlainCountsPostgres(t *testing.T) {
	s := flowHeavyFixture(t)
	const light, lateHeavy, heavy, listed = "cosmos1light", "cosmos1lateheavy", "cosmos1heavy", "cosmos1listed"
	flowHeavyRows(t, s, light, 1, 1, 0)
	flowHeavyRows(t, s, lateHeavy, 1, 0, 5)
	flowHeavyRows(t, s, heavy, 2, 2, 2)
	flowHeavyRows(t, s, listed, 1, 2, 3)
	require.NoError(t, s.db.Exec("INSERT INTO tax_balance_deferred VALUES ('mainnet', ?, 6, now())", listed).Error)

	ends := []time.Time{flowPGWindowEnd, flowPGWindowEnd.AddDate(0, 6, 0), flowPGWindowStart}
	for _, cacheBytes := range []int64{1, 1 << 20} {
		shared := &Server{db: s.db, native: s.native, flowShared: NewCommissionSharedHistory(cacheBytes)}
		for round := 0; round < 2; round++ {
			for _, addr := range []string{light, lateHeavy, heavy, listed} {
				for _, end := range ends {
					want, err := s.walletRowsBefore(addr, 3, end)
					require.NoError(t, err)
					got, err := shared.walletRowsBefore(addr, 3, end)
					require.NoError(t, err)
					require.Equal(t, want, got, "addr %s end %s cache %d round %d", addr, end, cacheBytes, round)
				}
			}
		}
	}
}

// A proved fact is not counted again: after the count entries are evicted,
// an over-cutoff wallet answers from the book, at its end and any later one,
// and a wallet tax_balance_deferred lists skips its lifetime count.
func TestFlowHeavyBookSkipsProvedCountsPostgres(t *testing.T) {
	s := flowHeavyFixture(t)
	const heavy, listed = "cosmos1heavy", "cosmos1listed"
	flowHeavyRows(t, s, heavy, 2, 2, 0)
	require.NoError(t, s.db.Exec("INSERT INTO tax_balance_deferred VALUES ('mainnet', ?, 9, now())", listed).Error)
	flowHeavyRows(t, s, listed, 1, 0, 0)

	shared := &Server{db: s.db, native: s.native, flowShared: NewCommissionSharedHistory(1)}
	n, err := shared.walletRowsBefore(heavy, 3, flowPGWindowEnd)
	require.NoError(t, err)
	require.EqualValues(t, 4, n)
	require.NoError(t, s.db.Exec("DELETE FROM block_event_attributes WHERE value = ?", heavy).Error)
	for _, end := range []time.Time{flowPGWindowEnd, flowPGWindowEnd.AddDate(1, 0, 0)} {
		n, err = shared.walletRowsBefore(heavy, 3, end)
		require.NoError(t, err)
		require.EqualValues(t, 4, n, "over before %s is answered without a count", end)
	}
	n, err = shared.walletRowsBefore(heavy, 3, flowPGWindowStart)
	require.NoError(t, err)
	require.EqualValues(t, 0, n, "an earlier end is counted, not answered from the book")

	n, err = shared.walletRowsBefore(listed, 3, flowPGWindowEnd)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "a listed wallet goes straight to the count before end")
	n, err = shared.walletRowsBefore(listed, 9, flowPGWindowEnd)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "a listing at or under the limit proves nothing")
}

// Without the table every wallet is counted, as before.
func TestFlowHeavyBookWithoutDeferredTablePostgres(t *testing.T) {
	s := flowHeavyFixture(t)
	require.NoError(t, s.db.Exec("DROP TABLE tax_balance_deferred").Error)
	flowHeavyRows(t, s, "cosmos1heavy", 2, 2, 1)
	shared := &Server{db: s.db, native: s.native, flowShared: NewCommissionSharedHistory(1 << 20)}
	n, err := shared.walletRowsBefore("cosmos1heavy", 3, flowPGWindowEnd)
	require.NoError(t, err)
	require.EqualValues(t, 4, n)
}
