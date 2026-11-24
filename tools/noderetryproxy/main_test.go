package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testProxy(t *testing.T, origin string, retryFor time.Duration) (*proxy, *httptest.Server, *[]string) {
	t.Helper()
	var logs []string
	p, err := newProxy(origin, retryFor, 2*time.Second, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(0, 0)
	p.now = func() time.Time { return clock }
	p.sleep = func(d time.Duration) { clock = clock.Add(d) }
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return p, front, &logs
}

func TestProxyRetriesFiveHundredsAndPassesTheAnswerThrough(t *testing.T) {
	var hits atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-cosmos-block-height") != "34020844" || r.URL.RawQuery != "pagination.limit=1000" {
			t.Errorf("request changed: %q %q", r.Header.Get("x-cosmos-block-height"), r.URL.RawQuery)
		}
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"wrong":true}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balances":[]}`))
	}))
	defer node.Close()
	p, front, logs := testProxy(t, node.URL, 20*time.Second)
	addr := "cosmos1" + strings.Repeat("q", 38)
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/cosmos/bank/v1beta1/balances/"+addr+"?pagination.limit=1000", nil)
	req.Header.Set("x-cosmos-block-height", "34020844")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"balances":[]}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if hits.Load() != 3 || p.retries.Load() != 2 || len(*logs) != 2 {
		t.Fatalf("hits %d retries %d logs %q", hits.Load(), p.retries.Load(), *logs)
	}
	for _, l := range *logs {
		if strings.Contains(l, addr) || !strings.Contains(l, "HTTP 500") {
			t.Fatalf("log %q", l)
		}
	}
}

func TestProxyRidesOutARefusingNodeThenGivesUpWith502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + ln.Addr().String()
	ln.Close()
	p, front, logs := testProxy(t, origin, 20*time.Second)
	resp, err := http.Get(front.URL + "/cosmos/base/tendermint/v1beta1/blocks/latest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "connection refused") || p.giveUps.Load() != 1 {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if !strings.Contains((*logs)[len(*logs)-1], "giving up") || p.retries.Load() < 10 {
		t.Fatalf("retries %d logs %q", p.retries.Load(), *logs)
	}
}

func TestProxyLeavesFourHundredsAndOtherMethodsAlone(t *testing.T) {
	var hits atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer node.Close()
	p, front, _ := testProxy(t, node.URL, 20*time.Second)
	resp, err := http.Get(front.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || hits.Load() != 1 || p.retries.Load() != 0 {
		t.Fatalf("404 changed: %d hits %d", resp.StatusCode, hits.Load())
	}
	resp, err = http.Post(front.URL+"/x", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || hits.Load() != 1 {
		t.Fatalf("POST forwarded: %d", resp.StatusCode)
	}
}

func TestProxyRefusesNonLoopbackAndBadOrigins(t *testing.T) {
	for _, o := range []string{"file:///etc/passwd", "http://u:p@node:1317", "http://node:1317?x=1", "node:1317"} {
		if _, err := newProxy(o, time.Second, time.Second, nil); err == nil {
			t.Fatalf("accepted %q", o)
		}
	}
}
