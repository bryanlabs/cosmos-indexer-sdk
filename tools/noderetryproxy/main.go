// Command noderetryproxy sits on localhost in a collector pod, in front of our
// own node's REST and RPC ports, so a read that reaches a node while it
// restarts is tried again instead of ending the run. It forwards GET requests
// unchanged to one configured origin per port and retries only a refused,
// reset or dropped connection, or a 5xx answer, before any of the answer is
// passed on. It never reads another source. Each retry and each give-up is
// logged, and a give-up answers 502, which the collector treats as a failure.
//
// A retry window is bounded by the collector's own per-read client timeout:
// a read the proxy holds longer than that times out in the collector anyway.
//
//	noderetryproxy -route 127.0.0.1:1317=http://node:1317 -route 127.0.0.1:26657=http://node:26657 -retry-for 20s
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type routes []string

func (r *routes) String() string     { return strings.Join(*r, ",") }
func (r *routes) Set(v string) error { *r = append(*r, v); return nil }

var bech32Like = regexp.MustCompile(`[a-z]+1[02-9ac-hj-np-z]{20,}`)

func transient(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func class(err error, status int) string {
	switch {
	case status != 0:
		return fmt.Sprintf("HTTP %d", status)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE):
		return "connection reset"
	default:
		return "connection closed"
	}
}

// hop-by-hop headers are not forwarded (RFC 9110 7.6.1).
var hop = map[string]bool{"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}

type proxy struct {
	origin   *url.URL
	client   *http.Client
	retryFor time.Duration
	maxWait  time.Duration
	logf     func(string, ...any)
	sleep    func(time.Duration)
	now      func() time.Time
	retries  atomic.Int64
	giveUps  atomic.Int64
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "noderetryproxy forwards GET only", http.StatusMethodNotAllowed)
		return
	}
	target := *p.origin
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	var first time.Time
	wait := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
		if err != nil {
			http.Error(w, "noderetryproxy: bad request", http.StatusBadRequest)
			return
		}
		for k, v := range r.Header {
			if !hop[k] {
				req.Header[k] = v
			}
		}
		resp, err := p.client.Do(req)
		status := 0
		if err == nil && resp.StatusCode < 500 {
			defer resp.Body.Close()
			for k, v := range resp.Header {
				if !hop[k] {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		if err == nil {
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		} else if r.Context().Err() != nil || !transient(err) {
			http.Error(w, "noderetryproxy: "+class(err, 0)+" (not retried)", http.StatusBadGateway)
			p.logf("read %s: not retried: %s", bech32Like.ReplaceAllString(r.URL.Path, "<address>"), strings.ReplaceAll(err.Error(), target.String(), "<origin>"+bech32Like.ReplaceAllString(r.URL.Path, "<address>")))
			return
		}
		now := p.now()
		if first.IsZero() {
			first = now
		}
		failing := now.Sub(first)
		path := bech32Like.ReplaceAllString(r.URL.Path, "<address>")
		if failing >= p.retryFor {
			p.giveUps.Add(1)
			p.logf("read %s: %s, giving up after %d attempts over %s", path, class(err, status), attempt, failing.Round(time.Millisecond))
			http.Error(w, fmt.Sprintf("noderetryproxy: %s after %d attempts over %s", class(err, status), attempt, failing.Round(time.Millisecond)), http.StatusBadGateway)
			return
		}
		wait = min(wait, p.retryFor-failing+time.Millisecond)
		p.retries.Add(1)
		p.logf("read %s: %s, attempt %d, retrying in %s (gives up %s after the first failure)", path, class(err, status), attempt, wait.Round(time.Millisecond), p.retryFor)
		p.sleep(wait)
		wait = min(2*wait, p.maxWait)
	}
}

func newProxy(origin string, retryFor, maxWait time.Duration, logf func(string, ...any)) (*proxy, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, fmt.Errorf("origin must be an HTTP(S) URL without credentials or query")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	tr.Proxy = nil
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &proxy{origin: u, client: client, retryFor: retryFor, maxWait: maxWait, logf: logf, sleep: time.Sleep, now: time.Now}, nil
}

func main() {
	var rs routes
	flag.Var(&rs, "route", "listen=origin, repeated; listen must be a loopback address")
	retryFor := flag.Duration("retry-for", 20*time.Second, "how long one read keeps retrying after its first failure")
	maxWait := flag.Duration("max-wait", 2*time.Second, "longest wait between two attempts")
	flag.Parse()
	logger := log.New(os.Stdout, "noderetryproxy ", log.LstdFlags|log.LUTC)
	if len(rs) == 0 || *retryFor <= 0 {
		logger.Fatal("need at least one -route and a positive -retry-for")
	}
	errc := make(chan error, len(rs))
	for _, r := range rs {
		listen, origin, ok := strings.Cut(r, "=")
		host, _, err := net.SplitHostPort(listen)
		if !ok || err != nil || !net.ParseIP(host).IsLoopback() {
			logger.Fatalf("route %q: listen must be a loopback host:port", listen)
		}
		p, err := newProxy(origin, *retryFor, *maxWait, logger.Printf)
		if err != nil {
			logger.Fatalf("route on %s: %v", listen, err)
		}
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			logger.Fatal(err)
		}
		logger.Printf("listening on %s, retry window %s, longest wait %s", listen, *retryFor, *maxWait)
		srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- srv.Serve(ln) }()
	}
	logger.Fatal(<-errc)
}
