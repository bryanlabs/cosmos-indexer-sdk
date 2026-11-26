package taxapi

import (
	"sync"
	"time"
)

// flowHeavyBook remembers, for one taxledger process, what the busy cutoff
// has proved about wallets over it. Every entry is a fact that cannot change
// while the run reads the index, so an answer from the book is the answer a
// fresh count returns:
//
//   - a wallet whose lifetime count was over n once stays over n, and so does
//     one tax_balance_deferred lists with more than n rows (the balance build
//     stores that same count when it defers a wallet);
//   - a wallet over n before end is over n before any later end, and the
//     count returned is then exactly n+1 (readWalletRowsBefore caps it).
//
// Counts at or under the cutoff are not kept here; they stay in the LRU.
type flowHeavyBook struct {
	mu       sync.Mutex
	loaded   bool
	deferred map[string]int64
	lifetime map[flowHeavyKey]bool
	overFrom map[flowHeavyKey]time.Time
}

type flowHeavyKey struct {
	addr string
	n    int
}

// load reads tax_balance_deferred once. A database without the table has no
// deferred wallets, and every lifetime check is counted as before.
func (b *flowHeavyBook) load(s *Server) error {
	if b.loaded {
		return nil
	}
	var exists bool
	if err := s.db.Raw("SELECT to_regclass('tax_balance_deferred') IS NOT NULL").Scan(&exists).Error; err != nil {
		return err
	}
	deferred := map[string]int64{}
	if exists {
		var rows []BalanceDeferred
		if err := s.db.Model(&BalanceDeferred{}).Select("chain, address, rows").Find(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			deferred[r.Address] = max(deferred[r.Address], r.Rows)
		}
	}
	b.deferred, b.lifetime, b.overFrom, b.loaded = deferred, map[flowHeavyKey]bool{}, map[flowHeavyKey]time.Time{}, true
	return nil
}

func (b *flowHeavyBook) lifetimeOver(s *Server, addr string, n int) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(s); err != nil {
		return false, err
	}
	return b.deferred[addr] > int64(n) || b.lifetime[flowHeavyKey{addr, n}], nil
}

func (b *flowHeavyBook) noteLifetimeOver(addr string, n int) {
	b.mu.Lock()
	b.lifetime[flowHeavyKey{addr, n}] = true
	b.mu.Unlock()
}

func (b *flowHeavyBook) overBy(addr string, n int, end time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	from, ok := b.overFrom[flowHeavyKey{addr, n}]
	return ok && !end.Before(from)
}

func (b *flowHeavyBook) noteOver(addr string, n int, end time.Time) {
	b.mu.Lock()
	k := flowHeavyKey{addr, n}
	if from, ok := b.overFrom[k]; !ok || end.Before(from) {
		b.overFrom[k] = end
	}
	b.mu.Unlock()
}

// sharedWalletRowsBefore is walletRowsBefore for a process with shared
// history: the same counts, read at most once per proved fact.
func (s *Server) sharedWalletRowsBefore(addr string, limit int, end time.Time) (int64, error) {
	b := &s.flowShared.heavy
	over, err := b.lifetimeOver(s, addr, limit)
	if err != nil {
		return 0, err
	}
	if !over {
		n, err := s.walletRows(addr, limit)
		if err != nil || n <= int64(limit) {
			return n, err
		}
		b.noteLifetimeOver(addr, limit)
	}
	end = end.UTC()
	if b.overBy(addr, limit, end) {
		return int64(limit) + 1, nil
	}
	n, err := s.sharedCount(flowSharedKey{kind: flowSharedWalletRowsBefore, addr: addr, end: end, n: limit}, func() (int64, error) {
		return s.readWalletRowsBefore(addr, limit, end)
	})
	if err == nil && n > int64(limit) {
		b.noteOver(addr, limit, end)
	}
	return n, err
}
