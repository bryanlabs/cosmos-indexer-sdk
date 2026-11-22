package taxapi

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlowProgressPreservesFinancialTrace(t *testing.T) {
	makeLedger := func() *fakeFlowLedger {
		l := newFakeFlowLedger()
		l.openings[flowTestReceiver] = flowHoldings{Liquid: amt(50)}
		l.commission(flowAt(1, 0), flowTestReceiver, 100)
		l.send(flowAt(2, 0), flowTestReceiver, "cosmos1downstream", 120)
		l.send(flowAt(3, 0), "cosmos1downstream", flowTestExchange, 90)
		return l
	}
	baseline := runFlowTest(t, makeLedger(), flowTestReceiver)
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), makeLedger())
	var updates []commissionFlowProgress
	tr.progress = func(p commissionFlowProgress) { updates = append(updates, p) }
	require.NoError(t, tr.run([]string{flowTestReceiver}))
	require.Equal(t, baseline.Fates, tr.Fates)
	require.Equal(t, baseline.Commission, tr.Commission)
	require.Equal(t, baseline.Wallets, tr.Wallets)
	require.Equal(t, baseline.Unresolved, tr.Unresolved)
	require.NotEmpty(t, updates)
	for i, p := range updates {
		require.Equal(t, i+1, p.Sequence)
		require.GreaterOrEqual(t, p.ElapsedMilliseconds, int64(0))
		if !p.DiscoveryComplete {
			require.Equal(t, -1, p.ClosingRemaining, "undiscovered work is unknown, never a false zero")
		}
		b, err := json.Marshal(p)
		require.NoError(t, err)
		require.NotContains(t, string(b), flowTestReceiver)
		require.NotContains(t, string(b), "cosmos1downstream")
		require.NotContains(t, string(b), "wallet_sha256")
		require.NotContains(t, string(b), privateSHA([]byte(flowTestReceiver)))
		require.NotContains(t, string(b), privateSHA([]byte("cosmos1downstream")))
	}
	last := updates[len(updates)-1]
	require.Equal(t, "discovery_complete", last.Phase)
	require.True(t, last.DiscoveryComplete)
	require.Equal(t, len(tr.order), last.ClosingRemaining)
	require.Equal(t, 0, last.PendingEvents)
	require.Equal(t, len(tr.order), last.LoadedWallets)
}

type failedProgressLedger struct{ *fakeFlowLedger }

func (l failedProgressLedger) events(string, time.Time, time.Time) ([]flowEvent, error) {
	return nil, errors.New("source failure")
}

func TestFlowProgressDoesNotCallPartialReadComplete(t *testing.T) {
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), failedProgressLedger{newFakeFlowLedger()})
	var updates []commissionFlowProgress
	tr.progress = func(p commissionFlowProgress) { updates = append(updates, p) }
	require.Error(t, tr.run([]string{flowTestReceiver}))
	require.NotEmpty(t, updates)
	last := updates[len(updates)-1]
	require.Equal(t, "events_started", last.Phase)
	require.Equal(t, 1, last.AttemptedWallets)
	require.Equal(t, 0, last.LoadedWallets)
	require.False(t, last.DiscoveryComplete)
	require.Equal(t, -1, last.ClosingRemaining)
	b, err := json.Marshal(last)
	require.NoError(t, err)
	require.NotContains(t, string(b), privateSHA([]byte(flowTestReceiver)))
}

func TestFlowProgressPrivateProjectionCloneRetainsCallback(t *testing.T) {
	s := NewServer(nil, nil, DefaultNativeAsset)
	called := 0
	base := &dbFlowLoader{s: s, private: true, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}, progress: func(commissionFlowProgress) { called++ }}
	_, clone := s.privateProjectionTrace(base)
	require.NotNil(t, clone.progress)
	clone.progress(commissionFlowProgress{})
	require.Equal(t, 1, called)
}

func TestFlowProgressQueuedEventCounts(t *testing.T) {
	l := newFakeFlowLedger()
	for i := 0; i < 1001; i++ {
		l.commission(flowAt(1, i), flowTestReceiver, 1)
	}
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	var applied []commissionFlowProgress
	tr.progress = func(p commissionFlowProgress) {
		if p.Phase == "events_applied" {
			applied = append(applied, p)
		}
	}
	require.NoError(t, tr.run([]string{flowTestReceiver}))
	require.Len(t, applied, 1)
	require.Equal(t, 1000, applied[0].AppliedQueuedEvents)
	require.Equal(t, 1, applied[0].PendingEvents)
	require.False(t, applied[0].DiscoveryComplete)
	span := tr.AsOf.Sub(tr.From)
	require.InDelta(t, float64(flowAt(1, 999).Time.Sub(tr.From))/float64(span), applied[0].EventTimeFraction, 1e-9)
}

func TestFlowProgressEventTimeFraction(t *testing.T) {
	l := newFakeFlowLedger()
	l.commission(flowAt(1, 0), flowTestReceiver, 100)
	l.send(flowAt(30, 0), flowTestReceiver, "cosmos1downstream", 60)
	l.send(flowAt(60, 0), "cosmos1downstream", flowTestExchange, 50)
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	var updates []commissionFlowProgress
	tr.progress = func(p commissionFlowProgress) { updates = append(updates, p) }
	require.NoError(t, tr.run([]string{flowTestReceiver}))
	require.Equal(t, -1.0, updates[0].EventTimeFraction, "no event applied yet")
	prev := -1.0
	for _, p := range updates {
		require.GreaterOrEqual(t, p.EventTimeFraction, prev)
		require.Less(t, p.EventTimeFraction, 1.0)
		prev = p.EventTimeFraction
	}
	span := tr.AsOf.Sub(tr.From)
	require.InDelta(t, float64(flowAt(60, 0).Time.Sub(tr.From))/float64(span), updates[len(updates)-1].EventTimeFraction, 1e-9)
}
