package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// node reads our own fullnode's CometBFT RPC.
type node struct {
	base   string
	client *http.Client
	tries  int

	mu     sync.Mutex
	times  map[int64]time.Time
	params map[int64]*big.Int
}

func newNode(base string, workers int) *node {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        workers + 4,
		MaxIdleConnsPerHost: workers + 4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &node{base: strings.TrimRight(base, "/"), client: &http.Client{Transport: tr, Timeout: 60 * time.Second}, tries: 6,
		times: map[int64]time.Time{}, params: map[int64]*big.Int{}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc %d %s %s", e.Code, e.Message, trim(e.Data))
}

// unavailable is the node's answer for a height it does not hold.
func (e *rpcError) unavailable() bool {
	return strings.Contains(e.Data, "is not available") || strings.Contains(e.Data, "lowest height") ||
		strings.Contains(e.Data, "must be less than or equal to the current blockchain height")
}

// get returns the raw JSON-RPC body. An answer the node gives for a height
// it does not hold is errMissing; transport errors and 5xx are retried.
func (n *node) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := n.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var last error
	for attempt := 0; attempt < n.tries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := n.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		var env struct {
			Error *rpcError `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil {
			if env.Error.unavailable() {
				return nil, errMissing
			}
			last = env.Error
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		return body, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("%s: %v", path, last)
}

func (n *node) blockResults(ctx context.Context, h int64) ([]byte, error) {
	return n.get(ctx, "/block_results", url.Values{"height": {strconv.FormatInt(h, 10)}})
}

type nodeStatus struct {
	Network        string
	EarliestHeight int64
	LatestHeight   int64
	LatestTime     time.Time
	CatchingUp     bool
}

func (n *node) status(ctx context.Context) (nodeStatus, error) {
	body, err := n.get(ctx, "/status", nil)
	if err != nil {
		return nodeStatus{}, err
	}
	var r struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
			SyncInfo struct {
				Earliest   string    `json:"earliest_block_height"`
				Latest     string    `json:"latest_block_height"`
				LatestTime time.Time `json:"latest_block_time"`
				CatchingUp bool      `json:"catching_up"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nodeStatus{}, fmt.Errorf("status: %v", err)
	}
	e, err1 := strconv.ParseInt(r.Result.SyncInfo.Earliest, 10, 64)
	l, err2 := strconv.ParseInt(r.Result.SyncInfo.Latest, 10, 64)
	if err1 != nil || err2 != nil {
		return nodeStatus{}, errors.New("status: bad heights")
	}
	return nodeStatus{Network: r.Result.NodeInfo.Network, EarliestHeight: e, LatestHeight: l,
		LatestTime: r.Result.SyncInfo.LatestTime.UTC(), CatchingUp: r.Result.SyncInfo.CatchingUp}, nil
}

// blockTime is the header time of height h.
func (n *node) blockTime(ctx context.Context, h int64) (time.Time, error) {
	n.mu.Lock()
	t, ok := n.times[h]
	n.mu.Unlock()
	if ok {
		return t, nil
	}
	body, err := n.get(ctx, "/header", url.Values{"height": {strconv.FormatInt(h, 10)}})
	if err != nil {
		return time.Time{}, fmt.Errorf("header %d: %w", h, err)
	}
	var r struct {
		Result struct {
			Header struct {
				Height string    `json:"height"`
				Time   time.Time `json:"time"`
			} `json:"header"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return time.Time{}, fmt.Errorf("header %d: %v", h, err)
	}
	if r.Result.Header.Height != strconv.FormatInt(h, 10) || r.Result.Header.Time.IsZero() {
		return time.Time{}, fmt.Errorf("header %d: height mismatch", h)
	}
	t = r.Result.Header.Time.UTC()
	n.mu.Lock()
	n.times[h] = t
	n.mu.Unlock()
	return t, nil
}

// communityTax is the distribution community_tax in the state after block
// h (1e18-scaled), so it is the rate block h+1 splits its mint at.
func (n *node) communityTax(ctx context.Context, h int64) (*big.Int, error) {
	n.mu.Lock()
	v, ok := n.params[h]
	n.mu.Unlock()
	if ok {
		return v, nil
	}
	body, err := n.get(ctx, "/abci_query", url.Values{
		"path":   {`"/cosmos.distribution.v1beta1.Query/Params"`},
		"height": {strconv.FormatInt(h, 10)},
	})
	if err != nil {
		return nil, fmt.Errorf("distribution params at %d: %w", h, err)
	}
	var r struct {
		Result struct {
			Response struct {
				Code   int    `json:"code"`
				Log    string `json:"log"`
				Value  string `json:"value"`
				Height string `json:"height"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("distribution params at %d: %v", h, err)
	}
	resp := r.Result.Response
	if resp.Code != 0 {
		return nil, fmt.Errorf("distribution params at %d: code %d %s", h, resp.Code, trim(resp.Log))
	}
	if resp.Height != strconv.FormatInt(h, 10) {
		return nil, fmt.Errorf("distribution params at %d: node answered for height %q", h, resp.Height)
	}
	raw, err := base64.StdEncoding.DecodeString(resp.Value)
	if err != nil {
		return nil, fmt.Errorf("distribution params at %d: %v", h, err)
	}
	s, err := communityTaxFromParams(raw)
	if err != nil {
		return nil, fmt.Errorf("distribution params at %d: %v", h, err)
	}
	v, err = legacyDec(s)
	if err != nil {
		return nil, fmt.Errorf("distribution params at %d: %v", h, err)
	}
	if v.Cmp(fixOne) > 0 {
		return nil, fmt.Errorf("distribution params at %d: community tax %s over 1", h, fixString(v))
	}
	n.mu.Lock()
	n.params[h] = v
	n.mu.Unlock()
	return v, nil
}

// communityTaxFromParams reads QueryParamsResponse{params: Params{community_tax: 1}}.
func communityTaxFromParams(b []byte) (string, error) {
	params, ok, err := protoBytesField(b, 1)
	if err != nil || !ok {
		return "", fmt.Errorf("no params in response (%v)", err)
	}
	tax, ok, err := protoBytesField(params, 1)
	if err != nil || !ok {
		return "", fmt.Errorf("no community_tax in params (%v)", err)
	}
	return string(tax), nil
}

// protoBytesField returns the last length-delimited field num in a protobuf message.
func protoBytesField(b []byte, num uint64) ([]byte, bool, error) {
	var out []byte
	found := false
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return nil, false, errors.New("bad varint")
		}
		b = b[n:]
		field, wt := key>>3, key&7
		switch wt {
		case 0:
			_, n = uvarint(b)
			if n <= 0 {
				return nil, false, errors.New("bad varint")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, false, errors.New("short fixed64")
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, false, errors.New("bad length")
			}
			v := b[n : n+int(l)]
			b = b[n+int(l):]
			if field == num {
				out, found = v, true
			}
		case 5:
			if len(b) < 4 {
				return nil, false, errors.New("short fixed32")
			}
			b = b[4:]
		default:
			return nil, false, fmt.Errorf("wire type %d", wt)
		}
	}
	return out, found, nil
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

// firstHeightAtOrAfter finds the first height in [lo, hi] whose header time
// is at or after t, or hi+1 when none is. Block times only increase.
func (n *node) firstHeightAtOrAfter(ctx context.Context, t time.Time, lo, hi int64) (int64, error) {
	a, b := lo, hi+1
	for a < b {
		m := a + (b-a)/2
		tm, err := n.blockTime(ctx, m)
		if err != nil {
			return 0, err
		}
		if tm.Before(t) {
			a = m + 1
		} else {
			b = m
		}
	}
	return a, nil
}
