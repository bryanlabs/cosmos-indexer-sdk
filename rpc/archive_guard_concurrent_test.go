package rpc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestArchiveGuardConcurrentBounded(t *testing.T) {
	var active, peak atomic.Int32
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	guard := &ArchiveGuard{Budget: 6, Interval: 10 * time.Millisecond, MaxConcurrent: 2}
	client := &http.Client{Transport: guard, Timeout: time.Second}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}()
	}
	wg.Wait()
	require.Equal(t, int32(2), peak.Load())
	require.Len(t, starts, 6)
	for i := 1; i < len(starts); i++ {
		require.GreaterOrEqual(t, starts[i].Sub(starts[i-1]), 7*time.Millisecond)
	}
	_, err := client.Get(server.URL)
	require.ErrorContains(t, err, "budget exhausted")
}

func TestArchiveGuardConcurrentDenialStopsQueuedStarts(t *testing.T) {
	for _, status := range []int{403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			guard := &ArchiveGuard{Budget: 20, Interval: 80 * time.Millisecond, MaxConcurrent: 4}
			client := &http.Client{Transport: guard, Timeout: time.Second}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					response, err := client.Get(server.URL)
					if err == nil {
						_ = response.Body.Close()
						t.Error("expected permanently open circuit")
					}
				}()
			}
			wg.Wait()
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestArchiveGuardConcurrentHoldsSlotUntilClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("response"))
	}))
	defer server.Close()
	guard := &ArchiveGuard{Budget: 10, MaxConcurrent: 2}
	client := &http.Client{Transport: guard}
	first, err := client.Get(server.URL)
	require.NoError(t, err)
	defer first.Body.Close()
	second, err := client.Get(server.URL)
	require.NoError(t, err)
	defer second.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.Error(t, err)
}

type inspectReplayTransport func(*http.Request) (*http.Response, error)

func (f inspectReplayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNoReplayPOSTTransport(t *testing.T) {
	calls := 0
	transport := NoReplayPOSTTransport{Transport: inspectReplayTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Nil(t, r.GetBody)
		require.Empty(t, r.Header.Get("Idempotency-Key"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	require.NotNil(t, req.GetBody)
	req.Header.Set("Idempotency-Key", "must-not-replay")
	response, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.NotNil(t, req.GetBody, "original request must not be mutated")
	get, err := http.NewRequest(http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(get)
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
