package taxapi

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"
)

// flowPrefetcher is a loader that can start a wallet's index reads before the
// trace asks for them. The trace still asks for every read, in its own order,
// and gets the answer it would get without the hint: a hint only lets the
// database part of the read run early, beside the trace.
type flowPrefetcher interface {
	// prefetchSlots is how many wallets may be read ahead at once.
	prefetchSlots() int
	// prefetch starts reading addr as the trace would discover it: its stop
	// check when needStop, then, when follow, its holdings at the end of
	// day and its events from the next day to end.
	prefetch(addr string, day, end time.Time, follow, needStop bool)
	// release drops what was read ahead for addr and not asked for.
	release(addr string)
}

// flowAheadCand is a queued transfer out to a wallet the trace may discover.
type flowAheadCand struct {
	key    flowKey
	seq    int
	wallet *flowWallet
	to     string
}

type flowAheadHeap []flowAheadCand

func (h flowAheadHeap) Len() int { return len(h) }
func (h flowAheadHeap) Less(i, j int) bool {
	return flowQueueOrder(h[i].key, h[i].seq, h[j].key, h[j].seq)
}
func (h flowAheadHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *flowAheadHeap) Push(x any)   { *h = append(*h, x.(flowAheadCand)) }
func (h *flowAheadHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// flowQueueOrder is the trace queue's order: key, then queue sequence.
func flowQueueOrder(k1 flowKey, s1 int, k2 flowKey, s2 int) bool {
	if k1 == k2 {
		return s1 < s2
	}
	return k1.before(k2)
}

// flowAhead picks which wallets to read ahead. It only issues hints; what
// the trace applies and discovers is decided by the queue alone.
type flowAhead struct {
	checked bool
	pre     flowPrefetcher
	cands   flowAheadHeap
	// hinted is the transfer each outstanding hint was made for, by wallet.
	hinted map[string]flowAheadCand
	// counts is what became of the transfers looked at, for diagnosis.
	counts map[string]int
}

// flowAheadScan bounds how many queued transfers, per slot, lookAhead looks
// at in one step.
const flowAheadScan = 8

func (a *flowAhead) count(what string) {
	if a.counts == nil {
		a.counts = map[string]int{}
	}
	a.counts[what]++
}

func (a *flowAhead) offer(load flowLoader, q flowQueued) {
	if !a.checked {
		a.checked = true
		if p, ok := load.(flowPrefetcher); ok && p.prefetchSlots() > 0 {
			a.pre, a.hinted = p, map[string]flowAheadCand{}
		}
	}
	e := q.event
	if a.pre == nil || e.Category != apiTransfer || e.Dir != apiOut || e.To == "" || e.From == e.To {
		return
	}
	a.count("offered")
	heap.Push(&a.cands, flowAheadCand{key: q.key, seq: q.seq, wallet: q.wallet, to: e.To})
}

func (a *flowAhead) stop() {
	if a.pre == nil {
		return
	}
	for addr := range a.hinted {
		a.pre.release(addr)
	}
	a.hinted, a.cands = map[string]flowAheadCand{}, nil
}

// lookAhead runs before cur is applied. It keeps hints on the first wallets,
// in queue order, that a queued transfer out may discover: wallets the trace
// does not follow yet and has not stopped at, sent to by a wallet that holds
// coins of a commission month now. The sender test is only a guess at which
// transfer will discover a wallet; a wrong guess costs a read, never a
// result. A hint that a nearer transfer displaces is released.
func (t *flowTrace) lookAhead(cur flowQueued) {
	a := &t.ahead
	if a.pre == nil {
		return
	}
	slots := a.pre.prefetchSlots()
	want := make(map[string]flowAheadCand, slots)
	var keep []flowAheadCand
	looked := 0
	for len(want) < slots && looked < flowAheadScan*slots && a.cands.Len() > 0 {
		c := heap.Pop(&a.cands).(flowAheadCand)
		looked++
		if flowQueueOrder(c.key, c.seq, cur.key, cur.seq) {
			a.count("passed")
			continue
		}
		if c.key == cur.key && c.seq == cur.seq {
			// cur is applied next: its hint, if any, is about to be used.
			if h, ok := a.hinted[c.to]; ok && h.key == c.key && h.seq == c.seq {
				want[c.to] = c
			}
			continue
		}
		if _, traced := t.Wallets[c.to]; traced {
			a.count("traced")
			continue
		}
		follow := c.wallet.Hop < t.hopLimit()
		stop, stopped := t.stops[c.to]
		if stopped && (stop.Class != "" || !follow) {
			a.count("stopped")
			continue
		}
		keep = append(keep, c)
		if _, dup := want[c.to]; dup || !c.wallet.holdsTagged() {
			continue
		}
		want[c.to] = c
	}
	for _, c := range keep {
		heap.Push(&a.cands, c)
	}
	for addr, h := range a.hinted {
		if w, ok := want[addr]; !ok || w.key != h.key || w.seq != h.seq {
			a.pre.release(addr)
			delete(a.hinted, addr)
			a.count("released")
		}
	}
	for addr, c := range want {
		if _, ok := a.hinted[addr]; ok {
			continue
		}
		_, stopped := t.stops[addr]
		a.pre.prefetch(addr, flowDay(c.key.Time).AddDate(0, 0, -1), t.AsOf, c.wallet.Hop < t.hopLimit(), !stopped)
		a.hinted[addr] = c
		a.count("hinted")
	}
}

// holdsTagged reports whether w holds liquid coins of a commission month
// under any rule.
func (w *flowWallet) holdsTagged() bool {
	for r := 0; r < flowRules; r++ {
		for _, x := range w.pools[r].liquid.parts {
			if x.Origin != flowOriginOther && x.Amount.IsPositive() {
				return true
			}
		}
	}
	return false
}

const (
	flowJobQueued = iota
	flowJobRunning
	flowJobDone
)

// flowPrefetchJob is one wallet read ahead. Only reads that succeeded are
// kept; anything that failed is read again inline, so errors surface exactly
// where and as they would without prefetch.
type flowPrefetchJob struct {
	addr     string
	day, end time.Time
	follow   bool
	needStop bool
	state    int
	dropped  atomic.Bool
	done     chan struct{}

	memo     int
	memoOK   bool
	rows     int64
	rowsOK   bool
	held     flowHoldingRead
	heldOK   bool
	events   []flowEvent
	reader   *Server
	eventsOK bool
}

// flowHoldingRead is the index part of a holdings read.
type flowHoldingRead struct {
	h      flowHoldings
	source CommissionFlowHoldingSource
}

// flowPrefetch reads wallets ahead for one trace with a fixed number of
// workers, each one database connection at a time.
type flowPrefetch struct {
	l       *dbFlowLoader
	slots   int
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*flowPrefetchJob
	jobs    map[string]*flowPrefetchJob
	started bool
	closed  bool
}

func newFlowPrefetch(l *dbFlowLoader, slots int) *flowPrefetch {
	if slots < 1 {
		return nil
	}
	p := &flowPrefetch{l: l, slots: slots, jobs: map[string]*flowPrefetchJob{}}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (l *dbFlowLoader) prefetchSlots() int {
	if l.pre == nil {
		return 0
	}
	return l.pre.slots
}

func (l *dbFlowLoader) prefetch(addr string, day, end time.Time, follow, needStop bool) {
	p := l.pre
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.jobs[addr] != nil {
		return
	}
	if !p.started {
		p.started = true
		for i := 0; i < p.slots; i++ {
			go p.work()
		}
	}
	j := &flowPrefetchJob{addr: addr, day: day, end: end, follow: follow, needStop: needStop, done: make(chan struct{})}
	p.jobs[addr] = j
	p.queue = append(p.queue, j)
	p.cond.Signal()
}

func (l *dbFlowLoader) release(addr string) {
	p := l.pre
	if p == nil {
		return
	}
	p.mu.Lock()
	if j := p.jobs[addr]; j != nil {
		j.dropped.Store(true)
		delete(p.jobs, addr)
	}
	p.mu.Unlock()
}

// closePrefetch stops the workers. A read already running finishes in the
// background and is discarded.
func (l *dbFlowLoader) closePrefetch() {
	p := l.pre
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	for _, j := range p.jobs {
		j.dropped.Store(true)
	}
	p.jobs = map[string]*flowPrefetchJob{}
	p.queue = nil
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *flowPrefetch) work() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		j := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		if j.dropped.Load() {
			p.mu.Unlock()
			close(j.done)
			continue
		}
		j.state = flowJobRunning
		p.mu.Unlock()
		p.read(j)
		p.mu.Lock()
		j.state = flowJobDone
		p.mu.Unlock()
		close(j.done)
	}
}

// read makes the same index reads stop, opening and events make, in the
// same order, stopping where the trace would stop.
func (p *flowPrefetch) read(j *flowPrefetchJob) {
	l := p.l
	if j.needStop {
		l.mu.Lock()
		lab, ok := l.labels[j.addr]
		l.mu.Unlock()
		if (ok && lab.Kind != labelValidatorOperator) || len(j.addr) > len("cosmos1")+38+1 {
			return
		}
		n, err := l.s.memoDepositSenders(j.addr)
		if err != nil {
			return
		}
		j.memo, j.memoOK = n, true
		if n >= flowMemoMinSenders || j.dropped.Load() {
			return
		}
		rows, err := l.s.walletRows(j.addr, ledgerHeavyRows)
		if err != nil {
			return
		}
		j.rows, j.rowsOK = rows, true
		if rows > ledgerHeavyRows {
			return
		}
	}
	if !j.follow || j.dropped.Load() {
		return
	}
	h, source, err := l.s.flowHoldingsAt(l.chain, j.addr, j.day, l.private)
	if err != nil {
		return
	}
	j.held, j.heldOK = flowHoldingRead{h: h, source: source}, true
	if j.dropped.Load() {
		return
	}
	r := l.s.prefetchReader()
	events, err := r.flowEvents(l.chain, j.addr, j.day.AddDate(0, 0, 1), j.end)
	if err != nil {
		return
	}
	j.events, j.reader, j.eventsOK = events, r, true
}

// wait returns addr's finished job, or nil when there is none to use. A job
// not yet started is dropped, since reading inline is then no slower.
func (p *flowPrefetch) wait(addr string) *flowPrefetchJob {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	j := p.jobs[addr]
	if j == nil {
		p.mu.Unlock()
		return nil
	}
	if j.state == flowJobQueued {
		j.dropped.Store(true)
		delete(p.jobs, addr)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	<-j.done
	return j
}

func (p *flowPrefetch) forget(addr string, j *flowPrefetchJob) {
	p.mu.Lock()
	if p.jobs[addr] == j {
		delete(p.jobs, addr)
	}
	p.mu.Unlock()
}

// prefetchReader is a database-only Server for reading one wallet's events
// ahead of the trace, like the read snapshot facade. Its receipt journals
// are its own, so a read the trace never asks for leaves no receipt; the
// trace merges them when it takes the events.
func (s *Server) prefetchReader() *Server {
	r := &Server{db: s.db, native: s.native, readOnlyLedger: s.readOnlyLedger, privateReadSnapshotTimeout: s.privateReadSnapshotTimeout, commissionPrivateProfile: s.commissionPrivateProfile, commissionSourceCorrections: s.commissionSourceCorrections, flowShared: s.flowShared}
	if s.privateProjectionReceipts != nil {
		r.privateProjectionReceipts = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	}
	if s.sourceCorrectionReceipts != nil {
		r.sourceCorrectionReceipts = newCorrectionJournal()
	}
	return r
}

func (s *Server) mergePrefetchReceipts(r *Server) {
	if s.privateProjectionReceipts != nil && r.privateProjectionReceipts != nil {
		s.privateProjectionReceipts.merge(r.privateProjectionReceipts)
	}
	if s.sourceCorrectionReceipts != nil && r.sourceCorrectionReceipts != nil {
		s.sourceCorrectionReceipts.merge(r.sourceCorrectionReceipts)
	}
}

// flowClosingReads reads the index part of every traced wallet's closing
// holdings ahead, with at most slots reads at once and at most window
// wallets past the one the closing loop is on. The loop takes them in its
// own order and makes any archive read itself, so receipts keep their order.
type flowClosingReads struct {
	results []chan *flowHoldingRead
	tokens  chan struct{}
	quit    chan struct{}
}

func (l *dbFlowLoader) closingReads(order []*flowWallet, day time.Time) *flowClosingReads {
	slots := l.prefetchSlots()
	if slots < 1 || len(order) == 0 {
		return nil
	}
	c := &flowClosingReads{results: make([]chan *flowHoldingRead, len(order)), tokens: make(chan struct{}, 4*slots), quit: make(chan struct{})}
	for i := range c.results {
		c.results[i] = make(chan *flowHoldingRead, 1)
	}
	sem := make(chan struct{}, slots)
	go func() {
		for i, w := range order {
			select {
			case c.tokens <- struct{}{}:
			case <-c.quit:
				return
			}
			select {
			case sem <- struct{}{}:
			case <-c.quit:
				return
			}
			go func(i int, addr string) {
				defer func() { <-sem }()
				h, source, err := l.s.flowHoldingsAt(l.chain, addr, day, l.private)
				if err != nil {
					c.results[i] <- nil
					return
				}
				c.results[i] <- &flowHoldingRead{h: h, source: source}
			}(i, w.Addr)
		}
	}()
	return c
}

// holdingsAt answers like the loader's holdingsAt for wallet i of the order.
func (c *flowClosingReads) holdingsAt(l *dbFlowLoader, i int, addr string, day time.Time) (flowHoldings, bool, error) {
	if c == nil {
		return l.holdingsAt(addr, day)
	}
	read := <-c.results[i]
	<-c.tokens
	return l.holdingsWith(addr, day, read)
}

func (c *flowClosingReads) close() {
	if c == nil {
		return
	}
	close(c.quit)
}
