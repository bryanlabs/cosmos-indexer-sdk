package servicehttp

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	t     time.Time
	waits []time.Duration
}

func (c *fakeClock) retry(d time.Duration, logs *[]string) Retry {
	return Retry{For: d, Max: 30 * time.Second, now: func() time.Time { return c.t }, sleep: func(w time.Duration) { c.waits = append(c.waits, w); c.t = c.t.Add(w) },
		Logf: func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) }}
}

func TestRetryRidesOutFiveHundredsAndKeepsTheAnswer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-cosmos-block-height") != "34020844" {
			t.Errorf("height header lost on retry: %q", r.Header.Get("x-cosmos-block-height"))
		}
		if hits.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"balances":[{"denom":"wrong","amount":"1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"balances":[{"denom":"uatom","amount":"7"}]}`))
	}))
	defer srv.Close()
	clock := &fakeClock{t: time.Unix(0, 0)}
	var logs []string
	e, err := New(&http.Client{Timeout: time.Second}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	e = e.WithRetry(clock.retry(10*time.Minute, &logs))
	var out struct {
		Balances []struct{ Denom, Amount string } `json:"balances"`
	}
	addr := "cosmos1" + strings.Repeat("q", 38)
	if err := e.GetJSONWithHeaders("/cosmos/bank/v1beta1/balances/"+addr, nil, map[string]string{"x-cosmos-block-height": "34020844"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Balances) != 1 || out.Balances[0].Denom != "uatom" || out.Balances[0].Amount != "7" {
		t.Fatalf("answer mixed with a failed one: %+v", out)
	}
	if hits.Load() != 4 || len(logs) != 3 {
		t.Fatalf("hits %d logs %d", hits.Load(), len(logs))
	}
	if fmt.Sprint(clock.waits) != "[1s 2s 4s]" {
		t.Fatalf("backoff %v", clock.waits)
	}
	for _, l := range logs {
		if strings.Contains(l, addr) || !strings.Contains(l, "HTTP 503") || !strings.Contains(l, "<address>") {
			t.Fatalf("retry log %q", l)
		}
	}
}

func TestRetryGivesUpAfterItsWindowAndSaysSo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + ln.Addr().String()
	ln.Close()
	clock := &fakeClock{t: time.Unix(0, 0)}
	var logs []string
	e, err := New(&http.Client{Timeout: time.Second}, origin)
	if err != nil {
		t.Fatal(err)
	}
	e = e.WithRetry(clock.retry(10*time.Minute, &logs))
	err = e.GetJSON("/cosmos/base/tendermint/v1beta1/blocks/latest", nil, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "over 10m") {
		t.Fatalf("err %v", err)
	}
	var total time.Duration
	for _, w := range clock.waits {
		if w > 30*time.Second {
			t.Fatalf("wait %s over the cap", w)
		}
		total += w
	}
	if total < 10*time.Minute || total > 10*time.Minute+time.Second {
		t.Fatalf("retried for %s", total)
	}
	if !strings.Contains(logs[len(logs)-1], "giving up") || !strings.Contains(logs[0], "connection refused") {
		t.Fatalf("logs %q", logs)
	}
}

func TestRetryLeavesOtherFailuresAlone(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/bad":
			_, _ = w.Write([]byte(`{"balances":`))
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	clock := &fakeClock{t: time.Unix(0, 0)}
	var logs []string
	e, err := New(&http.Client{Timeout: 100 * time.Millisecond}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	e = e.WithRetry(clock.retry(10*time.Minute, &logs))
	for _, p := range []string{"/missing", "/bad", "/slow"} {
		if err := e.GetJSON(p, nil, &map[string]any{}); err == nil {
			t.Fatalf("%s accepted", p)
		}
	}
	if hits.Load() != 3 || len(logs) != 0 {
		t.Fatalf("a 4xx, a body that broke while decoding or a timeout was retried: hits %d logs %q", hits.Load(), logs)
	}
}

func TestNoRetryUnlessAsked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	e, err := New(&http.Client{Timeout: time.Second}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.GetJSON("/x", nil, &struct{}{}); err == nil || err.Error() != "service HTTP status 502" || hits.Load() != 1 {
		t.Fatalf("err %v hits %d", err, hits.Load())
	}
}

func TestRetryRecoversFromResetConnections(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	clock := &fakeClock{t: time.Unix(0, 0)}
	var logs []string
	e, err := New(&http.Client{Timeout: time.Second}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	e = e.WithRetry(clock.retry(10*time.Minute, &logs))
	var out map[string]bool
	if err := e.GetJSON("/x", nil, &out); err != nil || !out["ok"] || hits.Load() != 3 || len(logs) != 2 {
		t.Fatalf("err %v out %v hits %d logs %q", err, out, hits.Load(), logs)
	}
}
