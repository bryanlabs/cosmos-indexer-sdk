// Package servicehttp binds JSON reads to an operator-configured service origin.
package servicehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Endpoint separates trusted configuration from request-controlled resource
// paths and query values. Private cluster origins are intentional.
type Endpoint struct {
	base   url.URL
	client http.Client
	retry  *Retry
}

// Retry keeps a read going through a short outage of the service: a refused
// or reset connection, or a 5xx answer. Only a failure before any of the
// answer is decoded is retried, so a caller never sees a value built from two
// answers. Every retry is logged. Once For has passed since the read's first
// failure it returns the last error, with how long it kept failing.
type Retry struct {
	For   time.Duration
	Max   time.Duration
	Logf  func(string, ...any)
	sleep func(time.Duration)
	now   func() time.Time
}

// WithRetry returns a copy of the endpoint whose reads follow r.
func (e *Endpoint) WithRetry(r Retry) *Endpoint {
	if r.Max <= 0 {
		r.Max = 30 * time.Second
	}
	if r.sleep == nil {
		r.sleep = time.Sleep
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.Logf == nil {
		r.Logf = func(string, ...any) {}
	}
	c := *e
	c.retry = &r
	return &c
}

// Transient reports whether err is a refused, reset or dropped connection.
// Timeouts and every other error are not.
func Transient(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

var bech32Like = regexp.MustCompile(`[a-z]+1[02-9ac-hj-np-z]{20,}`)

func failureClass(err error, status int) string {
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

func New(client *http.Client, origin string) (*Endpoint, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("invalid service origin: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return nil, fmt.Errorf("service origin must be an HTTP(S) URL without credentials, query or fragment")
	}
	bounded := *client
	bounded.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Endpoint{base: *base, client: bounded}, nil
}

// GetJSON cannot change the origin, including through // paths, query values,
// path traversal or redirects. Path input is never resolved as another URL.
func (e *Endpoint) GetJSON(path string, query url.Values, out any) error {
	return e.GetJSONWithHeaders(path, query, nil, out)
}

// GetJSONWithHeaders is GetJSON with request headers, such as the block height
// a Cosmos REST query is answered at.
func (e *Endpoint) GetJSONWithHeaders(path string, query url.Values, headers map[string]string, out any) error {
	target := e.base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	target.RawPath = ""
	target.RawQuery = query.Encode()
	var first time.Time
	wait := time.Second
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target.String(), nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		status := 0
		if err == nil {
			if resp.StatusCode == http.StatusOK || e.retry == nil || resp.StatusCode < 500 {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("service HTTP status %d", resp.StatusCode)
				}
				return json.NewDecoder(resp.Body).Decode(out)
			}
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			err = fmt.Errorf("service HTTP status %d", status)
		} else if e.retry == nil || !Transient(err) {
			return err
		}
		now := e.retry.now()
		if first.IsZero() {
			first = now
		}
		failing := now.Sub(first)
		if failing >= e.retry.For {
			e.retry.Logf("service read %s: %s, giving up after %d attempts over %s", bech32Like.ReplaceAllString(path, "<address>"), failureClass(err, status), attempt, failing.Round(time.Second))
			return fmt.Errorf("%w (failed %d times over %s)", err, attempt, failing.Round(time.Second))
		}
		wait = min(wait, e.retry.For-failing+time.Millisecond)
		e.retry.Logf("service read %s: %s, attempt %d, retrying in %s (gives up %s after the first failure)", bech32Like.ReplaceAllString(path, "<address>"), failureClass(err, status), attempt, wait.Round(time.Millisecond), e.retry.For)
		e.retry.sleep(wait)
		wait = min(2*wait, e.retry.Max)
	}
}
