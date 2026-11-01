package rpc

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ArchiveGuard is scoped to one indexing run and shared by all its HTTP clients.
// Serializing requests prevents another worker from sending after a denial.
// It deliberately does not replace http.DefaultTransport.
type ArchiveGuard struct {
	mu        sync.Mutex
	Transport http.RoundTripper
	Interval  time.Duration
	Budget    uint64
	sent      uint64
	last      time.Time
	opened    error
	OnOpen    func(error)
}

func (g *ArchiveGuard) open(err error) error {
	g.opened = fmt.Errorf("archive request circuit open: %w", err)
	if g.OnOpen != nil {
		g.OnOpen(g.opened)
	}
	return g.opened
}

func (g *ArchiveGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.opened != nil {
		return nil, g.opened
	}
	if g.Budget == 0 || g.sent >= g.Budget {
		return nil, g.open(fmt.Errorf("request budget exhausted (%d)", g.sent))
	}
	if wait := time.Until(g.last.Add(g.Interval)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	g.sent++
	g.last = time.Now()
	transport := g.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, g.open(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	return resp, nil
}
