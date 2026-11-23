package taxapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/cosmos/cosmos-sdk/types/query"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/shopspring/decimal"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// The archive is read only for the two balance history reads the serving node
// cannot answer below the lowest height it holds: a wallet's delegations
// either side of a block, and a validator's tokens per share. Its origin is a
// credential: no error, log line or metric names it, and every error says
// only "archive".
const (
	// archiveCallLimit bounds the archive calls of one wallet's walk; past it
	// the dates stay refused as they would be without the archive.
	archiveCallLimit = 20
	// archiveTimeout bounds each call, and the wait for the rate limit. A
	// call is never retried.
	archiveTimeout = 10 * time.Second
	// archiveFloorEvery is how often the serving node's lowest height is read
	// again, and archiveFloorRetry how soon after a failed read.
	archiveFloorEvery = time.Hour
	archiveFloorRetry = time.Minute
)

// The results archive_calls_total counts.
const (
	archiveResultOK      = "ok"
	archiveResultFailed  = "failed"
	archiveResultLimited = "rate_limited"
	archiveResultCeiling = "over_ceiling"
	archiveResultAbsent  = "absent"
)

var archiveResults = []string{archiveResultOK, archiveResultFailed, archiveResultLimited, archiveResultCeiling, archiveResultAbsent}

var archiveCalls = func() map[string]*atomic.Int64 {
	m := map[string]*atomic.Int64{}
	for _, r := range archiveResults {
		m[r] = new(atomic.Int64)
	}
	return m
}()

var (
	errArchiveConfig  = errors.New("archive: invalid configuration")
	errArchiveCeiling = errors.New("archive: call limit for this wallet reached")
	errArchiveLimited = errors.New("archive: rate limited")
	errArchiveFailed  = errors.New("archive: read failed")
	errArchiveHeight  = errors.New("archive: answered at another height")
	errArchiveAnswer  = errors.New("archive: unexpected answer")
	// errArchiveLostDenom is the delegation query failing where the archive's
	// newer binary cannot price delegations at the height (lostBondDenom).
	errArchiveLostDenom = errors.New("archive: delegations cannot be priced at the height")
	// errArchiveAbsent is the archive answering, for the height asked, that
	// a validator does not exist there, while it serves that height's
	// validators.
	errArchiveAbsent = errors.New("archive: no such validator at the height")
)

// validatorQuery is the archive read of one validator's tokens and shares,
// and sdkKeyNotFound the code, in the "sdk" codespace, it answers with, as a
// gRPC NotFound, for a validator the height does not hold. A height the
// archive has no state for answers with another code.
const (
	validatorQuery = "/cosmos.staking.v1beta1.Query/Validator"
	sdkKeyNotFound = 22
	grpcNotFound   = "rpc error: code = NotFound"
)

// archiveAbsentZeros counts the delegations balance history showed as zero
// because the archive answered that their validator did not exist yet.
var archiveAbsentZeros atomic.Int64

// servingArchive is the archive this process reads, when StartLedgerArchive
// configured one.
var servingArchive atomic.Pointer[ledgerArchive]

type ledgerArchive struct {
	rpc    *servicehttp.Endpoint
	bucket *tokenBucket
	// floor is the lowest height the serving node holds; zero while unknown,
	// and the archive is then not read.
	floor atomic.Int64
	logf  func(string, ...any)
}

// archivePace bounds this process's archive calls: rate calls a second, and
// burst at once.
type archivePace struct {
	rate  float64
	burst int
}

// archivePaceFromEnv reads the pace from LEDGER_ARCHIVE_RPS and
// LEDGER_ARCHIVE_BURST (default 1). The archive is off unless the rate is set
// above zero; a value that does not read as a positive number also leaves it
// off, with an error that names the variable.
func archivePaceFromEnv(getenv func(string) string) (archivePace, error) {
	rps := getenv("LEDGER_ARCHIVE_RPS")
	if rps == "" {
		return archivePace{}, errors.New("archive: off, LEDGER_ARCHIVE_RPS is not set")
	}
	rate, err := strconv.ParseFloat(rps, 64)
	if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return archivePace{}, errors.New("archive: off, LEDGER_ARCHIVE_RPS is not a number")
	}
	if rate <= 0 {
		return archivePace{}, errors.New("archive: off, LEDGER_ARCHIVE_RPS is not above zero")
	}
	burst := 1
	if b := getenv("LEDGER_ARCHIVE_BURST"); b != "" {
		if burst, err = strconv.Atoi(b); err != nil || burst < 1 {
			return archivePace{}, errors.New("archive: off, LEDGER_ARCHIVE_BURST is not a whole number above zero")
		}
	}
	return archivePace{rate: rate, burst: burst}, nil
}

func newLedgerArchive(origin string, timeout time.Duration, pace archivePace) (*ledgerArchive, error) {
	rpc, err := servicehttp.New(&http.Client{Timeout: timeout}, origin)
	if err != nil {
		return nil, errArchiveConfig
	}
	return &ledgerArchive{rpc: rpc, bucket: newTokenBucket(pace.rate, pace.burst)}, nil
}

// StartLedgerArchive makes balance history read below the serving node's
// lowest height from the archive RPC at origin, at the pace getenv gives
// (archivePaceFromEnv); without one the archive stays off. It reads that
// height from the serving node's RPC now and every archiveFloorEvery after,
// and the archive is not read while it is unknown.
func StartLedgerArchive(origin string, getenv func(string) string, logf func(string, ...any)) error {
	pace, err := archivePaceFromEnv(getenv)
	if err != nil {
		return err
	}
	a, err := newLedgerArchive(origin, archiveTimeout, pace)
	if err != nil {
		return err
	}
	a.logf = logf
	node, err := newLedgerNode()
	if err != nil {
		return err
	}
	refresh := func() time.Duration {
		h, err := node.earliestHeight()
		if err != nil {
			if a.floor.Swap(0) != 0 {
				logf("balance history archive: the serving node's lowest height is unknown, archive off until it is read")
			}
			return archiveFloorRetry
		}
		a.floor.Store(h)
		return archiveFloorEvery
	}
	next := refresh()
	if next == archiveFloorRetry {
		logf("balance history archive: the serving node's lowest height is unknown, archive off until it is read")
	}
	servingArchive.Store(a)
	go func() {
		for {
			time.Sleep(next)
			next = refresh()
		}
	}()
	return nil
}

// earliestHeight is the lowest height the node's RPC holds.
func (n *ledgerNode) earliestHeight() (int64, error) {
	var body struct {
		Result struct {
			SyncInfo struct {
				EarliestBlockHeight string `json:"earliest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := n.rpc.GetJSON("/status", nil, &body); err != nil {
		return 0, err
	}
	h, err := strconv.ParseInt(body.Result.SyncInfo.EarliestBlockHeight, 10, 64)
	if err != nil || h < 1 {
		return 0, errors.New("the node's status names no lowest height")
	}
	return h, nil
}

// below says whether height is under the lowest height the serving node
// holds, so that only the archive can answer it.
func (a *ledgerArchive) below(height int64) bool {
	floor := a.floor.Load()
	return floor > 0 && height < floor
}

// archiveBudget is what is left of one walk's archive calls.
type archiveBudget struct{ left int }

func newArchiveBudget() *archiveBudget { return &archiveBudget{left: archiveCallLimit} }

func countArchive(result string) { archiveCalls[result].Add(1) }

// abci reads one abci_query at height. Its errors are the fixed archive
// errors above, never the transport's, which name the origin.
func (a *ledgerArchive) abci(b *archiveBudget, path string, data []byte, height int64) ([]byte, error) {
	if b == nil || b.left <= 0 {
		countArchive(archiveResultCeiling)
		return nil, errArchiveCeiling
	}
	if !a.bucket.take(archiveTimeout) {
		countArchive(archiveResultLimited)
		return nil, errArchiveLimited
	}
	b.left--
	var body struct {
		Result struct {
			Response struct {
				Code      uint32 `json:"code"`
				Codespace string `json:"codespace"`
				Log       string `json:"log"`
				Value     string `json:"value"`
				Height    string `json:"height"`
			} `json:"response"`
		} `json:"result"`
	}
	q := url.Values{"path": {strconv.Quote(path)}, "data": {"0x" + hex.EncodeToString(data)}, rpcHeightParam: {strconv.FormatInt(height, 10)}}
	if err := a.rpc.GetJSON("/abci_query", q, &body); err != nil {
		countArchive(archiveResultFailed)
		return nil, errArchiveFailed
	}
	r := body.Result.Response
	if r.Code == sdkKeyNotFound && r.Codespace == "sdk" && strings.HasPrefix(r.Log, grpcNotFound) && path == validatorQuery && r.Height == strconv.FormatInt(height, 10) {
		countArchive(archiveResultAbsent)
		return nil, errArchiveAbsent
	}
	if r.Code != 0 {
		countArchive(archiveResultFailed)
		if strings.Contains(r.Log, "invalid denom") {
			return nil, errArchiveLostDenom
		}
		return nil, errArchiveFailed
	}
	// Some archives answer from their latest state whatever height is asked.
	if r.Height != strconv.FormatInt(height, 10) {
		countArchive(archiveResultFailed)
		return nil, errArchiveHeight
	}
	value, err := base64.StdEncoding.DecodeString(r.Value)
	if err != nil {
		countArchive(archiveResultFailed)
		return nil, errArchiveAnswer
	}
	countArchive(archiveResultOK)
	return value, nil
}

type archiveRequest interface{ Marshal() ([]byte, error) }
type archiveResponse interface{ Unmarshal([]byte) error }

func (a *ledgerArchive) query(b *archiveBudget, path string, req archiveRequest, resp archiveResponse, height int64) error {
	data, err := req.Marshal()
	if err != nil {
		return err
	}
	bz, err := a.abci(b, path, data, height)
	if err != nil {
		return err
	}
	if err := resp.Unmarshal(bz); err != nil {
		return errArchiveAnswer
	}
	return nil
}

// delegations reads addr's delegations at height, priced as the delegation
// query prices them. Where the archive cannot price them at the height, each
// delegation record is read from the staking store, as storeDelegations does.
func (a *ledgerArchive) delegations(b *archiveBudget, addr string, height int64) (map[string]*big.Int, error) {
	out := map[string]*big.Int{}
	var next []byte
	for {
		var r stakingtypes.QueryDelegatorDelegationsResponse
		err := a.query(b, "/cosmos.staking.v1beta1.Query/DelegatorDelegations", &stakingtypes.QueryDelegatorDelegationsRequest{DelegatorAddr: addr, Pagination: &query.PageRequest{Key: next, Limit: 1000}}, &r, height)
		if errors.Is(err, errArchiveLostDenom) && next == nil {
			return a.storeDelegations(b, addr, height)
		}
		if err != nil {
			return nil, err
		}
		for _, d := range r.DelegationResponses {
			if d.Delegation.DelegatorAddress != addr || out[d.Delegation.ValidatorAddress] != nil {
				return nil, errArchiveAnswer
			}
			out[d.Delegation.ValidatorAddress] = d.Balance.Amount.BigInt()
		}
		if r.Pagination == nil || len(r.Pagination.NextKey) == 0 {
			return out, nil
		}
		next = r.Pagination.NextKey
	}
}

func (a *ledgerArchive) storeDelegations(b *archiveBudget, addr string, height int64) (map[string]*big.Int, error) {
	_, del, err := bech32.DecodeAndConvert(addr)
	if err != nil {
		return nil, err
	}
	out := map[string]*big.Int{}
	var next []byte
	for {
		var r stakingtypes.QueryDelegatorValidatorsResponse
		if err := a.query(b, "/cosmos.staking.v1beta1.Query/DelegatorValidators", &stakingtypes.QueryDelegatorValidatorsRequest{DelegatorAddr: addr, Pagination: &query.PageRequest{Key: next, Limit: 1000}}, &r, height); err != nil {
			return nil, err
		}
		for _, v := range r.Validators {
			if out[v.OperatorAddress] != nil {
				return nil, errArchiveAnswer
			}
			_, val, err := bech32.DecodeAndConvert(v.OperatorAddress)
			if err != nil {
				return nil, err
			}
			if len(del) > 255 || len(val) > 255 {
				return nil, errors.New("an address is too long for a store key")
			}
			key := append([]byte{delegationKeyPrefix, byte(len(del))}, del...) //nolint:gosec // length checked above
			key = append(append(key, byte(len(val))), val...)                  //nolint:gosec // length checked above
			bz, err := a.abci(b, "/store/staking/key", key, height)
			if err != nil {
				return nil, err
			}
			var d stakingtypes.Delegation
			if len(bz) == 0 || d.Unmarshal(bz) != nil || d.DelegatorAddress != addr || d.ValidatorAddress != v.OperatorAddress {
				return nil, errArchiveAnswer
			}
			out[v.OperatorAddress] = v.TokensFromShares(d.Shares).TruncateInt().BigInt()
		}
		if r.Pagination == nil || len(r.Pagination.NextKey) == 0 {
			return out, nil
		}
		next = r.Pagination.NextKey
	}
}

// rate reads validator v's tokens and delegator shares at height.
func (a *ledgerArchive) rate(b *archiveBudget, v string, height int64) (validatorRate, error) {
	var r stakingtypes.QueryValidatorResponse
	if err := a.query(b, validatorQuery, &stakingtypes.QueryValidatorRequest{ValidatorAddr: v}, &r, height); err != nil {
		if errors.Is(err, errArchiveAbsent) && !a.servesValidators(b, height) {
			return validatorRate{}, errArchiveFailed
		}
		return validatorRate{}, err
	}
	if r.Validator.OperatorAddress != v || r.Validator.Tokens.IsNil() || r.Validator.DelegatorShares.IsNil() {
		return validatorRate{}, errArchiveAnswer
	}
	shares, err := decimal.NewFromString(r.Validator.DelegatorShares.String())
	if err != nil {
		return validatorRate{}, errArchiveAnswer
	}
	return validatorRate{Tokens: r.Validator.Tokens.BigInt(), Shares: shares}, nil
}

// servesValidators says whether the archive holds the staking state at
// height: it answers for that height with a validator of the set.
func (a *ledgerArchive) servesValidators(b *archiveBudget, height int64) bool {
	var r stakingtypes.QueryValidatorsResponse
	if err := a.query(b, "/cosmos.staking.v1beta1.Query/Validators", &stakingtypes.QueryValidatorsRequest{Pagination: &query.PageRequest{Limit: 1}}, &r, height); err != nil {
		return false
	}
	return len(r.Validators) == 1 && r.Validators[0].OperatorAddress != ""
}

// delegationsAt reads addr's delegations at height from the node, or from
// the archive below the lowest height the node holds, spending b.
func (n *ledgerNode) delegationsAt(addr string, height int64, b *archiveBudget) (map[string]*big.Int, error) {
	if a := n.archive; a != nil && a.below(height) {
		return a.delegations(b, addr, height)
	}
	d, _, err := n.delegations(addr, height)
	return d, err
}

// rateAtHeight reads validator v's rate at height from the node, or from the
// archive below the lowest height the node holds, spending b.
func (n *ledgerNode) rateAtHeight(v string, height int64, b *archiveBudget) (validatorRate, error) {
	if a := n.archive; a != nil && a.below(height) {
		return a.rate(b, v, height)
	}
	return n.rateAt(v, height)
}

// tokenBucket admits calls at rate a second, with up to burst at once.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
	sleep  func(time.Duration)
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), now: time.Now, sleep: time.Sleep}
}

// take waits for a token. It takes nothing and reports false when none would
// be free within wait.
func (t *tokenBucket) take(wait time.Duration) bool {
	t.mu.Lock()
	now := t.now()
	if !t.last.IsZero() {
		t.tokens = min(t.burst, t.tokens+now.Sub(t.last).Seconds()*t.rate)
	}
	t.last = now
	need := time.Duration(0)
	if t.tokens < 1 {
		need = time.Duration((1 - t.tokens) / t.rate * float64(time.Second))
	}
	if need > wait {
		t.mu.Unlock()
		return false
	}
	t.tokens--
	t.mu.Unlock()
	if need > 0 {
		t.sleep(need)
	}
	return true
}

// writeArchiveMetrics writes archive_calls_total in the Prometheus text format.
func writeArchiveMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP archive_calls_total Balance history calls to the archive node, by result.")
	fmt.Fprintln(w, "# TYPE archive_calls_total counter")
	for _, r := range archiveResults {
		fmt.Fprintf(w, "archive_calls_total{result=%q} %d\n", r, archiveCalls[r].Load())
	}
	fmt.Fprintln(w, "# HELP archive_absent_zero_total Delegations shown as zero because the archive answered that their validator did not exist yet.")
	fmt.Fprintln(w, "# TYPE archive_absent_zero_total counter")
	fmt.Fprintf(w, "archive_absent_zero_total %d\n", archiveAbsentZeros.Load())
}

// noteAbsent records a delegation on v at height the archive found no
// validator for: shown as zero when accepted, refused otherwise. The
// validator is named by a short hash, as no log line names a wallet's
// holdings.
func (a *ledgerArchive) noteAbsent(v string, height int64, sp shareSpan, accepted bool) {
	if accepted {
		archiveAbsentZeros.Add(1)
	}
	if a.logf == nil {
		return
	}
	vh := sha256.Sum256([]byte(v))
	outcome := "refused, the walk holds shares there"
	if accepted {
		outcome = "shown as zero"
	}
	a.logf("balance history archive: validator %x absent at height %d, shares %s..%s, %s", vh[:4], height, sp.lo, sp.hi, outcome)
}
