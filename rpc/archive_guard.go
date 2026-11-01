package rpc

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ArchiveGuard is scoped to one indexing run and shared by all its HTTP clients.
// Requests are serial by default. Opt-in bounded concurrency still paces every
// request start and prevents new reservations after a denial. Already in-flight
// requests cannot be recalled. It does not replace http.DefaultTransport.
type ArchiveGuard struct {
	mu            sync.Mutex
	Transport     http.RoundTripper
	Interval      time.Duration
	Budget        uint64
	sent          uint64
	last          time.Time
	opened        error
	OnOpen        func(error)
	MaxConcurrent int
	EvidenceDir   string
	slotsOnce     sync.Once
	slots         chan struct{}
}

func (g *ArchiveGuard) open(err error) error {
	g.opened = fmt.Errorf("archive request circuit open: %w", err)
	if g.OnOpen != nil {
		g.OnOpen(g.opened)
	}
	return g.opened
}

func (g *ArchiveGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.MaxConcurrent > 1 {
		return g.concurrentRoundTrip(req)
	}
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
	if err := g.inspectResponse(req, resp, g.sent); err != nil {
		return nil, g.open(err)
	}
	return resp, nil
}
