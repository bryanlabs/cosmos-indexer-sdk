// Command archiveshim answers the archive REST queries the tax ledger tools
// make (bank balances, staking and slashing state at a height) from archive
// RPC nodes through abci_query, for when an archive's REST API is down. It
// sends the same gRPC query the REST gateway would, at the height in the
// x-cosmos-block-height header, refuses an answer given at any other height,
// and writes the response the way the gateway does. Each upstream RPC origin
// gets its own rate limit; requests rotate across them.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/std"
	"github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/cosmos/gogoproto/proto"
)

const heightHeader = "x-cosmos-block-height"

// upstream is one archive RPC origin with its own request budget.
type upstream struct {
	origin string
	gap    time.Duration
	mu     sync.Mutex
	next   time.Time
	// restUntil is when an upstream that failed to answer is tried again
	// first. Until then it is used only when every upstream is resting.
	restUntil atomic.Int64
}

// restFor is how long an upstream that failed to answer is passed over.
const restFor = 30 * time.Second

// wait spaces calls to the upstream evenly at its rate.
func (u *upstream) wait(ctx context.Context) error {
	u.mu.Lock()
	now := time.Now()
	at := u.next
	if at.Before(now) {
		at = now
	}
	u.next = at.Add(u.gap)
	u.mu.Unlock()
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type shim struct {
	ups    []*upstream
	next   atomic.Uint64
	client *http.Client
	reg    codectypes.InterfaceRegistry
}

// route maps a REST path to the gRPC method and request the gateway sends.
type route struct {
	re     *regexp.Regexp
	method string
	req    func(m []string, page *query.PageRequest, q url.Values) proto.Message
	resp   func() proto.Message
}

var routes = []route{
	{regexp.MustCompile(`^/cosmos/bank/v1beta1/balances/([a-z0-9]+)$`), "/cosmos.bank.v1beta1.Query/AllBalances",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &banktypes.QueryAllBalancesRequest{Address: m[1], Pagination: p}
		}, func() proto.Message { return &banktypes.QueryAllBalancesResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/delegations/([a-z0-9]+)$`), "/cosmos.staking.v1beta1.Query/DelegatorDelegations",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &stakingtypes.QueryDelegatorDelegationsRequest{DelegatorAddr: m[1], Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryDelegatorDelegationsResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/delegators/([a-z0-9]+)/unbonding_delegations$`), "/cosmos.staking.v1beta1.Query/DelegatorUnbondingDelegations",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &stakingtypes.QueryDelegatorUnbondingDelegationsRequest{DelegatorAddr: m[1], Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryDelegatorUnbondingDelegationsResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/delegators/([a-z0-9]+)/validators$`), "/cosmos.staking.v1beta1.Query/DelegatorValidators",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &stakingtypes.QueryDelegatorValidatorsRequest{DelegatorAddr: m[1], Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryDelegatorValidatorsResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/validators/([a-z0-9]+)/delegations$`), "/cosmos.staking.v1beta1.Query/ValidatorDelegations",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &stakingtypes.QueryValidatorDelegationsRequest{ValidatorAddr: m[1], Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryValidatorDelegationsResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/validators/([a-z0-9]+)/unbonding_delegations$`), "/cosmos.staking.v1beta1.Query/ValidatorUnbondingDelegations",
		func(m []string, p *query.PageRequest, _ url.Values) proto.Message {
			return &stakingtypes.QueryValidatorUnbondingDelegationsRequest{ValidatorAddr: m[1], Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryValidatorUnbondingDelegationsResponse{} }},
	{regexp.MustCompile(`^/cosmos/staking/v1beta1/validators$`), "/cosmos.staking.v1beta1.Query/Validators",
		func(_ []string, p *query.PageRequest, q url.Values) proto.Message {
			return &stakingtypes.QueryValidatorsRequest{Status: q.Get("status"), Pagination: p}
		}, func() proto.Message { return &stakingtypes.QueryValidatorsResponse{} }},
	{regexp.MustCompile(`^/cosmos/slashing/v1beta1/params$`), "/cosmos.slashing.v1beta1.Query/Params",
		func(_ []string, _ *query.PageRequest, _ url.Values) proto.Message {
			return &slashingtypes.QueryParamsRequest{}
		}, func() proto.Message { return &slashingtypes.QueryParamsResponse{} }},
}

func pageRequest(q url.Values) (*query.PageRequest, error) {
	var p query.PageRequest
	set := false
	if v := q.Get("pagination.key"); v != "" {
		k, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			if k, err = base64.URLEncoding.DecodeString(v); err != nil {
				return nil, fmt.Errorf("pagination.key: %w", err)
			}
		}
		p.Key, set = k, true
	}
	for name, dst := range map[string]*uint64{"pagination.offset": &p.Offset, "pagination.limit": &p.Limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			*dst, set = n, true
		}
	}
	for name, dst := range map[string]*bool{"pagination.count_total": &p.CountTotal, "pagination.reverse": &p.Reverse} {
		if v := q.Get(name); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			*dst, set = b, true
		}
	}
	if !set {
		return nil, nil
	}
	return &p, nil
}

// errStatus is an answer the shim gives with its own HTTP status.
type errStatus struct {
	code int
	msg  string
}

func (e errStatus) Error() string { return e.msg }

func (s *shim) pick() *upstream {
	now := time.Now().UnixNano()
	first := int(s.next.Add(1) - 1)
	for i := 0; i < len(s.ups); i++ {
		if u := s.ups[(first+i)%len(s.ups)]; u.restUntil.Load() <= now {
			return u
		}
	}
	return s.ups[first%len(s.ups)]
}

// attempts is how many upstream calls one query may use when an upstream
// fails to answer (transport error, HTTP error, or no JSON-RPC result).
const attempts = 4

// rpc sends one JSON-RPC call, moving to the next upstream when one fails to
// answer. An answer the node gave (a query error) is never retried.
func (s *shim) rpc(ctx context.Context, method string, params any, out any) error {
	var err error
	for i := 0; i < attempts; i++ {
		u := s.pick()
		if err = s.call(ctx, u, method, params, out); err == nil {
			return nil
		}
		var es errStatus
		if !errors.As(err, &es) || es.code != http.StatusBadGateway || ctx.Err() != nil {
			return err
		}
		u.restUntil.Store(time.Now().Add(restFor).UnixNano())
		log.Printf("upstream %d failed %s (attempt %d): %v", s.index(u), method, i+1, err)
	}
	return err
}

func (s *shim) index(u *upstream) int {
	for i, v := range s.ups {
		if v == u {
			return i
		}
	}
	return -1
}

func (s *shim) call(ctx context.Context, u *upstream, method string, params any, out any) error {
	if err := u.wait(ctx); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.origin, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		// The origin carries a credential, so only the error kind is passed on.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return errStatus{http.StatusBadGateway, "upstream request failed: " + err.Error()}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return errStatus{http.StatusBadGateway, "upstream read failed"}
	}
	if res.StatusCode != http.StatusOK {
		return errStatus{http.StatusBadGateway, fmt.Sprintf("upstream HTTP status %d", res.StatusCode)}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return errStatus{http.StatusBadGateway, "upstream answer is not JSON-RPC"}
	}
	if env.Error != nil {
		return errStatus{http.StatusBadGateway, fmt.Sprintf("upstream error %d: %s %s", env.Error.Code, env.Error.Message, env.Error.Data)}
	}
	return json.Unmarshal(env.Result, out)
}

// abciQuery runs one gRPC query at height and returns the response bytes. It
// refuses an answer given at any other height than the one asked for.
func (s *shim) abciQuery(ctx context.Context, method string, req proto.Message, height int64) ([]byte, error) {
	data, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"path": method, "data": hex.EncodeToString(data), "prove": false}
	if height > 0 {
		params["height"] = strconv.FormatInt(height, 10)
	}
	var out struct {
		Response struct {
			Code   uint32 `json:"code"`
			Log    string `json:"log"`
			Value  string `json:"value"`
			Height string `json:"height"`
		} `json:"response"`
	}
	if err := s.rpc(ctx, "abci_query", params, &out); err != nil {
		return nil, err
	}
	r := out.Response
	if r.Code != 0 {
		return nil, errStatus{http.StatusInternalServerError, fmt.Sprintf("query code %d: %s", r.Code, r.Log)}
	}
	if height > 0 && r.Height != strconv.FormatInt(height, 10) {
		return nil, errStatus{http.StatusBadGateway, fmt.Sprintf("upstream answered at height %q, not %d", r.Height, height)}
	}
	return base64.StdEncoding.DecodeString(r.Value)
}

// rawMethods are the RPC methods the tool also calls directly (store key
// reads and block reads). The shim passes them to an upstream with the same
// pacing and failover, so the tool needs no second node.
var rawMethods = map[string]bool{"abci_query": true, "block": true, "block_results": true, "status": true}

// rawRPC passes an RPC-style GET /<method>?... to an upstream as JSON-RPC and
// returns its envelope. The caller checks the height it was answered at.
func (s *shim) rawRPC(ctx context.Context, method string, q url.Values) ([]byte, error) {
	params := map[string]any{}
	for k := range q {
		v := strings.Trim(q.Get(k), `"`)
		switch k {
		case "height":
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return nil, errStatus{http.StatusBadRequest, "bad height"}
			}
		case "data":
			v = strings.TrimPrefix(v, "0x")
		case "prove":
			params[k] = v == "true"
			continue
		}
		params[k] = v
	}
	if method == "abci_query" {
		params["prove"] = false
	}
	var res json.RawMessage
	if err := s.rpc(ctx, method, params, &res); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": -1, "result": res})
}

func (s *shim) latest(ctx context.Context) ([]byte, error) {
	var st struct {
		SyncInfo struct {
			Height string    `json:"latest_block_height"`
			Time   time.Time `json:"latest_block_time"`
		} `json:"sync_info"`
	}
	if err := s.rpc(ctx, "status", map[string]any{}, &st); err != nil {
		return nil, err
	}
	if _, err := strconv.ParseInt(st.SyncInfo.Height, 10, 64); err != nil {
		return nil, errStatus{http.StatusBadGateway, "upstream status has no height"}
	}
	return json.Marshal(map[string]any{"block": map[string]any{"header": map[string]any{
		"height": st.SyncInfo.Height, "time": st.SyncInfo.Time.UTC().Format(time.RFC3339Nano)}}})
}

func (s *shim) answer(r *http.Request) ([]byte, error) {
	if r.Method != http.MethodGet {
		return nil, errStatus{http.StatusMethodNotAllowed, "GET only"}
	}
	if r.URL.Path == "/cosmos/base/tendermint/v1beta1/blocks/latest" {
		return s.latest(r.Context())
	}
	if m := strings.TrimPrefix(r.URL.Path, "/"); rawMethods[m] {
		return s.rawRPC(r.Context(), m, r.URL.Query())
	}
	var height int64
	if v := r.Header.Get(heightHeader); v != "" {
		h, err := strconv.ParseInt(v, 10, 64)
		if err != nil || h < 0 {
			return nil, errStatus{http.StatusBadRequest, "bad " + heightHeader}
		}
		height = h
	}
	for _, rt := range routes {
		m := rt.re.FindStringSubmatch(r.URL.Path)
		if m == nil {
			continue
		}
		q := r.URL.Query()
		page, err := pageRequest(q)
		if err != nil {
			return nil, errStatus{http.StatusBadRequest, err.Error()}
		}
		bz, err := s.abciQuery(r.Context(), rt.method, rt.req(m, page, q), height)
		if err != nil {
			return nil, err
		}
		resp := rt.resp()
		if err := proto.Unmarshal(bz, resp); err != nil {
			return nil, errStatus{http.StatusBadGateway, "decode " + rt.method + ": " + err.Error()}
		}
		return codec.ProtoMarshalJSON(resp, s.reg)
	}
	return nil, errStatus{http.StatusNotImplemented, "path not served by the shim"}
}

func (s *shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	start := time.Now()
	body, err := s.answer(r)
	w.Header().Set("content-type", "application/json")
	if err != nil {
		code := http.StatusBadGateway
		var es errStatus
		if errors.As(err, &es) {
			code = es.code
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": err.Error()})
		log.Printf("%d %s height=%s %s %v", code, routeName(r.URL.Path), r.Header.Get(heightHeader), time.Since(start).Round(time.Millisecond), err)
		return
	}
	_, _ = w.Write(body)
}

// routeName names the query without the account it was asked for.
func routeName(p string) string {
	return regexp.MustCompile(`/(cosmos|cosmosvaloper)1[a-z0-9]+`).ReplaceAllString(p, "/<addr>")
}

func newShim(origins []string, perSecond []float64) (*shim, error) {
	if len(origins) == 0 || len(origins) != len(perSecond) {
		return nil, errors.New("each upstream needs a rate")
	}
	reg := codectypes.NewInterfaceRegistry()
	std.RegisterInterfaces(reg)
	stakingtypes.RegisterInterfaces(reg)
	banktypes.RegisterInterfaces(reg)
	s := &shim{client: &http.Client{Timeout: 90 * time.Second}, reg: reg}
	for i, o := range origins {
		s.ups = append(s.ups, &upstream{origin: strings.TrimRight(o, "/"), gap: time.Duration(float64(time.Second) / perSecond[i])})
	}
	return s, nil
}

// readOrigin reads an RPC origin from a file (a bare URL, or a TOML line
// rpc = "<url>"), or from an environment variable named env:<NAME>.
func readOrigin(path string) (string, error) {
	if name, ok := strings.CutPrefix(path, "env:"); ok {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("environment variable %s is empty", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	re := regexp.MustCompile(`^\s*rpc\s*=\s*"([^"]+)"`)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line, nil
		}
	}
	return "", fmt.Errorf("no RPC origin in %s", path)
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var upstreams listFlag
	listen := flag.String("listen", ":1317", "address to serve the REST queries on")
	flag.Var(&upstreams, "upstream", "RPC origin file (or env:<NAME>) and calls per second, as <source>=<rate>; repeat for more origins")
	flag.Parse()
	var origins []string
	var rates []float64
	for _, u := range upstreams {
		file, r, ok := strings.Cut(u, "=")
		if !ok {
			log.Fatalf("--upstream %q is not <file>=<rate>", file)
		}
		n, err := strconv.ParseFloat(r, 64)
		if err != nil || n <= 0 {
			log.Fatalf("--upstream rate %q is not a positive number", r)
		}
		o, err := readOrigin(file)
		if err != nil {
			log.Fatal(err)
		}
		origins = append(origins, o)
		rates = append(rates, n)
	}
	s, err := newShim(origins, rates)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving on %s with %d upstreams", *listen, len(origins))
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
