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

// Reward rows name the delegator even when the chain paid its withdraw
// address. The trace must take reward coins from what was paid.
func TestFlowRewardsFollowTheWithdrawAddress(t *testing.T) {
	const withdraw = "cosmos1withdraw"
	k := flowAt(1, 5)
	booked := []flowEvent{
		{Key: k, Category: apiReward, Dir: "in", Amount: amt(50), To: flowTestReceiver, Validator: flowTestOther},
	}
	// The delegator was paid nothing: its reward row is dropped.
	require.Empty(t, reconcileRewardPayouts(flowTestReceiver, booked, nil))
	// Its withdraw address has no reward row but was paid 50.
	got := reconcileRewardPayouts(withdraw, nil, map[uint]flowRewardPayout{k.Message: {Key: k, Amount: amt(50)}})
	require.Len(t, got, 1)
	require.Equal(t, apiReward, got[0].Category)
	require.Equal(t, "50", got[0].Amount.String())
	require.Equal(t, withdraw, got[0].To)
	// Commission rows already name the receiver; only the rest of the
	// payout is reward, so a 50 reward row paid 120 with 100 commission keeps 20.
	mixed := []flowEvent{
		{Key: k, Category: apiCommission, Dir: "in", Amount: amt(100), To: flowTestReceiver, Validator: flowTestValidator},
		{Key: k, Category: apiReward, Dir: "in", Amount: amt(50), To: flowTestReceiver, Validator: flowTestOther},
	}
	got = reconcileRewardPayouts(flowTestReceiver, mixed, map[uint]flowRewardPayout{k.Message: {Key: k, Amount: amt(120)}})
	require.Len(t, got, 2)
	require.Equal(t, "100", got[0].Amount.String())
	require.Equal(t, "20", got[1].Amount.String())
	// A reward paid where it was booked is unchanged.
	got = reconcileRewardPayouts(flowTestReceiver, booked, map[uint]flowRewardPayout{k.Message: {Key: k, Amount: amt(50)}})
	require.Equal(t, booked, got)

	// In a trace, the phantom reward dilutes the pro-rata share of
	// commission that left: 100 of 150 instead of all of it.
	run := func(rows []flowEvent) *flowTrace {
		l := newFakeFlowLedger()
		l.commission(flowAt(1, 0), flowTestReceiver, 100)
		l.rows[flowTestReceiver] = append(l.rows[flowTestReceiver], rows...)
		l.send(flowAt(2, 0), flowTestReceiver, flowTestExchange, 100)
		return runFlowTest(t, l, flowTestReceiver)
	}
	before := run(booked)
	require.Equal(t, "66.666666666667", fate(before, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	after := run(reconcileRewardPayouts(flowTestReceiver, booked, nil))
	require.Equal(t, "100", fate(after, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "100", fate(after, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
}

// Older SDK logs merge a message's transfers into one event with several
// recipient, sender, amount triples (an undelegate paying a 2 uatom reward
// and moving 6198375 uatom of principal). Only the distribution triple to
// the wallet is a payout.
func TestFlowRewardPayoutsPairMergedTransfers(t *testing.T) {
	const (
		wallet = "cosmos1wallet"
		pool   = "cosmos1notbonded"
	)
	dist := moduleAddress("distribution")
	at := time.Date(2021, 10, 1, 0, 0, 0, 0, time.UTC)
	ev := func(id uint, idx uint64, key, value string) flowTransferAttr {
		return flowTransferAttr{EventID: id, MessageID: 7, Height: 8000002, TimeStamp: at, Index: idx, Key: key, Value: value}
	}
	merged := []flowTransferAttr{
		ev(1, 0, "recipient", wallet), ev(1, 1, "sender", dist), ev(1, 2, "amount", "2uatom"),
		ev(1, 3, "recipient", wallet), ev(1, 4, "sender", pool), ev(1, 5, "amount", "6198375uatom"),
	}
	got := rewardPayoutsFrom(merged, wallet, dist, "uatom")
	require.Len(t, got, 1)
	require.Equal(t, "2", got[7].Amount.String())
	// A second event in the same message (newer SDKs) adds up.
	more := append(merged, ev(2, 0, "recipient", wallet), ev(2, 1, "sender", dist), ev(2, 2, "amount", "5uatom,3ibc/ABC"))
	require.Equal(t, "7", rewardPayoutsFrom(more, wallet, dist, "uatom")[7].Amount.String())
	// The distribution triple to someone else is not this wallet's payout.
	other := []flowTransferAttr{
		ev(3, 0, "recipient", pool), ev(3, 1, "sender", dist), ev(3, 2, "amount", "9uatom"),
		ev(3, 3, "recipient", wallet), ev(3, 4, "sender", pool), ev(3, 5, "amount", "4uatom"),
	}
	require.Empty(t, rewardPayoutsFrom(other, wallet, dist, "uatom"))
}

// A native source correction is a reward proved against the chain. It stays
// as booked and uses its share of the payout, so the indexed reward row in
// the same message keeps only the rest and nothing is counted twice.
func TestFlowRewardPayoutsKeepSourceCorrections(t *testing.T) {
	k := flowAt(1, 5)
	corrected := flowEvent{Key: k, Category: apiReward, Dir: "in", Amount: amt(30), To: flowTestReceiver, Trigger: flowSourceCorrectionTrigger}
	indexed := flowEvent{Key: k, Category: apiReward, Dir: "in", Amount: amt(50), To: flowTestReceiver, Validator: flowTestOther}
	got := reconcileRewardPayouts(flowTestReceiver, []flowEvent{corrected, indexed}, map[uint]flowRewardPayout{k.Message: {Key: k, Amount: amt(50)}})
	require.Len(t, got, 2)
	require.Equal(t, "30", got[0].Amount.String())
	require.Equal(t, flowSourceCorrectionTrigger, got[0].Trigger)
	require.Equal(t, "20", got[1].Amount.String())
	got = reconcileRewardPayouts(flowTestReceiver, []flowEvent{corrected}, nil)
	require.Equal(t, []flowEvent{corrected}, got)
	got = reconcileRewardPayouts(flowTestReceiver, []flowEvent{corrected}, map[uint]flowRewardPayout{k.Message: {Key: k, Amount: amt(30)}})
	require.Equal(t, []flowEvent{corrected}, got)
}
