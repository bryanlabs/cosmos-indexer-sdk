package rpc

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestArchiveGuardGlobalBudgetAndPacing(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	interval := 30 * time.Millisecond
	guard := &ArchiveGuard{Interval: interval, Budget: 3}
	clients := []*http.Client{{Transport: guard}, {Transport: guard}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := clients[i%2].Get(server.URL)
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	if len(times) != 3 {
		t.Fatalf("sent %d requests, want 3", len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < interval-5*time.Millisecond {
			t.Fatal("global pacing violated")
		}
	}
}

func TestArchiveGuardPermanentDenial(t *testing.T) {
	for _, status := range []int{403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, opens := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) }))
			defer server.Close()
			guard := &ArchiveGuard{Budget: 10, OnOpen: func(error) { opens++ }}
			client := &http.Client{Transport: guard}
			for i := 0; i < 3; i++ {
				if _, err := client.Get(server.URL); err == nil {
					t.Fatal("expected closed circuit error")
				}
			}
			if calls != 1 || opens != 1 {
				t.Fatalf("calls=%d opens=%d", calls, opens)
			}
		})
	}
}

func TestArchiveGuardTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := &http.Client{Transport: &ArchiveGuard{Budget: 1}, Timeout: 20 * time.Millisecond}
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("expected timeout")
	}
}
