package taxapi

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestCommissionSharedHistoryReadsEachKeyOnce(t *testing.T) {
	c := NewCommissionSharedHistory(1 << 20)
	s := &Server{flowShared: c}
	var reads atomic.Int64
	read := func() (int64, error) {
		reads.Add(1)
		time.Sleep(50 * time.Millisecond)
		return 7, nil
	}
	k := flowSharedKey{kind: flowSharedWalletRows, addr: "cosmos1shared", n: 10}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.sharedCount(k, read)
			require.NoError(t, err)
			require.Equal(t, int64(7), n)
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), reads.Load(), "traces asking for one key at once share one read")
	k.n = 11
	_, err := s.sharedCount(k, read)
	require.NoError(t, err)
	require.Equal(t, int64(2), reads.Load(), "another limit is another read")
	hits, misses, evicted, _ := c.Stats()
	require.Equal(t, [3]int64{7, 2, 0}, [3]int64{hits, misses, evicted})
}

func TestCommissionSharedHistoryKeepsNoFailedRead(t *testing.T) {
	s := &Server{flowShared: NewCommissionSharedHistory(1 << 20)}
	k := flowSharedKey{kind: flowSharedMemo, addr: "cosmos1flaky"}
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int64
	failing := func() (int64, error) {
		reads.Add(1)
		close(started)
		<-release
		return 0, errors.New("canceling statement due to conflict with recovery")
	}
	done := make(chan error)
	go func() {
		_, err := s.sharedCount(k, failing)
		done <- err
	}()
	<-started
	waiter := make(chan int64)
	go func() {
		n, err := s.sharedCount(k, func() (int64, error) { reads.Add(1); return 3, nil })
		require.NoError(t, err)
		waiter <- n
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	require.Error(t, <-done, "the trace whose read failed gets the error, as it would without sharing")
	require.Equal(t, int64(3), <-waiter, "a trace waiting on a failed read reads again itself")
	require.Equal(t, int64(2), reads.Load())
	n, err := s.sharedCount(k, func() (int64, error) { t.Fatal("a finished read is kept"); return 0, nil })
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
}

func TestCommissionSharedHistoryDropsLeastRecentlyUsed(t *testing.T) {
	c := NewCommissionSharedHistory(400)
	s := &Server{flowShared: c}
	reads := map[string]int{}
	get := func(addr string) {
		_, err := s.sharedCount(flowSharedKey{kind: flowSharedMemo, addr: addr}, func() (int64, error) { reads[addr]++; return 1, nil })
		require.NoError(t, err)
	}
	get("a")
	get("b")
	get("a")
	get("c")
	_, _, evicted, bytes := c.Stats()
	require.Equal(t, int64(1), evicted)
	require.LessOrEqual(t, bytes, int64(400))
	get("a")
	get("b")
	require.Equal(t, map[string]int{"a": 1, "b": 2, "c": 1}, reads, "the least recently used key is dropped and read again when asked")
}

func TestCommissionSharedHistoryHoldingsAreCopies(t *testing.T) {
	h := flowHoldings{Staked: map[string]decimal.Decimal{"v": decimal.NewFromInt(5)}, Unbonding: map[string]decimal.Decimal{}}
	c := h.clone()
	c.Staked["v"] = decimal.NewFromInt(6)
	c.Unbonding["w"] = decimal.NewFromInt(1)
	require.Equal(t, "5", h.Staked["v"].String(), "a trace never changes the holdings another trace reads")
	require.Empty(t, h.Unbonding)
}

func TestCommissionSharedHistoryCountsPostgres(t *testing.T) {
	s := flowPGServer(t)
	const hub = "cosmos1sharedhub"
	require.NoError(t, s.db.Exec("CREATE TABLE message_event_attributes (value text)").Error)
	require.NoError(t, s.db.Exec("CREATE TABLE block_event_attributes (value text)").Error)
	require.NoError(t, s.db.Exec("INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', ?, 'cosmos1payee'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,30) g", hub).Error)
	require.NoError(t, s.db.Exec("INSERT INTO message_event_attributes SELECT ? FROM generate_series(1,4)", hub).Error)

	shared := &Server{db: s.db, native: s.native, flowShared: NewCommissionSharedHistory(1 << 20)}
	n, err := shared.fanoutReceivers(hub, flowPGWindowStart, flowPGWindowEnd, 100)
	require.NoError(t, err)
	require.Equal(t, 30, n)
	rows, err := shared.walletRows(hub, 10)
	require.NoError(t, err)
	require.Equal(t, int64(4), rows)
	plain, err := s.walletRows(hub, 10)
	require.NoError(t, err)
	require.Equal(t, plain, rows, "a shared count is the count an unshared trace reads")

	require.NoError(t, s.db.Exec("INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '1', 'uatom', ?, 'cosmos1newpayee', '2025-02-02T00:00:00Z')", hub).Error)
	require.NoError(t, s.db.Exec("INSERT INTO message_event_attributes VALUES (?)", hub).Error)
	n, err = shared.fanoutReceivers(hub, flowPGWindowStart, flowPGWindowEnd, 100)
	require.NoError(t, err)
	require.Equal(t, 30, n, "the same key is not read again")
	rows, err = shared.walletRows(hub, 10)
	require.NoError(t, err)
	require.Equal(t, int64(4), rows)
	n, err = shared.fanoutReceivers(hub, flowPGWindowStart, flowPGWindowEnd, 10)
	require.NoError(t, err)
	require.Equal(t, 11, n, "another limit is its own read")
	n, err = shared.fanoutReceivers(hub, flowPGWindowStart.AddDate(0, 1, 0), flowPGWindowEnd, 100)
	require.NoError(t, err)
	require.Equal(t, 31, n, "another window is its own read")
	plain, err = s.walletRows(hub, 10)
	require.NoError(t, err)
	require.Equal(t, int64(5), plain, "a server without sharing reads the index")
}
