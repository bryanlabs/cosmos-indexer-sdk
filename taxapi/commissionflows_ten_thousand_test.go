package taxapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlowHeaderCacheTenThousandWalletReceiptsNativeCallDelta(t *testing.T) {
	var nativeCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nativeCalls.Add(1)
		_, _ = w.Write([]byte(cachedHeaderBody))
	}))
	defer server.Close()
	baseline, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	cached, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	const wallets = 10000
	for i := 0; i < wallets; i++ {
		addr := fmt.Sprintf("synthetic-wallet-%d", i)
		// Cold-cache-per-call is the pre-sharing path; no other source exists.
		baseline.headers = flowHeaderCache{}
		before, err := baseline.header(addr, 123)
		require.NoError(t, err)
		after, err := cached.header(addr, 123)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	require.EqualValues(t, wallets, baseline.callAttempts.Load())
	require.EqualValues(t, 1, cached.callAttempts.Load())
	require.EqualValues(t, wallets-1, cached.headerReuses.Load())
	require.EqualValues(t, wallets+1, nativeCalls.Load())
	before, after := baseline.evidence(nil), cached.evidence(nil)
	require.Len(t, before, wallets)
	require.Len(t, after, wallets)
	for i := range before {
		require.Equal(t, before[i].Address, after[i].Address)
		require.Equal(t, before[i].Height, after[i].Height)
		require.Equal(t, before[i].Response, after[i].Response)
		require.Equal(t, before[i].SHA256, after[i].SHA256)
	}
	t.Logf("synthetic sole-source header proof: wallets=%d baseline_native_calls=%d cached_native_calls=%d delta=%d exact_per_wallet_receipts=%d", wallets, baseline.callAttempts.Load(), cached.callAttempts.Load(), baseline.callAttempts.Load()-cached.callAttempts.Load(), len(after))
}

func TestFlowProgressTenThousandSourceEventsPreserveFatesAndCounters(t *testing.T) {
	makeLedger := func() *fakeFlowLedger {
		l := newFakeFlowLedger()
		for i := 0; i < 10001; i++ {
			l.commission(flowAt(1, i), flowTestReceiver, 1)
		}
		return l
	}
	baseline := runFlowTest(t, makeLedger(), flowTestReceiver)
	l := makeLedger()
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	var applied []commissionFlowProgress
	tr.progress = func(p commissionFlowProgress) {
		if p.Phase == "events_applied" {
			applied = append(applied, p)
		}
	}
	require.NoError(t, tr.run([]string{flowTestReceiver}))
	require.Equal(t, baseline.Fates, tr.Fates)
	require.Equal(t, baseline.Commission, tr.Commission)
	require.Equal(t, baseline.Wallets, tr.Wallets)
	require.Equal(t, baseline.Unresolved, tr.Unresolved)
	require.Equal(t, []string{flowTestReceiver}, l.loaded)
	require.Equal(t, 10001, tr.appliedQueuedEvents)
	require.Len(t, applied, 10)
	for i, p := range applied {
		require.Equal(t, (i+1)*1000, p.AppliedQueuedEvents)
		require.Equal(t, 10001-p.AppliedQueuedEvents, p.PendingEvents)
		require.False(t, p.DiscoveryComplete)
		require.Equal(t, -1, p.ClosingRemaining)
		require.Zero(t, p.ClosingReadCompletedWallets)
	}
}
