package taxapi

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A hot wallet whose first 3000 receipts come from wallets that also spend
// elsewhere, and whose deposit addresses arrive later, is exchange-like: the
// sweep count covers every sender and every row of theirs in the window.
// Rows past any per-sender cap decide two senders: one leaks a final
// transfer elsewhere, one was funded by 300 small receipts.
func TestCommissionFlowSweepCountsAllSendersPostgres(t *testing.T) {
	s := flowPGServer(t)
	const hot = "cosmos1hotgrownlater"
	for _, sql := range []string{
		// 250 spenders send the first 3000 receipts, 12 each, and also pay a shop.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '10', 'uatom', 'cosmos1spender'||(g%250), '" + hot + "', '2025-01-02T00:00:00Z'::timestamptz + g * interval '1 minute' FROM generate_series(1,3000) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '10', 'uatom', 'cosmos1spender'||g, 'cosmos1shop', '2025-01-10T00:00:00Z' FROM generate_series(0,249) g",
		// 300 deposit addresses arrive later and forward all they got.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1customer'||g, 'cosmos1deposit'||g, '2025-02-15T00:00:00Z' FROM generate_series(1,300) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1deposit'||g, '" + hot + "', '2025-03-01T00:00:00Z' FROM generate_series(1,300) g",
		// The leaker forwards in 250 parts, then once to the shop.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '5000', 'uatom', 'cosmos1customerleak', 'cosmos1leaker', '2025-02-15T00:00:00Z')",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '20', 'uatom', 'cosmos1leaker', '" + hot + "', '2025-03-02T00:00:00Z'::timestamptz + g * interval '1 minute' FROM generate_series(1,250) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '1', 'uatom', 'cosmos1leaker', 'cosmos1shop', '2025-03-03T00:00:00Z')",
		// The big deposit was funded by 300 receipts of 10 and forwards all 3000.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '10', 'uatom', 'cosmos1customerbig', 'cosmos1bigdeposit', '2025-02-15T00:00:00Z'::timestamptz + g * interval '1 minute' FROM generate_series(1,300) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '3000', 'uatom', 'cosmos1bigdeposit', '" + hot + "', '2025-03-01T00:00:00Z')",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	var first []string
	require.NoError(t, s.db.Raw(`SELECT DISTINCT from_addr FROM (SELECT from_addr FROM taxable_events WHERE to_addr = ? ORDER BY timestamp, id LIMIT 3000) f`, hot).Scan(&first).Error)
	require.Len(t, first, 250, "the first 3000 receipts come only from the spenders")

	examined, deposits, err := s.sweepDepositSenders(hot, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, 552, examined)
	require.Equal(t, 301, deposits, "300 deposits and the big one; the leaker paid the shop")
	l := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
	stop, err := l.busy(hot, ledgerHeavyRows+1, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowSweepHeuristic}, stop)

	// Before the deposit addresses arrived only the spenders count.
	examined, deposits, err = s.sweepDepositSenders(hot, flowPGWindowStart, time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 250, examined)
	require.Zero(t, deposits)

	// Shared across traces in one process: the second read is a hit with the same answer.
	s.flowShared = NewCommissionSharedHistory(1 << 20)
	for i := 0; i < 2; i++ {
		examined, deposits, err = s.sweepDepositSenders(hot, flowPGWindowStart, flowPGWindowEnd)
		require.NoError(t, err)
		require.Equal(t, [2]int{552, 301}, [2]int{examined, deposits})
	}
	hits, misses, _, _ := s.flowShared.Stats()
	require.Equal(t, [2]int64{1, 1}, [2]int64{hits, misses})
}
