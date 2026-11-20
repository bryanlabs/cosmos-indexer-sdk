package taxapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	querytypes "github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/shopspring/decimal"
)

// FlowArchiveReceipt retains the exact response, not merely an inferred rate.
// Endpoint credentials are never included in evidence or returned errors.
type FlowArchiveReceipt struct {
	Address     string
	Height      int64
	Method      string
	QueryPath   string
	RequestData string
	SHA256      string
	Response    string
	// Reused marks an exact-height header shared from this endpoint's cache.
	Reused bool `json:",omitempty"`
}

type flowArchive struct {
	rpc          *servicehttp.Endpoint
	mu           sync.Mutex
	receipts     []FlowArchiveReceipt
	holdings     map[string]flowHoldings
	headers      flowHeaderCache
	callAttempts atomic.Int64
	headerReuses atomic.Int64
	// pause waits between attempts of one read; tests replace it.
	pause func(time.Duration)
}

// flowArchiveRetryWaits are the waits before each retry of an archive read
// that got no usable HTTP response (a transport error, a non-200 status or a
// body that is not JSON): about 15 minutes in all. One such failure used to
// end a root's trace, and a giant root's trace runs for many hours. Every
// request is pinned to a height, so a retry asks for exactly the same state,
// and only a response that arrived is kept as a receipt. An answer that
// arrived but reports an error is not retried.
var flowArchiveRetryWaits = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute, 3 * time.Minute}

func newFlowArchive(origin string) (*flowArchive, error) {
	rpc, err := servicehttp.New(&http.Client{Timeout: 30 * time.Second}, origin)
	if err != nil {
		return nil, fmt.Errorf("invalid commission archive configuration")
	}
	return &flowArchive{rpc: rpc, holdings: map[string]flowHoldings{}, pause: time.Sleep}, nil
}

// getJSON is one archive read, retried over flowArchiveRetryWaits while no
// usable response arrives. Each try counts as a call attempt.
func (a *flowArchive) getJSON(method string, q url.Values, out *json.RawMessage) error {
	var err error
	for try := 0; ; try++ {
		a.callAttempts.Add(1)
		*out = nil
		if err = a.rpc.GetJSON(method, q, out); err == nil || try == len(flowArchiveRetryWaits) {
			return err
		}
		a.pause(flowArchiveRetryWaits[try])
	}
}

func (a *flowArchive) response(addr string, height int64, method string, q url.Values, out any, local ...*[]FlowArchiveReceipt) error {
	var raw json.RawMessage
	if err := a.getJSON(method, q, &raw); err != nil {
		return fmt.Errorf("commission archive %s request failed", method)
	}
	a.retainResponse(addr, height, method, q, string(raw), false, local...)
	var envelope struct{ Error json.RawMessage }
	if err := json.Unmarshal(raw, &envelope); err != nil || (len(envelope.Error) > 0 && string(envelope.Error) != "null") {
		return fmt.Errorf("invalid commission archive %s envelope", method)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid commission archive %s response", method)
	}
	return nil
}

func (a *flowArchive) retainResponse(addr string, height int64, method string, q url.Values, raw string, reused bool, local ...*[]FlowArchiveReceipt) {
	hash := sha256.Sum256([]byte(raw))
	receipt := FlowArchiveReceipt{Address: addr, Height: height, Method: method, QueryPath: q.Get("path"), RequestData: q.Get("data"), SHA256: hex.EncodeToString(hash[:]), Response: raw, Reused: reused}
	a.mu.Lock()
	a.receipts = append(a.receipts, receipt)
	a.mu.Unlock()
	for _, dst := range local {
		*dst = append(*dst, receipt)
	}
}

func (a *flowArchive) header(addr string, height int64, local ...*[]FlowArchiveReceipt) (time.Time, error) {
	q := url.Values{"height": {strconv.FormatInt(height, 10)}}
	raw, reused, err := a.headers.get(height, func() (string, error) {
		var body json.RawMessage
		if err := a.getJSON("block", q, &body); err != nil {
			return "", fmt.Errorf("commission archive block request failed")
		}
		_, err := flowArchiveHeader(string(body), height)
		return string(body), err
	})
	if reused {
		a.headerReuses.Add(1)
	}
	// Failed responses and cache hits both retain the exact bytes in every
	// requesting wallet's evidence, not just the first caller's receipts.
	if raw != "" {
		a.retainResponse(addr, height, "block", q, raw, reused, local...)
	}
	if err != nil {
		return time.Time{}, err
	}
	return flowArchiveHeader(raw, height)
}

func flowArchiveHeader(raw string, height int64) (time.Time, error) {
	var body struct {
		Error  json.RawMessage
		Result struct {
			Block struct {
				Header struct {
					ChainID string    `json:"chain_id"`
					Height  string    `json:"height"`
					Time    time.Time `json:"time"`
				} `json:"header"`
			} `json:"block"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || (len(body.Error) > 0 && string(body.Error) != "null") {
		return time.Time{}, fmt.Errorf("invalid commission archive block envelope")
	}
	h := body.Result.Block.Header
	if h.ChainID != "cosmoshub-4" || h.Height != strconv.FormatInt(height, 10) || h.Time.IsZero() {
		return time.Time{}, fmt.Errorf("commission archive chain or header height mismatch")
	}
	return h.Time, nil
}

type flowProtoRequest interface{ Marshal() ([]byte, error) }
type flowProtoResponse interface{ Unmarshal([]byte) error }

func (a *flowArchive) query(addr string, height int64, path string, request flowProtoRequest, response flowProtoResponse, local ...*[]FlowArchiveReceipt) error {
	data, err := request.Marshal()
	if err != nil {
		return err
	}
	var body struct {
		Result struct {
			Response struct {
				Code   uint32 `json:"code"`
				Height string `json:"height"`
				Value  string `json:"value"`
			} `json:"response"`
		} `json:"result"`
	}
	q := url.Values{"path": {strconv.Quote(path)}, "height": {strconv.FormatInt(height, 10)}, "data": {"0x" + hex.EncodeToString(data)}}
	if err := a.response(addr, height, "abci_query", q, &body, local...); err != nil {
		return err
	}
	r := body.Result.Response
	if r.Code != 0 || r.Height != strconv.FormatInt(height, 10) {
		return fmt.Errorf("commission archive state unavailable at requested height")
	}
	value, err := base64.StdEncoding.DecodeString(r.Value)
	if err != nil || len(value) == 0 {
		return fmt.Errorf("invalid or missing commission archive state encoding")
	}
	if err := response.Unmarshal(value); err != nil {
		return fmt.Errorf("invalid commission archive state payload")
	}
	return nil
}

func (a *flowArchive) cachedHoldings(addr string, day time.Time) (flowHoldings, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.holdings[addr+"|"+flowDay(day).Format(time.DateOnly)]
	return h, ok
}

func (a *flowArchive) holdingSource(addr string, day time.Time) CommissionFlowHoldingSource {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holdings[addr+"|"+flowDay(day).Format(time.DateOnly)].Source
}

func (a *flowArchive) holdingsAt(s *Server, chain, addr string, day time.Time) (flowHoldings, error) {
	if chain != apiMainnet {
		return flowHoldings{}, fmt.Errorf("commission archive supports only the verified Hub chain")
	}
	if h, ok := a.cachedHoldings(addr, day); ok {
		return h, nil
	}
	h := flowHoldings{Liquid: decimal.Zero, Staked: map[string]decimal.Decimal{}, Unbonding: map[string]decimal.Decimal{}}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return h, err
	}
	last, err := s.dayEndBlock(ids, day)
	if err != nil {
		return h, err
	}
	end := flowDay(day).AddDate(0, 0, 1)
	var localReceipts []FlowArchiveReceipt
	at, err := a.header(addr, last.Height, &localReceipts)
	if err != nil {
		return h, err
	}
	next, err := a.header(addr, last.Height+1, &localReceipts)
	if err != nil {
		return h, err
	}
	// PostgreSQL timestamps retain microseconds, while block headers retain
	// nanoseconds. Compare at the database's precision, not by rounding heights.
	if !flowArchiveDayBoundary(at, next, last.TimeStamp, end) {
		return h, fmt.Errorf("commission archive day boundary differs from indexed boundary")
	}
	// Each endpoint pages at exactly the same application version. No rate
	// sample is extrapolated, and no current-state response can be accepted.
	var key []byte
	for page := 0; page < 1000; page++ {
		var response banktypes.QueryAllBalancesResponse
		if err := a.query(addr, last.Height, "/cosmos.bank.v1beta1.Query/AllBalances", &banktypes.QueryAllBalancesRequest{Address: addr, Pagination: &querytypes.PageRequest{Key: key, Limit: 1000}}, &response, &localReceipts); err != nil {
			return h, err
		}
		for _, coin := range response.Balances {
			if coin.Denom == s.native.Denom {
				h.Liquid = h.Liquid.Add(decimal.RequireFromString(coin.Amount.String()))
			}
		}
		key, err = flowArchivePageKey(response.Pagination)
		if err != nil {
			return h, err
		}
		if len(key) == 0 {
			break
		}
	}
	if len(key) > 0 {
		return h, fmt.Errorf("commission archive bank pagination did not finish")
	}
	for page := 0; page < 1000; page++ {
		var response stakingtypes.QueryDelegatorDelegationsResponse
		if err := a.query(addr, last.Height, "/cosmos.staking.v1beta1.Query/DelegatorDelegations", &stakingtypes.QueryDelegatorDelegationsRequest{DelegatorAddr: addr, Pagination: &querytypes.PageRequest{Key: key, Limit: 1000}}, &response, &localReceipts); err != nil {
			return h, err
		}
		for _, d := range response.DelegationResponses {
			if d.Balance.Denom != s.native.Denom {
				return h, fmt.Errorf("commission archive delegation denomination mismatch")
			}
			h.Staked[d.Delegation.ValidatorAddress] = h.Staked[d.Delegation.ValidatorAddress].Add(decimal.RequireFromString(d.Balance.Amount.String()))
		}
		key, err = flowArchivePageKey(response.Pagination)
		if err != nil {
			return h, err
		}
		if len(key) == 0 {
			break
		}
	}
	if len(key) > 0 {
		return h, fmt.Errorf("commission archive delegation pagination did not finish")
	}
	for page := 0; page < 1000; page++ {
		var response stakingtypes.QueryDelegatorUnbondingDelegationsResponse
		if err := a.query(addr, last.Height, "/cosmos.staking.v1beta1.Query/DelegatorUnbondingDelegations", &stakingtypes.QueryDelegatorUnbondingDelegationsRequest{DelegatorAddr: addr, Pagination: &querytypes.PageRequest{Key: key, Limit: 1000}}, &response, &localReceipts); err != nil {
			return h, err
		}
		for _, d := range response.UnbondingResponses {
			for _, entry := range d.Entries {
				h.Unbonding[d.ValidatorAddress] = h.Unbonding[d.ValidatorAddress].Add(decimal.RequireFromString(entry.Balance.String()))
			}
		}
		key, err = flowArchivePageKey(response.Pagination)
		if err != nil {
			return h, err
		}
		if len(key) == 0 {
			break
		}
	}
	if len(key) > 0 {
		return h, fmt.Errorf("commission archive unbonding pagination did not finish")
	}
	a.mu.Lock()
	keyName := addr + "|" + flowDay(day).Format(time.DateOnly)
	source := CommissionFlowHoldingSource{Kind: "archive_exact", Height: last.Height}
	seen := map[string]bool{}
	for _, receipt := range localReceipts {
		if receipt.Address == addr && (receipt.Height == last.Height || receipt.Height == last.Height+1) && !seen[receipt.SHA256] {
			source.ReceiptHashes = append(source.ReceiptHashes, receipt.SHA256)
			seen[receipt.SHA256] = true
		}
	}
	h.Source = source
	a.holdings[keyName] = h
	a.mu.Unlock()
	return h, nil
}

func flowArchivePageKey(page *querytypes.PageResponse) ([]byte, error) {
	if page == nil {
		return nil, fmt.Errorf("commission archive pagination missing")
	}
	return page.NextKey, nil
}

func flowArchiveDayBoundary(at, next, indexed, end time.Time) bool {
	return at.Sub(indexed).Abs() < time.Microsecond && at.Before(end) && !next.Before(end)
}

func (a *flowArchive) evidence(addresses map[string]bool) []FlowArchiveReceipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []FlowArchiveReceipt
	for _, r := range a.receipts {
		if addresses == nil || addresses[r.Address] {
			out = append(out, r)
		}
	}
	return out
}
