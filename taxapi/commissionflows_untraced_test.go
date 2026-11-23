package taxapi

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func hopChainLedger() (*fakeFlowLedger, []string) {
	l := newFakeFlowLedger()
	w := []string{flowTestReceiver, "cosmos1hop1", "cosmos1hop2", "cosmos1hop3", "cosmos1hop4", "cosmos1hop5", "cosmos1hop6"}
	l.commission(flowAt(1, 0), w[0], 100)
	for i := 0; i+1 < len(w); i++ {
		l.send(flowAt(2+i, 0), w[i], w[i+1], 100)
	}
	return l, w
}

// The default limit stops the coins leaving hop 3 and names where they went.
func TestFlowStopRecordsBeyondHopsAddress(t *testing.T) {
	l, w := hopChainLedger()
	tr := runFlowTest(t, l, w[0])
	require.Equal(t, "100", fate(tr, flowRuleFIFO, "2025-01", flowBeyondHops, "", false))
	k := FlowStopKey{Rule: flowRuleFIFO, Class: flowBeyondHops, From: w[3], To: w[4], Hop: 3}
	require.Equal(t, "100", tr.Stops[k].String())
	ev := commissionFlowStopEvidence(tr.Stops)
	require.Len(t, ev, 2)
	require.Equal(t, CommissionFlowStopEvidence{Rule: "fifo", Class: flowBeyondHops, From: w[3], To: w[4], Hop: 3, AmountBase: "100"}, ev[0])
}

// A higher limit follows the same coins further.
func TestFlowHopLimitParameter(t *testing.T) {
	l, w := hopChainLedger()
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	tr.maxHops = 5
	require.NoError(t, tr.run([]string{w[0]}))
	require.Equal(t, "100", fate(tr, flowRuleFIFO, "2025-01", flowBeyondHops, "", false))
	require.Equal(t, 5, tr.Wallets[w[5]].Hop)
	require.Equal(t, "100", tr.Stops[FlowStopKey{Rule: flowRuleFIFO, Class: flowBeyondHops, From: w[5], To: w[6], Hop: 5}].String())

	l, w = hopChainLedger()
	tr = newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	tr.maxHops = 8
	require.NoError(t, tr.run([]string{w[0]}))
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowBeyondHops, "", false))
	require.Equal(t, "100", fate(tr, flowRuleFIFO, "2025-01", flowHeldDownstream, "hop 6", false))
	require.Empty(t, tr.Stops)
}

// A labelled stop names its address too.
func TestFlowStopRecordsLabelledStop(t *testing.T) {
	l := newFakeFlowLedger()
	l.commission(flowAt(1, 0), flowTestReceiver, 100)
	l.send(flowAt(2, 0), flowTestReceiver, flowTestExchange, 60)
	tr := runFlowTest(t, l, flowTestReceiver)
	k := FlowStopKey{Rule: flowRuleProRata, Class: flowExchange, Detail: "Test Exchange", From: flowTestReceiver, To: flowTestExchange, Hop: 0}
	require.Equal(t, "60", tr.Stops[k].String())
}

func TestFlowHopVersionNaming(t *testing.T) {
	base := CommissionFlowBuildOptions{Version: "ops847-sample", Build: "b", AsOf: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
	require.NoError(t, base.validate())
	o := base
	o.MaxHops = 5
	require.Error(t, o.validate())
	o.Version = "ops847-sample-hops5"
	require.NoError(t, o.validate())
	o.Version = "ops847-sample-hops50"
	require.Error(t, o.validate())
	o.Version = "ops847-hops5-sample"
	require.NoError(t, o.validate())
	o.MaxHops = flowMaxHopsCeiling + 1
	o.Version = "x-hops21"
	require.Error(t, o.validate())
	o = base
	o.Version = "ops847-sample-hops3"
	require.Error(t, o.validate())
}

func TestFlowSweepExchangeRule(t *testing.T) {
	require.True(t, sweepExchange(200, 100))
	require.True(t, sweepExchange(30, 20))
	require.False(t, sweepExchange(200, 99))
	require.False(t, sweepExchange(19, 19))
	require.False(t, sweepExchange(200, 24))
}

// A busy wallet fed by deposit addresses that pass everything through is
// exchange-like; one whose senders also spend elsewhere stays busy_wallet
// with the measurement in the evidence. Private runs store no label.
func TestCommissionFlowBusyWalletSweepPostgres(t *testing.T) {
	s := flowPGServer(t)
	const hot, service = "cosmos1hotwallet", "cosmos1service"
	for _, sql := range []string{
		"CREATE TABLE message_event_attributes (value text)",
		"CREATE TABLE block_event_attributes (value text)",
		// 30 deposit addresses: a customer funds each, it forwards all of it.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1customer'||g, 'cosmos1deposit'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,30) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1deposit'||g, '" + hot + "', '2025-02-02T00:00:00Z' FROM generate_series(1,30) g",
		// 10 senders that keep part of what they got, 5 that also delegate.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1funder', 'cosmos1keeper'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,10) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1000', 'uatom', 'cosmos1keeper'||g, '" + hot + "', '2025-02-02T00:00:00Z' FROM generate_series(1,10) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1funder', 'cosmos1user'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,5) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '2500', 'uatom', 'cosmos1user'||g, '" + hot + "', '2025-02-02T00:00:00Z' FROM generate_series(1,5) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp, validator_address) SELECT 'delegate', '2500', 'uatom', 'cosmos1user'||g, '', '2025-02-03T00:00:00Z', 'cosmosvaloper1x' FROM generate_series(1,5) g",
		// The service: 30 users who send it part and spend the rest elsewhere.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1funder', 'cosmos1patron'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,30) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '2000', 'uatom', 'cosmos1patron'||g, '" + service + "', '2025-02-02T00:00:00Z' FROM generate_series(1,30) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '3000', 'uatom', 'cosmos1patron'||g, 'cosmos1shop', '2025-02-03T00:00:00Z' FROM generate_series(1,30) g",
		// Outside the window: does not count.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '5000', 'uatom', 'cosmos1late'||g, '" + service + "', '2025-06-01T00:00:00Z' FROM generate_series(1,40) g",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	examined, deposits, err := s.sweepDepositSenders(hot, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, 45, examined)
	require.Equal(t, 30, deposits, "keepers forwarded a fifth, users also delegated")
	examined, deposits, err = s.sweepDepositSenders(service, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, 30, examined)
	require.Zero(t, deposits)

	l := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
	stop, err := l.busy(hot, ledgerHeavyRows+1, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowSweepHeuristic}, stop)
	stop, err = l.stop(hot, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowSweepHeuristic}, stop, "the inferred label answers later stops")
	stop, err = l.busy(service, ledgerHeavyRows+1, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowBusyWallet}, stop)
	labels := l.evidence().Labels
	require.Len(t, labels, 2)
	byAddr := map[string]AddressLabel{labels[0].Address: labels[0], labels[1].Address: labels[1]}
	require.Equal(t, labelExchangeLike, byAddr[hot].Kind)
	require.Contains(t, byAddr[hot].Evidence, "30 of 45 senders from 2025-01-01 to 2025-04-01 pass through to it")
	require.Equal(t, labelBusyWallet, byAddr[service].Kind)
	require.Contains(t, byAddr[service].Evidence, "0 of 30 senders")
	var stored int64
	require.NoError(t, s.db.Model(&AddressLabel{}).Where("source = ?", flowSweepHeuristic).Count(&stored).Error)
	require.Zero(t, stored, "a private run stores no label")
}
