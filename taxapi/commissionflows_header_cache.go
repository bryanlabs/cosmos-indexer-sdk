package taxapi

import "sync"

// Only validated, exact-height block responses enter this per-endpoint cache.
// Receipts are still attached separately to every requesting wallet.
const flowHeaderCacheBytes = 16 << 20
const flowHeaderCacheEntries = 16

type flowHeaderFlight struct {
	done    chan struct{}
	raw     string
	err     error
	waiters int
}

type flowHeaderCache struct {
	mu      sync.Mutex
	entries map[int64]string
	order   []int64
	bytes   int
	flights map[int64]*flowHeaderFlight
}

func (c *flowHeaderCache) get(height int64, fetch func() (string, error)) (string, bool, error) {
	for {
		c.mu.Lock()
		if raw, ok := c.entries[height]; ok {
			c.mu.Unlock()
			return raw, true, nil
		}
		if f := c.flights[height]; f != nil {
			f.waiters++
			c.mu.Unlock()
			<-f.done
			if f.err != nil {
				continue
			}
			return f.raw, true, nil
		}
		if c.flights == nil {
			c.flights = make(map[int64]*flowHeaderFlight)
		}
		f := &flowHeaderFlight{done: make(chan struct{})}
		c.flights[height] = f
		c.mu.Unlock()

		raw, err := fetch()
		c.mu.Lock()
		if err == nil && len(raw) <= flowHeaderCacheBytes {
			if c.entries == nil {
				c.entries = make(map[int64]string)
			}
			for len(c.order) > 0 && (len(c.entries) >= flowHeaderCacheEntries || c.bytes+len(raw) > flowHeaderCacheBytes) {
				old := c.order[0]
				c.order = c.order[1:]
				c.bytes -= len(c.entries[old])
				delete(c.entries, old)
			}
			c.entries[height] = raw
			c.order = append(c.order, height)
			c.bytes += len(raw)
		}
		f.raw, f.err = raw, err
		delete(c.flights, height)
		close(f.done)
		c.mu.Unlock()
		return raw, false, err
	}
}
