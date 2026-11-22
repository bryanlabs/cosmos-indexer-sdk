package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// flowTraceDigest hashes everything a trace decided: wallet order, hops,
// openings, pools, overdraws, fates, commission, stops and unresolved
// wallets. Two traces with the same digest give the same rows.
func flowTraceDigest(tr *flowTrace) string {
	type pool struct {
		Parts []flowPart
	}
	pools := func(m map[string]*flowPool) []any {
		var out []any
		for _, k := range sortedPoolKeys(m) {
			out = append(out, k, m[k].parts)
		}
		return out
	}
	var wallets []any
	for _, w := range tr.order {
		var rules []any
		for r := 0; r < flowRules; r++ {
			p := w.pools[r]
			rules = append(rules, p.liquid.parts, p.locked.parts, pools(p.staked), pools(p.unbonding), pools(p.bids))
		}
		wallets = append(wallets, w.Addr, w.Hop, w.Since, w.Known, w.OpeningSource, w.Overdraw, rules)
	}
	var fates []string
	for k, v := range tr.Fates {
		fates = append(fates, fmt.Sprintf("%d|%s|%s|%s|%t|%s", k.Rule, k.Month, k.Class, k.Detail, k.ViaUnbonding, v.String()))
	}
	sort.Strings(fates)
	var commission []string
	for k, v := range tr.Commission {
		commission = append(commission, k+"|"+v.String())
	}
	sort.Strings(commission)
	var stops []string
	for k, v := range tr.stops {
		stops = append(stops, k+"|"+v.Class+"|"+v.Detail)
	}
	sort.Strings(stops)
	b, err := json.Marshal([]any{wallets, fates, commission, stops, tr.Unresolved, tr.appliedQueuedEvents})
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// prefetchFlowLedger is the constructed ledger with read-ahead hints. It
// answers exactly as fakeFlowLedger does and records every call and hint.
type prefetchFlowLedger struct {
	*fakeFlowLedger
	slots    int
	mu       sync.Mutex
	calls    []string
	hinted   map[string]bool
	open     map[string]bool
	maxOpen  int
	hits     int
	released int
}

func (l *prefetchFlowLedger) prefetchSlots() int { return l.slots }
func (l *prefetchFlowLedger) prefetch(addr string, day, end time.Time, follow, needStop bool) {
	if l.open[addr] {
		panic("hinted twice while outstanding")
	}
	l.hinted[addr], l.open[addr] = true, true
	l.maxOpen = max(l.maxOpen, len(l.open))
}

func (l *prefetchFlowLedger) release(addr string) {
	if l.open[addr] {
		l.released++
	}
	delete(l.open, addr)
}

func (l *prefetchFlowLedger) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	l.calls = append(l.calls, "opening|"+addr+"|"+day.String())
	if l.open[addr] {
		l.hits++
	}
	return l.fakeFlowLedger.opening(addr, day)
}

func (l *prefetchFlowLedger) events(addr string, start, end time.Time) ([]flowEvent, error) {
	l.calls = append(l.calls, "events|"+addr+"|"+start.String())
	return l.fakeFlowLedger.events(addr, start, end)
}

func (l *prefetchFlowLedger) stop(addr string, start, end time.Time) (flowStop, error) {
	l.calls = append(l.calls, "stop|"+addr)
	return l.fakeFlowLedger.stop(addr, start, end)
}

// recordingFlowLedger is the same ledger without hints.
type recordingFlowLedger struct {
	*fakeFlowLedger
	calls []string
}

func (l *recordingFlowLedger) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	l.calls = append(l.calls, "opening|"+addr+"|"+day.String())
	return l.fakeFlowLedger.opening(addr, day)
}

func (l *recordingFlowLedger) events(addr string, start, end time.Time) ([]flowEvent, error) {
	l.calls = append(l.calls, "events|"+addr+"|"+start.String())
	return l.fakeFlowLedger.events(addr, start, end)
}

func (l *recordingFlowLedger) stop(addr string, start, end time.Time) (flowStop, error) {
	l.calls = append(l.calls, "stop|"+addr)
	return l.fakeFlowLedger.stop(addr, start, end)
}

// randomFlowLedger builds a ledger of n wallets with commission, transfers,
// staking, fees and stops, the same for the same seed.
func randomFlowLedger(seed int64, n int) (*fakeFlowLedger, []string) {
	rng := rand.New(rand.NewSource(seed))
	l := newFakeFlowLedger()
	addrs := make([]string, n)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("cosmos1w%04d", i)
		if rng.Intn(4) == 0 {
			l.openings[addrs[i]] = flowHoldings{Liquid: amt(int64(rng.Intn(500)))}
		}
	}
	for i := 0; i < n/10; i++ {
		classes := []string{flowExchange, flowModule, flowContract, flowBusyWallet, flowExchangeLike}
		l.stops[addrs[rng.Intn(n)]] = flowStop{Class: classes[rng.Intn(len(classes))], Detail: "x"}
	}
	receivers := []string{addrs[0], addrs[1]}
	delete(l.stops, addrs[0])
	delete(l.stops, addrs[1])
	minute := 0
	for day := 0; day < 80; day++ {
		for k := 0; k < 3; k++ {
			minute++
			l.commission(flowAt(day, minute), receivers[rng.Intn(2)], int64(10+rng.Intn(200)))
		}
		for k := 0; k < 4*n/10; k++ {
			minute++
			from, to := addrs[rng.Intn(n)], addrs[rng.Intn(n)]
			if rng.Intn(3) == 0 {
				from = receivers[rng.Intn(2)]
			}
			if rng.Intn(20) == 0 {
				to = flowTestExchange
			}
			key := flowAt(day, minute)
			if rng.Intn(10) == 0 {
				// Two legs of one message share a key.
				key = flowAt(day, minute-1)
			}
			l.send(key, from, to, int64(1+rng.Intn(120)))
			switch rng.Intn(6) {
			case 0:
				minute++
				l.rows[from] = append(l.rows[from], flowEvent{Key: flowAt(day, minute), Category: apiFee, Dir: apiOut, Amount: amt(1), From: from})
			case 1:
				minute++
				l.rows[to] = append(l.rows[to], flowEvent{Key: flowAt(day, minute), Category: flowCatDelegate, Dir: apiOut, Amount: amt(int64(1 + rng.Intn(30))), From: to, Validator: flowTestOther})
			}
		}
	}
	for a, rows := range l.rows {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Key.before(rows[j].Key) })
		l.rows[a] = rows
	}
	return l, receivers
}

func TestFlowPrefetchHintsLeaveTraceUnchanged(t *testing.T) {
	for _, hops := range []int{0, 5, 8} {
		testFlowPrefetchHints(t, hops)
	}
}

// testFlowPrefetchHints runs every seed and slot count at one hop limit
// (zero is flowMaxHops).
func testFlowPrefetchHints(t *testing.T, hops int) {
	for seed := int64(1); seed <= 12; seed++ {
		for _, slots := range []int{1, 4, 16} {
			base, group := randomFlowLedger(seed, 300)
			asOf := flowTestStart.AddDate(0, 3, 0)
			serial := &recordingFlowLedger{fakeFlowLedger: base}
			a := newFlowTrace(flowTestValidator, flowTestStart, asOf, serial)
			a.maxHops = hops
			require.NoError(t, a.run(group))

			base2, _ := randomFlowLedger(seed, 300)
			ahead := &prefetchFlowLedger{fakeFlowLedger: base2, slots: slots, hinted: map[string]bool{}, open: map[string]bool{}}
			b := newFlowTrace(flowTestValidator, flowTestStart, asOf, ahead)
			b.maxHops = hops
			require.NoError(t, b.run(group))

			require.Equal(t, flowTraceDigest(a), flowTraceDigest(b), "hops %d seed %d slots %d", hops, seed, slots)
			require.Equal(t, serial.calls, ahead.calls, "hops %d seed %d slots %d: loader calls must keep their order", hops, seed, slots)
			require.LessOrEqual(t, ahead.maxOpen, slots)
			require.Empty(t, ahead.open, "every hint is released")
			require.Greater(t, len(a.order), 20, "seed %d discovers wallets", seed)
			if seed == 1 {
				t.Logf("hops %d slots %d: wallets %d, openings hinted %d, released %d, %v", hops, slots, len(b.order), ahead.hits, ahead.released, b.ahead.counts)
			}
		}
	}
}

func TestFlowPrefetchOffForLoadersWithoutSlots(t *testing.T) {
	base, group := randomFlowLedger(3, 100)
	ahead := &prefetchFlowLedger{fakeFlowLedger: base, slots: 0, hinted: map[string]bool{}, open: map[string]bool{}}
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), ahead)
	require.NoError(t, tr.run(group))
	require.Empty(t, ahead.hinted)
	require.Nil(t, tr.ahead.pre)
}

func TestFlowPrefetchWaitDropsQueuedJob(t *testing.T) {
	l := &dbFlowLoader{mu: &sync.Mutex{}, labels: map[string]AddressLabel{}}
	p := newFlowPrefetch(l, 1)
	l.pre = p
	// Without workers started, a hinted job stays queued; the trace then
	// reads inline instead of waiting.
	j := &flowPrefetchJob{addr: "a", done: make(chan struct{})}
	p.jobs["a"] = j
	p.queue = append(p.queue, j)
	require.Nil(t, p.wait("a"))
	require.True(t, j.dropped.Load())
	require.Nil(t, p.jobs["a"])
	// A finished job is returned and forgotten once its events are taken.
	k := &flowPrefetchJob{addr: "b", state: flowJobDone, done: make(chan struct{})}
	close(k.done)
	p.jobs["b"] = k
	require.Same(t, k, p.wait("b"))
	p.forget("b", k)
	require.Nil(t, p.jobs["b"])
	var none *flowPrefetch
	require.Nil(t, none.wait("c"))
	require.Equal(t, 0, (&dbFlowLoader{}).prefetchSlots())
}

func TestFlowPrefetchOptionBounds(t *testing.T) {
	o := CommissionFlowBuildOptions{Version: "v", Build: "b", AsOf: flowTestStart}
	o.WalletPrefetch = -1
	require.Error(t, o.validate())
	o.WalletPrefetch = maxCommissionWalletPrefetch + 1
	require.Error(t, o.validate())
	o.WalletPrefetch = 8
	require.NoError(t, o.validate())
}
