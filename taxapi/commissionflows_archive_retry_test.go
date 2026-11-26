package taxapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlowArchiveRetriesFailedRequestOnly(t *testing.T) {
	prev := flowArchiveBackoff
	flowArchiveBackoff = func(int) time.Duration { return 0 }
	defer func() { flowArchiveBackoff = prev }()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"result":{"ok":true}}`))
	}))
	defer srv.Close()
	a, err := newFlowArchive(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Result struct{ OK bool } }
	if err := a.response("x", 7, "abci_query", url.Values{"path": {"/p"}}, &out); err != nil || !out.Result.OK {
		t.Fatalf("response %v %+v", err, out)
	}
	if hits.Load() != 2 || a.callAttempts.Load() != 2 || a.callRetries.Load() != 1 || len(a.receipts) != 1 {
		t.Fatalf("hits %d attempts %d retries %d receipts %d", hits.Load(), a.callAttempts.Load(), a.callRetries.Load(), len(a.receipts))
	}
}

func TestFlowArchiveGivesUpAfterBound(t *testing.T) {
	prev := flowArchiveBackoff
	flowArchiveBackoff = func(int) time.Duration { return 0 }
	defer func() { flowArchiveBackoff = prev }()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	a, _ := newFlowArchive(srv.URL)
	var out struct{}
	if err := a.response("x", 7, "abci_query", url.Values{}, &out); err == nil {
		t.Fatal("want error")
	}
	if hits.Load() != int64(flowArchiveAttempts) || len(a.receipts) != 0 {
		t.Fatalf("hits %d receipts %d", hits.Load(), len(a.receipts))
	}
}

func TestFlowArchiveEnvelopeErrorNotRetried(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"height not available"}}`))
	}))
	defer srv.Close()
	a, _ := newFlowArchive(srv.URL)
	var out struct{}
	if err := a.response("x", 7, "abci_query", url.Values{}, &out); err == nil || hits.Load() != 1 {
		t.Fatalf("err %v hits %d", err, hits.Load())
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestFlowArchiveCauseClasses(t *testing.T) {
	cases := map[string]error{
		"timeout":    &url.Error{Op: "Get", URL: "http://secret.example/path", Err: timeoutErr{}},
		"status_429": fmt.Errorf("service HTTP status 429"),
		"decode":     io.ErrUnexpectedEOF,
		"transport":  &url.Error{Op: "Get", URL: "http://secret.example/path", Err: fmt.Errorf("connection refused")},
	}
	for want, err := range cases {
		if got := flowArchiveCause(err); got != want {
			t.Fatalf("%v: got %s want %s", err, got, want)
		}
	}
	var syn json.RawMessage
	if got := flowArchiveCause(json.Unmarshal([]byte(`{"a":`), &syn)); got != "decode" {
		t.Fatalf("syntax error class %s", got)
	}
}

func TestFlowArchiveCausesCountedWithoutURL(t *testing.T) {
	prev := flowArchiveBackoff
	flowArchiveBackoff = func(int) time.Duration { return 0 }
	defer func() { flowArchiveBackoff = prev }()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			http.Error(w, "busy", http.StatusTooManyRequests)
		case 2:
			w.Write([]byte(`{"jsonrpc":"2.0","res`))
		default:
			http.Error(w, "down", http.StatusBadGateway)
		}
	}))
	secret := srv.URL + "/dedicated-secret-path"
	a, _ := newFlowArchive(secret)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	var out struct{}
	err := a.response("x", 7, "abci_query", url.Values{}, &out)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "status_502") {
		t.Fatalf("error %v", err)
	}
	got := a.causeCounts()
	if got["status_429"] != 1 || got["decode"] != 1 || got["status_502"] != 3 || a.callRetries.Load() != 4 {
		t.Fatalf("causes %v retries %d", got, a.callRetries.Load())
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "127.0.0.1") || !strings.Contains(logs.String(), "status_429; retrying") {
		t.Fatalf("log %q", logs.String())
	}
	srv.Close()
	b, _ := newFlowArchive(secret)
	if err := b.response("x", 7, "abci_query", url.Values{}, &out); err == nil || strings.Contains(err.Error(), "secret") || b.causeCounts()["transport"] != int64(flowArchiveAttempts) {
		t.Fatalf("closed server %v %v", err, b.causeCounts())
	}
}
