package taxapi

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/shopspring/decimal"
)

// CommissionSharedHistory keeps, for one taxledger process, the index reads
// commission traces make, keyed by exactly what each read is for: the chain,
// the wallet, and its window, day or limit. A trace that asks for a key
// another trace already read gets that read's result and the private read
// receipts it produced, so a run over many validators or several hop limits
// reads each wallet window from the index once. Results do not depend on it:
// a key is shared only when the read it stands for is the same read.
type CommissionSharedHistory struct {
	limit int64

	mu      sync.Mutex
	entries map[flowSharedKey]*flowSharedEntry
	lru     *list.List
	bytes   int64

	hits, misses, evicted atomic.Int64

	// heavy is kept outside the LRU, so the busy cutoff is not counted again
	// for a wallet whose count entries were evicted by event reads.
	heavy flowHeavyBook
}

// NewCommissionSharedHistory keeps up to limitBytes of finished reads,
// dropping the least recently used past that. A dropped key is read again
// when a trace next asks for it.
func NewCommissionSharedHistory(limitBytes int64) *CommissionSharedHistory {
	return &CommissionSharedHistory{limit: limitBytes, entries: map[flowSharedKey]*flowSharedEntry{}, lru: list.New()}
}

// Stats is hits, misses and evictions so far.
func (c *CommissionSharedHistory) Stats() (hits, misses, evicted, bytes int64) {
	c.mu.Lock()
	bytes = c.bytes
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evicted.Load(), bytes
}

const (
	flowSharedEvents byte = iota + 1
	flowSharedHoldings
	flowSharedWalletRows
	flowSharedMemo
	flowSharedFanout
	flowSharedWalletRowsBefore
	flowSharedSweep
)

type flowSharedKey struct {
	kind       byte
	chain      string
	addr       string
	start, end time.Time
	n          int
	strict     bool
}

type flowSharedEntry struct {
	done   chan struct{}
	ok     bool
	events []flowEvent
	h      flowHoldings
	source CommissionFlowHoldingSource
	n      int64
	// reader holds the private read receipts the read produced.
	reader *Server
	size   int64
	elem   *list.Element
}

// read returns k's finished entry, filling it with fill when no trace has.
// A failed read is not kept: the caller gets its error, and a trace that was
// waiting on it reads again itself, as it would without sharing.
func (c *CommissionSharedHistory) read(k flowSharedKey, fill func(*flowSharedEntry) error) (*flowSharedEntry, error) {
	for {
		c.mu.Lock()
		if e, found := c.entries[k]; found {
			if e.elem != nil {
				c.lru.MoveToFront(e.elem)
			}
			c.mu.Unlock()
			<-e.done
			if e.ok {
				c.hits.Add(1)
				return e, nil
			}
			continue
		}
		e := &flowSharedEntry{done: make(chan struct{})}
		c.entries[k] = e
		c.mu.Unlock()
		err := fill(e)
		c.misses.Add(1)
		c.mu.Lock()
		if err != nil {
			delete(c.entries, k)
		} else {
			e.ok = true
			e.elem = c.lru.PushFront(k)
			c.bytes += e.size
			for c.bytes > c.limit && c.lru.Len() > 1 {
				old := c.lru.Remove(c.lru.Back()).(flowSharedKey)
				c.bytes -= c.entries[old].size
				delete(c.entries, old)
				c.evicted.Add(1)
			}
		}
		c.mu.Unlock()
		close(e.done)
		return e, err
	}
}

// sharedReader is a reader like the trace's own, with its own receipt
// journals, for one read whose receipts the shared entry keeps. It shares
// the trace's caches but not the shared history, so it reads the index.
func (s *Server) sharedReader() *Server {
	r := &Server{db: s.db, oracle: s.oracle, native: s.native, readOnlyLedger: s.readOnlyLedger, privateReadSnapshotTimeout: s.privateReadSnapshotTimeout,
		commissionPrivateProfile: s.commissionPrivateProfile, commissionSourceCorrections: s.commissionSourceCorrections, covRollup: s.covRollup, staking: s.staking}
	if s.privateProjectionReceipts != nil {
		r.privateProjectionReceipts = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	}
	if s.sourceCorrectionReceipts != nil {
		r.sourceCorrectionReceipts = newCorrectionJournal()
	}
	return r
}

func (r *Server) sharedReceiptsSize() int64 {
	var n int64
	if r.privateProjectionReceipts != nil {
		r.privateProjectionReceipts.mu.Lock()
		n += int64(len(r.privateProjectionReceipts.rows)) * 512
		r.privateProjectionReceipts.mu.Unlock()
	}
	if r.sourceCorrectionReceipts != nil {
		n += int64(len(r.sourceCorrectionReceipts.receipts())) * 512
	}
	return n
}

const flowSharedEventBytes = int64(unsafe.Sizeof(flowEvent{})) + 192

func (s *Server) sharedFlowEvents(chain, addr string, start, end time.Time) ([]flowEvent, error) {
	e, err := s.flowShared.read(flowSharedKey{kind: flowSharedEvents, chain: chain, addr: addr, start: start, end: end}, func(e *flowSharedEntry) error {
		r := s.sharedReader()
		events, err := r.readFlowEvents(chain, addr, start, end)
		e.events, e.reader = events, r
		e.size = 256 + int64(len(events))*flowSharedEventBytes + r.sharedReceiptsSize()
		return err
	})
	if err != nil {
		return nil, err
	}
	s.mergePrefetchReceipts(e.reader)
	return append([]flowEvent(nil), e.events...), nil
}

func (s *Server) sharedFlowHoldingsAt(chain, addr string, day time.Time, strict bool) (flowHoldings, CommissionFlowHoldingSource, error) {
	e, err := s.flowShared.read(flowSharedKey{kind: flowSharedHoldings, chain: chain, addr: addr, start: day, strict: strict}, func(e *flowSharedEntry) error {
		r := s.sharedReader()
		h, source, err := r.readFlowHoldingsAt(chain, addr, day, strict)
		e.h, e.source, e.reader = h, source, r
		e.size = 512 + int64(len(h.Staked)+len(h.Unbonding))*160 + r.sharedReceiptsSize()
		return err
	})
	if err != nil {
		return flowHoldings{}, CommissionFlowHoldingSource{}, err
	}
	s.mergePrefetchReceipts(e.reader)
	return e.h.clone(), e.source, nil
}

// sharedCount is a count read that produces no receipts.
func (s *Server) sharedCount(k flowSharedKey, read func() (int64, error)) (int64, error) {
	e, err := s.flowShared.read(k, func(e *flowSharedEntry) error {
		n, err := read()
		e.n, e.size = n, 192
		return err
	})
	if err != nil {
		return 0, err
	}
	return e.n, nil
}

func (h flowHoldings) clone() flowHoldings {
	out := h
	if h.Staked != nil {
		out.Staked = make(map[string]decimal.Decimal, len(h.Staked))
		for k, v := range h.Staked {
			out.Staked[k] = v
		}
	}
	if h.Unbonding != nil {
		out.Unbonding = make(map[string]decimal.Decimal, len(h.Unbonding))
		for k, v := range h.Unbonding {
			out.Unbonding[k] = v
		}
	}
	return out
}
