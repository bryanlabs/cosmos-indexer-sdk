package taxapi

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A deposit address whose customers arrive after its first 3000 memo
// receipts is exchange-like: the count covers every receipt before as_of,
// stops at the threshold, and ignores receipts after as_of.
func TestCommissionFlowMemoSendersCountAllReceiptsPostgres(t *testing.T) {
	s := flowPGServer(t)
	const grown = "cosmos1depositgrownlater"
	for _, sql := range []string{
		// 3000 early receipts from 10 senders, then 150 new senders, all before as_of.
		"INSERT INTO txes SELECT 5000+g, 1, 'E'||g, 0, 'acct'||(500000+g%10) FROM generate_series(1,3000) g",
		"INSERT INTO messages SELECT 5000+g, 5000+g FROM generate_series(1,3000) g",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp) SELECT 5000+g, 'transfer', '1', 'uatom', 'cosmos1first'||(g%10), '" + grown + "', '2025-01-02T00:00:00Z'::timestamptz + g * interval '1 minute' FROM generate_series(1,3000) g",
		"INSERT INTO txes SELECT 9000+g, 1, 'G'||g, 0, ' acct'||(900000+g)||E'\\t' FROM generate_series(1,150) g",
		"INSERT INTO messages SELECT 9000+g, 9000+g FROM generate_series(1,150) g",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp) SELECT 9000+g, 'ibc_in', '1', 'uatom', 'cosmos1later'||g, '" + grown + "', '2025-03-01T00:00:00Z' FROM generate_series(1,150) g",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	var first []string
	require.NoError(t, s.db.Raw(`SELECT DISTINCT from_addr FROM (SELECT from_addr FROM taxable_events WHERE to_addr = ? ORDER BY timestamp, id LIMIT 3000) f`, grown).Scan(&first).Error)
	require.Len(t, first, 10, "the first 3000 receipts hold only 10 senders")

	n, err := s.memoDepositSenders(grown, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowMemoMinSenders, n, "counted over every receipt, capped at the threshold")
	l := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
	stop, err := l.stop(grown, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowMemoHeuristic}, stop)

	// With as_of before the later senders arrived, only the first 10 count.
	n, err = s.memoDepositSenders(grown, flowPGWindowStart.AddDate(0, 2, 0))
	require.NoError(t, err)
	require.Equal(t, 10, n)
}

// flowMemoSQL accepts exactly the memos depositMemo accepts.
func TestFlowMemoSQLMatchesDepositMemoPostgres(t *testing.T) {
	s := flowPGServer(t)
	memos := []string{
		"", "abc", "abc1", "a1b2c3d4", "1234", "abcd", "12345678901234567890123456789012345678901", "1234567890123456789012345678901234567890",
		" acct7 ", "\tacct7\n", "\vacct7\f", "\racct7\r\n", "\u0085acct7\u00a0", "\u1680acct7\u2000", "\u200aacct7\u2028", "\u2029acct7\u202f",
		"\u205facct7\u3000", "\u200bacct7", "acct7\ufeff", "acct 7", "acct.7", "acct-7_x", "ACCT7", "\u00e4cct7", "acct\uff17",
		"   ", "\u3000\u3000\u30001234\u3000", "12\n34", "memo: 1234", "-_-1", "____", "\u00a0\u00a01\u00a0\u00a0", " 123 ",
	}
	for _, m := range memos {
		var ok bool
		require.NoError(t, s.db.Raw(`SELECT ?::text ~ ? AND ?::text ~ '[0-9]'`, m, flowMemoSQL, m).Scan(&ok).Error, "%q", m)
		require.Equal(t, depositMemo(m), ok, "%q", m)
	}
}
