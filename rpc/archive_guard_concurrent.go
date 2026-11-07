package rpc

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// concurrentRoundTrip limits both request starts and full response lifetimes.
func (g *ArchiveGuard) concurrentRoundTrip(req *http.Request) (*http.Response, error) {
	g.slotsOnce.Do(func() { g.slots = make(chan struct{}, g.MaxConcurrent) })
	select {
	case g.slots <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	release := func() { <-g.slots }
	var sequence uint64
	for {
		g.mu.Lock()
		if g.opened != nil {
			err := g.opened
			g.mu.Unlock()
			release()
			return nil, err
		}
		if g.Budget == 0 || g.sent >= g.Budget {
			err := g.open(fmt.Errorf("request budget exhausted (%d)", g.sent))
			g.mu.Unlock()
			release()
			return nil, err
		}
		if err := req.Context().Err(); err != nil {
			g.mu.Unlock()
			release()
			return nil, err
		}
		if wait := time.Until(g.last.Add(g.Interval)); wait > 0 {
			g.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
				continue
			case <-req.Context().Done():
				timer.Stop()
				release()
				return nil, req.Context().Err()
			}
		}
		g.sent++
		sequence = g.sent
		g.last = time.Now()
		g.mu.Unlock()
		break
	}

	transport := g.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	if inspectionErr := g.inspectResponse(req, response, sequence); inspectionErr != nil {
		release()
		g.mu.Lock()
		if g.opened == nil {
			err = g.open(inspectionErr)
		} else {
			err = g.opened
		}
		g.mu.Unlock()
		return nil, err
	}
	response.Body = &archiveSlotBody{ReadCloser: response.Body, release: release}
	return response, nil
}

type archiveSlotBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *archiveSlotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
