package taxapi

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The stop reads see only the index before as_of: receipts and attribute
// rows from after the trace's as-of date change neither the memo-deposit count, the
// busy cutoff, nor the stop.
func TestCommissionFlowStopReadsIgnoreAfterAsOfPostgres(t *testing.T) {
	s := flowPGServer(t)
	const near, heavy, quiet = "cosmos1nearexchange", "cosmos1heavyinwindow", "cosmos1heavylater"
	for _, sql := range []string{
		"ALTER TABLE blocks ADD COLUMN time_stamp timestamptz",
		"UPDATE blocks SET time_stamp = '2025-02-01T00:00:00Z'",
		"INSERT INTO blocks VALUES (50, 1, 5000, '2025-02-15T00:00:00Z'), (60, 1, 6000, '2025-05-01T00:00:00Z')",
		"CREATE TABLE message_events (id bigint, message_id bigint)",
		"CREATE TABLE message_event_attributes (message_event_id bigint, value text)",
		"CREATE TABLE block_events (id bigint, block_id bigint)",
		"CREATE TABLE block_event_attributes (block_event_id bigint, value text)",
		// 99 deposit-style senders in the window: one short of exchange-like.
		"INSERT INTO txes SELECT 2000+g, 50, 'N'||g, 0, 'acct'||(200000+g) FROM generate_series(1,99) g",
		"INSERT INTO messages SELECT 2000+g, 2000+g FROM generate_series(1,99) g",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp) SELECT 2000+g, 'transfer', '1', 'uatom', 'cosmos1early'||g, '" + near + "', '2025-02-15T00:00:00Z' FROM generate_series(1,99) g",
		// Attribute rows: heavy names 3 in the window, quiet names 1.
		"INSERT INTO message_events VALUES (1, 2001), (2, 2002)",
		"INSERT INTO message_event_attributes VALUES (1, '" + heavy + "'), (2, '" + heavy + "'), (1, '" + quiet + "')",
		"INSERT INTO block_events VALUES (10, 50), (20, 60)",
		"INSERT INTO block_event_attributes VALUES (10, '" + heavy + "')",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	type reads struct {
		memo         int
		heavy, quiet int64
		stop         flowStop
	}
	read := func() reads {
		var r reads
		var err error
		r.memo, err = s.memoDepositSenders(near, flowPGWindowEnd)
		require.NoError(t, err)
		r.heavy, err = s.walletRowsBefore(heavy, 2, flowPGWindowEnd)
		require.NoError(t, err)
		r.quiet, err = s.walletRowsBefore(quiet, 2, flowPGWindowEnd)
		require.NoError(t, err)
		l := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
		r.stop, err = l.stop(near, flowPGWindowStart, flowPGWindowEnd)
		require.NoError(t, err)
		return r
	}
	before := read()
	require.Equal(t, reads{memo: 99, heavy: 3, quiet: 1}, before)

	// After the as-of date: 5 more deposit-style senders, and attribute rows
	// that put quiet's lifetime count over the cutoff.
	for _, sql := range []string{
		"INSERT INTO txes SELECT 3000+g, 60, 'L'||g, 0, 'acct'||(300000+g) FROM generate_series(1,5) g",
		"INSERT INTO messages SELECT 3000+g, 3000+g FROM generate_series(1,5) g",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp) SELECT 3000+g, 'transfer', '1', 'uatom', 'cosmos1late'||g, '" + near + "', '2025-05-01T00:00:00Z' FROM generate_series(1,5) g",
		"INSERT INTO message_events VALUES (3, 3001)",
		"INSERT INTO message_event_attributes SELECT 3, '" + quiet + "' FROM generate_series(1,4)",
		"INSERT INTO block_event_attributes SELECT 20, '" + heavy + "' FROM generate_series(1,4)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	after := read()
	require.Equal(t, before, after, "activity after the as-of date changes no stop read")
	lifetime, err := s.walletRows(quiet, 2)
	require.NoError(t, err)
	require.EqualValues(t, 3, lifetime, "quiet is over the cutoff only by its later rows")
}
