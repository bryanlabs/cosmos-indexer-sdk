package taxapi

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	flowTestValidator = "cosmosvaloper1test"
	flowTestOther     = "cosmosvaloper1other"
	flowTestReceiver  = "cosmos1receiver"
	flowTestExchange  = "cosmos1exchange"
	flowTestChain     = "osmosis-1"
)

// fakeFlowLedger is a constructed ledger: each wallet's events and opening
// holdings, and the addresses that end a trace.
type fakeFlowLedger struct {
	openings map[string]flowHoldings
	rows     map[string][]flowEvent
	stops    map[string]flowStop
	wide     map[string]bool
	loaded   []string
	stopped  []string
}

func (l *fakeFlowLedger) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	h, ok := l.openings[addr]
	if !ok {
		return flowHoldings{Liquid: decimal.Zero}, true, nil
	}
	return h, true, nil
}

func (l *fakeFlowLedger) events(addr string, start, end time.Time) ([]flowEvent, error) {
	l.loaded = append(l.loaded, addr)
	var out []flowEvent
	for _, e := range l.rows[addr] {
		if !e.Key.Time.Before(start) && e.Key.Time.Before(end) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *fakeFlowLedger) stop(addr string, start, end time.Time) (flowStop, error) {
	l.stopped = append(l.stopped, addr)
	if s, ok := l.stops[addr]; ok {
		return s, nil
	}
	return l.distributor(addr, start, end)
}

func (l *fakeFlowLedger) distributor(addr string, _, _ time.Time) (flowStop, error) {
	if l.wide[addr] {
		return flowStop{Class: flowWideDistributor, Detail: flowFanoutHeuristic}, nil
	}
	return flowStop{}, nil
}

func (l *fakeFlowLedger) known(addr string) flowStop { return l.stops[addr] }

func (l *fakeFlowLedger) ibcDestination(e flowEvent) string { return flowTestChain }

var flowTestStart = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func flowAt(day, minute int) flowKey {
	return flowKey{Time: flowTestStart.AddDate(0, 0, day).Add(time.Duration(minute) * time.Minute), Height: int64(day*10000 + minute), Message: uint(minute + 1)}
}

func amt(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// send adds a transfer to both sides of the constructed ledger.
func (l *fakeFlowLedger) send(k flowKey, from, to string, v int64) {
	l.rows[from] = append(l.rows[from], flowEvent{Key: k, Category: apiTransfer, Dir: apiOut, Amount: amt(v), From: from, To: to})
	l.rows[to] = append(l.rows[to], flowEvent{Key: k, Category: apiTransfer, Dir: "in", Amount: amt(v), From: from, To: to})
}

func (l *fakeFlowLedger) commission(k flowKey, to string, v int64) {
	l.rows[to] = append(l.rows[to], flowEvent{Key: k, Category: apiCommission, Dir: "in", Amount: amt(v), To: to, Validator: flowTestValidator})
}

func newFakeFlowLedger() *fakeFlowLedger {
	return &fakeFlowLedger{openings: map[string]flowHoldings{}, rows: map[string][]flowEvent{}, wide: map[string]bool{}, stops: map[string]flowStop{
		flowTestExchange: {Class: flowExchange, Detail: "Test Exchange"},
	}}
}

func runFlowTest(t *testing.T, l *fakeFlowLedger, group ...string) *flowTrace {
	t.Helper()
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	require.NoError(t, tr.run(group))
	return tr
}

func fate(tr *flowTrace, rule int, month, class, detail string, via bool) string {
	return tr.Fates[FlowFateKey{Rule: rule, Month: month, Class: class, Detail: detail, ViaUnbonding: via}].String()
}

func TestFlowRulesSplitMixedOutflow(t *testing.T) {
	l := newFakeFlowLedger()
	l.openings[flowTestReceiver] = flowHoldings{Liquid: amt(50)}
	l.commission(flowAt(1, 0), flowTestReceiver, 100)
	l.send(flowAt(2, 0), flowTestReceiver, flowTestExchange, 120)
	tr := runFlowTest(t, l, flowTestReceiver)

	// First in first out: the 50 already held leave first.
	require.Equal(t, "70", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "30", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
	// Pro-rata: commission was 100 of 150, so it is two thirds of the 120.
	require.Equal(t, "80", fate(tr, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "20", fate(tr, flowRuleProRata, "2025-01", flowHeld, "", false))
	require.Equal(t, "100", tr.Commission["2025-01"].String())
	// The exchange is never loaded as a wallet.
	require.Equal(t, []string{flowTestReceiver}, l.loaded)
}

func TestFlowRestakeThenUnbondThenSell(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 100)
	l.rows[r] = append(l.rows[r],
		flowEvent{Key: flowAt(1, 5), Category: flowCatDelegate, Dir: apiOut, Amount: amt(60), From: r, Validator: flowTestValidator},
		flowEvent{Key: flowAt(1, 6), Category: flowCatDelegate, Dir: apiOut, Amount: amt(10), From: r, Validator: flowTestOther},
		flowEvent{Key: flowAt(5, 0), Category: flowCatUndelegate, Dir: "in", Amount: amt(20), To: r, Validator: flowTestValidator},
		flowEvent{Key: flowAt(26, 0), Category: flowCatUnbondingComplete, Dir: "in", Amount: amt(20), To: r, Validator: flowTestValidator},
		flowEvent{Key: flowAt(27, 0), Category: apiFee, Dir: apiOut, Amount: amt(1), From: r},
	)
	l.send(flowAt(40, 0), r, flowTestExchange, 49)
	tr := runFlowTest(t, l, r)

	for rule := 0; rule < flowRules; rule++ {
		require.Equal(t, "40", fate(tr, rule, "2025-01", flowRestakedOwn, "", false), flowRuleNames[rule])
		require.Equal(t, "10", fate(tr, rule, "2025-01", flowRestakedOther, flowTestOther, false), flowRuleNames[rule])
	}
	require.Equal(t, "1", fate(tr, flowRuleFIFO, "2025-01", flowFees, "", false))
	// Pro-rata splits the fee across the coins that came back from unbonding.
	require.Equal(t, "0.6", fate(tr, flowRuleProRata, "2025-01", flowFees, "", false))
	require.Equal(t, "0.4", fate(tr, flowRuleProRata, "2025-01", flowFees, "", true))
	// FIFO: the 30 never staked leave before the 20 that came back from
	// unbonding (the fee took one of the 30).
	require.Equal(t, "29", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "20", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", true))
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
}

func TestFlowFollowsHopsAndReplaysDiscoveredWallet(t *testing.T) {
	l := newFakeFlowLedger()
	r, w1, w2, w3, w4 := flowTestReceiver, "cosmos1hop1", "cosmos1hop2", "cosmos1hop3", "cosmos1hop4"
	l.commission(flowAt(1, 0), r, 100)
	// w1 already held 10 and got 5 from a stranger earlier on the day the
	// commission reached it; both are other coins it spends first.
	l.openings[w1] = flowHoldings{Liquid: amt(10)}
	l.send(flowAt(2, 1), "cosmos1stranger", w1, 5)
	l.send(flowAt(2, 9), r, w1, 100)
	l.send(flowAt(3, 0), w1, w2, 115)
	l.send(flowAt(4, 0), w2, w3, 115)
	l.send(flowAt(5, 0), w3, w4, 40)
	tr := runFlowTest(t, l, r)

	require.Equal(t, []string{r, w1, w2, w3}, l.loaded)
	// The 15 other coins reach w3 ahead of the commission and leave first.
	require.Equal(t, "25", fate(tr, flowRuleFIFO, "2025-01", flowBeyondHops, "", false))
	require.Equal(t, "75", fate(tr, flowRuleFIFO, "2025-01", flowHeldDownstream, "hop 3", false))
	require.True(t, tr.Wallets[w3].simulated().Equal(amt(75)))
	require.Equal(t, 1, tr.Wallets[w1].Hop)
}

// A wallet a hop out that pays out to a crowd ends the trace there, the way
// a busy wallet does, even when it is a validator operator account.
func TestFlowWideDistributorStopsReceiver(t *testing.T) {
	l := newFakeFlowLedger()
	r, hub := flowTestReceiver, "cosmos1hub"
	l.wide[hub] = true
	l.commission(flowAt(1, 0), r, 100)
	l.send(flowAt(2, 0), r, hub, 60)
	l.send(flowAt(3, 0), hub, "cosmos1delegatorone", 30)
	l.send(flowAt(3, 1), hub, "cosmos1delegatortwo", 30)
	tr := runFlowTest(t, l, r)

	for rule := 0; rule < flowRules; rule++ {
		require.Equal(t, "60", fate(tr, rule, "2025-01", flowWideDistributor, flowFanoutHeuristic, false), flowRuleNames[rule])
		require.Equal(t, "40", fate(tr, rule, "2025-01", flowHeld, "", false), flowRuleNames[rule])
	}
	require.Equal(t, []string{r}, l.loaded, "the distributor is never loaded as a wallet")
	require.Equal(t, 50000, flowFanoutMaxReceivers)
	require.Contains(t, FlowClassNotes[flowWideDistributor], "more than 50,000", "the page states the threshold the code uses")
	require.Contains(t, FlowMethod, "more than 50,000")
}

// A receiving wallet (hop 0) that pays out to a crowd stays traced, so what
// it keeps or restakes is still reported, but its payouts end as distributed
// instead of opening a wallet per recipient. A labelled receiver keeps its
// fate, and no receiver is looked up one by one.
func TestFlowWideDistributorAtHopZero(t *testing.T) {
	l := newFakeFlowLedger()
	op := flowTestReceiver
	l.wide[op] = true
	l.openings[op] = flowHoldings{Liquid: amt(100)}
	l.commission(flowAt(1, 0), op, 100)
	l.rows[op] = append(l.rows[op], flowEvent{Key: flowAt(1, 5), Category: flowCatDelegate, Dir: apiOut, Amount: amt(20), From: op, Validator: flowTestValidator})
	for i := 0; i < 50; i++ {
		l.send(flowAt(2, i), op, fmt.Sprintf("cosmos1delegator%02d", i), 2)
	}
	l.send(flowAt(3, 0), op, flowTestExchange, 40)
	tr := runFlowTest(t, l, op)

	// FIFO: the 20 restaked and the first 80 out are the 100 held before.
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowRestakedOwn, "", false))
	require.Equal(t, "20", fate(tr, flowRuleFIFO, "2025-01", flowWideDistributor, flowFanoutHeuristic, false))
	require.Equal(t, "40", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "40", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
	// Pro-rata: half of every outflow and of what is left is commission.
	require.Equal(t, "10", fate(tr, flowRuleProRata, "2025-01", flowRestakedOwn, "", false))
	require.Equal(t, "50", fate(tr, flowRuleProRata, "2025-01", flowWideDistributor, flowFanoutHeuristic, false))
	require.Equal(t, "20", fate(tr, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "20", fate(tr, flowRuleProRata, "2025-01", flowHeld, "", false))
	require.Equal(t, []string{op}, l.loaded)
	require.Empty(t, l.stopped, "receivers of a distributor are not looked up one by one")
}

// An ordinary receiving wallet still opens hop 1 for each receiver.
func TestFlowNarrowReceiverStillFollowed(t *testing.T) {
	l := newFakeFlowLedger()
	r, w1 := flowTestReceiver, "cosmos1hop1"
	l.commission(flowAt(1, 0), r, 100)
	l.send(flowAt(2, 0), r, w1, 100)
	tr := runFlowTest(t, l, r)

	require.Equal(t, []string{r, w1}, l.loaded)
	require.Equal(t, []string{w1}, l.stopped)
	require.Equal(t, "100", fate(tr, flowRuleFIFO, "2025-01", flowHeldDownstream, "hop 1", false))
}

func TestFlowTransfersBetweenReceiversCountOnce(t *testing.T) {
	l := newFakeFlowLedger()
	a, b := flowTestReceiver, "cosmos1second"
	l.commission(flowAt(1, 0), a, 100)
	l.commission(flowAt(1, 1), b, 50)
	l.send(flowAt(2, 0), a, b, 100)
	l.send(flowAt(3, 0), b, flowTestExchange, 30)
	tr := runFlowTest(t, l, a, b)

	require.Equal(t, "150", tr.Commission["2025-01"].String())
	require.Equal(t, "30", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "120", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
	require.True(t, tr.Wallets[a].simulated().IsZero())
	require.True(t, tr.Wallets[b].simulated().Equal(amt(120)))
}

func TestFlowOverdrawAndIBC(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 100)
	l.rows[r] = append(l.rows[r], flowEvent{Key: flowAt(2, 0), Category: flowCatIBCOut, Dir: apiOut, Amount: amt(130), From: r, To: "osmo1x"})
	tr := runFlowTest(t, l, r)

	for rule := 0; rule < flowRules; rule++ {
		require.Equal(t, "100", fate(tr, rule, "2025-01", flowIBCOut, flowTestChain, false))
		require.Equal(t, "30", tr.Wallets[r].Overdraw[rule].String())
	}
}

func TestFlowCommissionOfOtherValidatorIsOther(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.rows[r] = append(l.rows[r], flowEvent{Key: flowAt(1, 0), Category: apiCommission, Dir: "in", Amount: amt(70), To: r, Validator: flowTestOther})
	l.commission(flowAt(1, 1), r, 30)
	l.send(flowAt(2, 0), r, flowTestExchange, 70)
	tr := runFlowTest(t, l, r)
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "21", fate(tr, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	require.Len(t, tr.Commission, 1)
}

func TestFlowPoolProRataKeepsTotals(t *testing.T) {
	p := &flowPool{rule: flowRuleProRata}
	p.add([]flowPart{{Origin: "a", Amount: amt(1)}, {Origin: "b", Amount: amt(2)}})
	out, short := p.take(amt(1))
	require.True(t, short.IsZero())
	sum := decimal.Zero
	for _, x := range out {
		sum = sum.Add(x.Amount)
	}
	require.True(t, sum.Equal(amt(1)), sum.String())
	require.True(t, p.total().Equal(amt(2)), p.total().String())
}
