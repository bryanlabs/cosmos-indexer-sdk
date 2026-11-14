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
}

type flowArchive struct {
	rpc      *servicehttp.Endpoint
	mu       sync.Mutex
	receipts []FlowArchiveReceipt
	holdings map[string]flowHoldings
}

func newFlowArchive(origin string) (*flowArchive, error) {
	rpc, err := servicehttp.New(&http.Client{Timeout: 30 * time.Second}, origin)
	if err != nil {
		return nil, fmt.Errorf("invalid commission archive configuration")
	}
	return &flowArchive{rpc: rpc, holdings: map[string]flowHoldings{}}, nil
}

func (a *flowArchive) response(addr string, height int64, method string, q url.Values, out any, local ...*[]FlowArchiveReceipt) error {
	var raw json.RawMessage
	if err := a.rpc.GetJSON(method, q, &raw); err != nil {
		return fmt.Errorf("commission archive %s request failed", method)
	}
	hash := sha256.Sum256(raw)
	receipt := FlowArchiveReceipt{Address: addr, Height: height, Method: method, QueryPath: q.Get("path"), RequestData: q.Get("data"), SHA256: hex.EncodeToString(hash[:]), Response: string(raw)}
	a.mu.Lock()
	a.receipts = append(a.receipts, receipt)
	a.mu.Unlock()
	for _, dst := range local {
		*dst = append(*dst, receipt)
	}
	var envelope struct{ Error json.RawMessage }
	if err := json.Unmarshal(raw, &envelope); err != nil || (len(envelope.Error) > 0 && string(envelope.Error) != "null") {
		return fmt.Errorf("invalid commission archive %s envelope", method)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid commission archive %s response", method)
	}
	return nil
}

func (a *flowArchive) header(addr string, height int64, local ...*[]FlowArchiveReceipt) (time.Time, error) {
	var body struct {
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
	if err := a.response(addr, height, "block", url.Values{"height": {strconv.FormatInt(height, 10)}}, &body, local...); err != nil {
		return time.Time{}, err
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
