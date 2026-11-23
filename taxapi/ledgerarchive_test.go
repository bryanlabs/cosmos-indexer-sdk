package taxapi

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// archiveSecret is part of the test archive's path, as a credential is.
const archiveSecret = "k3y-0f-the-archive"

// fakeArchive answers abci_query as an archive RPC would, from fixed state by
// height, and counts the calls.
type fakeArchive struct {
	mu    sync.Mutex
	calls []string
	// delegations by height, validator to tokens.
	delegations map[int64]map[string]int64
	// lostDenom heights fail the delegation query as lostBondDenom does.
	lostDenom map[int64]bool
	rates     map[int64]map[string][2]int64
	// answerAt, when set, is the height every answer claims.
	answerAt int64
	// codespace is the codespace of the answer for a validator it lacks, and
	// pruned the heights it holds no state for.
	codespace string
	pruned    map[int64]bool
}

func (f *fakeArchive) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/"+archiveSecret+"/abci_query", r.URL.Path)
		path, err := strconv.Unquote(r.URL.Query().Get("path"))
		require.NoError(t, err)
		data, err := hex.DecodeString(strings.TrimPrefix(r.URL.Query().Get("data"), "0x"))
		require.NoError(t, err)
		h, err := strconv.ParseInt(r.URL.Query().Get(rpcHeightParam), 10, 64)
		require.NoError(t, err)
		f.mu.Lock()
		f.calls = append(f.calls, path+"@"+strconv.FormatInt(h, 10))
		f.mu.Unlock()
		answer := func(code uint32, log string, value []byte) {
			at := h
			if f.answerAt != 0 {
				at = f.answerAt
			}
			resp := map[string]any{"code": code, "log": log, "value": base64.StdEncoding.EncodeToString(value), "height": strconv.FormatInt(at, 10)}
			if code == sdkKeyNotFound {
				resp["codespace"] = f.codespace
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"response": resp}})
		}
		marshal := func(m interface{ Marshal() ([]byte, error) }) []byte {
			bz, err := m.Marshal()
			require.NoError(t, err)
			return bz
		}
		if f.pruned[h] {
			answer(38, "failed to load state at height "+strconv.FormatInt(h, 10)+"; no commit info found: not found", nil)
			return
		}
		switch path {
		case "/cosmos.staking.v1beta1.Query/Validators":
			answer(0, "", marshal(&stakingtypes.QueryValidatorsResponse{Validators: []stakingtypes.Validator{{OperatorAddress: "cosmosvaloper1set", Tokens: math.NewInt(1), DelegatorShares: math.LegacyNewDec(1)}}}))
		case "/cosmos.staking.v1beta1.Query/DelegatorDelegations":
			var req stakingtypes.QueryDelegatorDelegationsRequest
			require.NoError(t, req.Unmarshal(data))
			if f.lostDenom[h] {
				answer(1, "invalid denom: ", nil)
				return
			}
			var resp stakingtypes.QueryDelegatorDelegationsResponse
			for v, n := range f.delegations[h] {
				resp.DelegationResponses = append(resp.DelegationResponses, stakingtypes.DelegationResponse{
					Delegation: stakingtypes.Delegation{DelegatorAddress: req.DelegatorAddr, ValidatorAddress: v, Shares: math.LegacyNewDec(n)},
					Balance:    sdk.NewCoin("uatom", math.NewInt(n)),
				})
			}
			answer(0, "", marshal(&resp))
		case "/cosmos.staking.v1beta1.Query/DelegatorValidators":
			var resp stakingtypes.QueryDelegatorValidatorsResponse
			for v := range f.delegations[h] {
				resp.Validators = append(resp.Validators, stakingtypes.Validator{OperatorAddress: v, Tokens: math.NewInt(985), DelegatorShares: math.LegacyNewDec(1000)})
			}
			answer(0, "", marshal(&resp))
		case "/store/staking/key":
			// delegationKeyPrefix, the delegator, then the validator.
			del := data[2 : 2+int(data[1])]
			rest := data[2+int(data[1]):]
			val := rest[1 : 1+int(rest[0])]
			delAddr, err := bech32.ConvertAndEncode("cosmos", del)
			require.NoError(t, err)
			valAddr, err := bech32.ConvertAndEncode("cosmosvaloper", val)
			require.NoError(t, err)
			answer(0, "", marshal(&stakingtypes.Delegation{DelegatorAddress: delAddr, ValidatorAddress: valAddr, Shares: math.LegacyNewDec(f.delegations[h][valAddr])}))
		case "/cosmos.staking.v1beta1.Query/Validator":
			var req stakingtypes.QueryValidatorRequest
			require.NoError(t, req.Unmarshal(data))
			r, ok := f.rates[h][req.ValidatorAddr]
			if !ok {
				answer(22, "rpc error: code = NotFound desc = validator not found: key not found", nil)
				return
			}
			answer(0, "", marshal(&stakingtypes.QueryValidatorResponse{Validator: stakingtypes.Validator{OperatorAddress: req.ValidatorAddr, Tokens: math.NewInt(r[0]), DelegatorShares: math.LegacyNewDec(r[1])}}))
		default:
			http.NotFound(w, r)
		}
	}))
}

func testArchive(t *testing.T, srv *httptest.Server, floor int64) *ledgerArchive {
	a, err := newLedgerArchive(srv.URL+"/"+archiveSecret, 2*time.Second)
	require.NoError(t, err)
	a.bucket = newTokenBucket(1000, 1000)
	a.floor.Store(floor)
	return a
}

func archiveCount(result string) int64 { return archiveCalls[result].Load() }

func bech(t *testing.T, prefix string, b byte) string {
	addr, err := bech32.ConvertAndEncode(prefix, []byte(strings.Repeat(string([]byte{b}), 20)))
	require.NoError(t, err)
	return addr
}

// The serving node answers at and above its lowest height and the archive
// only below it; with the lowest height unknown the archive is not read.
func TestLedgerNodeReadsArchiveOnlyBelowItsFloor(t *testing.T) {
	wallet, val := bech(t, "cosmos", 1), bech(t, "cosmosvaloper", 2)
	fake := &fakeArchive{
		delegations: map[int64]map[string]int64{99: {val: 700}},
		rates:       map[int64]map[string][2]int64{99: {val: {985, 1000}}},
	}
	srv := fake.serve(t)
	defer srv.Close()
	var mainReads []string
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainReads = append(mainReads, r.URL.Path+"@"+r.Header.Get(heightHeader))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":2,"message":"version does not exist"}`))
	}))
	defer main.Close()
	rest, err := servicehttp.New(http.DefaultClient, main.URL)
	require.NoError(t, err)

	node := &ledgerNode{rest: rest, archive: testArchive(t, srv, 100)}
	b := newArchiveBudget()
	d, err := node.delegationsAt(wallet, 99, b)
	require.NoError(t, err)
	require.Equal(t, "700", d[val].String())
	r, err := node.rateAtHeight(val, 99, b)
	require.NoError(t, err)
	require.Equal(t, "985", r.Tokens.String())
	require.True(t, r.Shares.Equal(rate(985, 1000).Shares))
	require.Empty(t, mainReads)
	require.Equal(t, archiveCallLimit-2, b.left)

	_, err = node.delegationsAt(wallet, 100, b)
	require.Error(t, err)
	_, err = node.rateAtHeight(val, 100, b)
	require.Error(t, err)
	require.Len(t, mainReads, 2, "the node's own heights are read from the node")

	node.archive.floor.Store(0)
	_, err = node.delegationsAt(wallet, 99, b)
	require.Error(t, err)
	require.Len(t, fake.calls, 2, "an unknown floor never reads the archive")
	node.archive = nil
	_, err = node.delegationsAt(wallet, 99, b)
	require.Error(t, err)
	require.Len(t, fake.calls, 2)
}

// The archive's answers are refused unless they are for the height asked,
// and a delegation query it cannot price is read from the store instead.
func TestLedgerArchiveDelegations(t *testing.T) {
	wallet, v1, v2 := bech(t, "cosmos", 1), bech(t, "cosmosvaloper", 2), bech(t, "cosmosvaloper", 3)
	fake := &fakeArchive{
		delegations: map[int64]map[string]int64{50: {v1: 1000, v2: 10}, 60: {v1: 2000}},
		lostDenom:   map[int64]bool{60: true},
		rates:       map[int64]map[string][2]int64{50: {v1: {985, 1000}}},
	}
	srv := fake.serve(t)
	defer srv.Close()
	a := testArchive(t, srv, 100)

	d, err := a.delegations(newArchiveBudget(), wallet, 50)
	require.NoError(t, err)
	intsEqual(t, "h50", d, map[string]int64{v1: 1000, v2: 10})

	// 2000 shares at 985 tokens per 1000 shares, rounded down.
	fake.calls = nil
	d, err = a.delegations(newArchiveBudget(), wallet, 60)
	require.NoError(t, err)
	intsEqual(t, "h60", d, map[string]int64{v1: 1970})
	require.Equal(t, []string{
		"/cosmos.staking.v1beta1.Query/DelegatorDelegations@60",
		"/cosmos.staking.v1beta1.Query/DelegatorValidators@60",
		"/store/staking/key@60",
	}, fake.calls)

	fake.answerAt = 51
	_, err = a.delegations(newArchiveBudget(), wallet, 50)
	require.ErrorIs(t, err, errArchiveHeight)
	_, err = a.rate(newArchiveBudget(), v1, 50)
	require.ErrorIs(t, err, errArchiveHeight)
	fake.answerAt = 0
	_, err = a.rate(newArchiveBudget(), v2, 50)
	require.ErrorIs(t, err, errArchiveFailed, "a not-found answer outside the sdk codespace")

	// The SDK's gRPC NotFound for the height asked, from an archive that
	// serves that height's validator set, says the validator did not exist
	// there; the same answer for another height says nothing.
	fake.codespace = "sdk"
	absent := archiveCount(archiveResultAbsent)
	fake.calls = nil
	_, err = a.rate(newArchiveBudget(), v2, 50)
	require.ErrorIs(t, err, errArchiveAbsent)
	require.Equal(t, absent+1, archiveCount(archiveResultAbsent))
	require.Equal(t, []string{
		"/cosmos.staking.v1beta1.Query/Validator@50",
		"/cosmos.staking.v1beta1.Query/Validators@50",
	}, fake.calls)
	fake.answerAt = 51
	_, err = a.rate(newArchiveBudget(), v2, 50)
	require.ErrorIs(t, err, errArchiveFailed)
	require.Equal(t, absent+1, archiveCount(archiveResultAbsent))
	fake.answerAt = 0
}

// Only the validator query's own gRPC NotFound, for a height whose state the
// archive serves, reads as a validator that did not exist. A height it has no
// state for, or any other error, refuses, and the walk then refuses a span
// that allows zero as it would without the archive.
func TestLedgerArchiveAbsentNeedsTheHeightServed(t *testing.T) {
	fake := &fakeArchive{codespace: "sdk", pruned: map[int64]bool{}}
	srv := fake.serve(t)
	defer srv.Close()
	a := testArchive(t, srv, 100)
	walk := func() (map[int64]ledgerHoldings, error) {
		in := absentInput(nil)
		in.RateAt = func(v string, h int64) (validatorRate, error) { return a.rate(newArchiveBudget(), v, h) }
		return walkBack(in, []int64{50, 20})
	}

	got, err := walk()
	require.NoError(t, err)
	require.NotContains(t, got[20].Delegations, ledgerTestVal)

	// A pruned height answers every query with its own not-found.
	fake.pruned[20] = true
	_, err = a.rate(newArchiveBudget(), ledgerTestVal, 20)
	require.ErrorIs(t, err, errArchiveFailed)
	got, err = walk()
	var unk ledgerUnknown
	require.ErrorAs(t, err, &unk)
	require.NotContains(t, got, int64(20))

	// The validator's NotFound with the set unreadable at the height.
	delete(fake.pruned, 20)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("path"), "Query/Validators") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv2.Close()
	a2 := testArchive(t, srv2, 100)
	_, err = a2.rate(newArchiveBudget(), ledgerTestVal, 20)
	require.ErrorIs(t, err, errArchiveFailed)

	// A not-found that is not the gRPC NotFound status, and an empty set.
	for _, c := range []struct{ validator, set string }{
		{`{"code":22,"codespace":"sdk","log":"key not found","height":"20"}`, ""},
		{"", `{"code":0,"height":"20"}`},
	} {
		srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			answer := c.validator
			if strings.Contains(r.URL.Query().Get("path"), "Query/Validators") {
				answer = c.set
			}
			if answer == "" {
				srv.Config.Handler.ServeHTTP(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"result":{"response":` + answer + `}}`))
		}))
		_, err = testArchive(t, srv3, 100).rate(newArchiveBudget(), ledgerTestVal, 20)
		srv3.Close()
		require.ErrorIs(t, err, errArchiveFailed)
	}
}

// An accepted zero is counted and logged, and a refused one only logged; the
// log names the validator by a short hash.
func TestLedgerArchiveNoteAbsent(t *testing.T) {
	val := bech(t, "cosmosvaloper", 4)
	var lines []string
	a := &ledgerArchive{logf: func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }}
	before := archiveAbsentZeros.Load()
	sp := shareSpan{decimal.RequireFromString("-0.000082"), decimal.RequireFromString("1.0001")}
	a.noteAbsent(val, 20, sp, true)
	a.noteAbsent(val, 21, sp, false)
	require.Equal(t, before+1, archiveAbsentZeros.Load())
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "height 20")
	require.Contains(t, lines[0], "shown as zero")
	require.Contains(t, lines[1], "refused")
	for _, l := range lines {
		require.NotContains(t, l, val)
	}

	rec := httptest.NewRecorder()
	NewServer(nil, nil, DefaultNativeAsset).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Contains(t, rec.Body.String(), "archive_absent_zero_total "+strconv.FormatInt(archiveAbsentZeros.Load(), 10)+"\n")
}

// No archive error names the archive's origin or anything in it, whatever
// fails: the origin, a timeout, a refused connection, a non-200, a body that
// does not decode, or an error the archive sends back.
func TestLedgerArchiveErrorsNeverNameTheOrigin(t *testing.T) {
	wallet, val := bech(t, "cosmos", 1), bech(t, "cosmosvaloper", 2)
	leak := func(t *testing.T, origin string, err error) {
		t.Helper()
		require.Error(t, err)
		u := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
		for _, s := range []string{origin, u, strings.Split(u, "/")[0], archiveSecret} {
			require.NotContains(t, err.Error(), s)
		}
		require.True(t, strings.HasPrefix(err.Error(), "archive: "), err.Error())
	}
	reads := func(t *testing.T, origin string, timeout time.Duration) {
		t.Helper()
		a, err := newLedgerArchive(origin, timeout)
		require.NoError(t, err)
		a.floor.Store(100)
		_, err = a.delegations(newArchiveBudget(), wallet, 50)
		leak(t, origin, err)
		_, err = a.rate(newArchiveBudget(), val, 50)
		leak(t, origin, err)
		node := &ledgerNode{archive: a}
		_, err = node.delegationsAt(wallet, 50, newArchiveBudget())
		leak(t, origin, err)
	}
	echo := func(status int, body func(r *http.Request) string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body(r)))
		}))
	}
	full := func(r *http.Request) string { return "http://" + r.Host + r.URL.String() }

	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
		}))
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, 50*time.Millisecond)
	})
	t.Run("refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		origin := srv.URL + "/" + archiveSecret
		srv.Close()
		reads(t, origin, time.Second)
	})
	t.Run("non-200", func(t *testing.T) {
		srv := echo(http.StatusBadGateway, func(r *http.Request) string { return `{"message":"` + full(r) + `"}` })
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, time.Second)
	})
	t.Run("redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, full(r)+"&again=1", http.StatusFound)
		}))
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, time.Second)
	})
	t.Run("decode", func(t *testing.T) {
		srv := echo(http.StatusOK, func(r *http.Request) string { return "not json " + full(r) })
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, time.Second)
	})
	t.Run("value", func(t *testing.T) {
		srv := echo(http.StatusOK, func(r *http.Request) string {
			return `{"result":{"response":{"code":0,"height":"50","value":"` + base64.StdEncoding.EncodeToString([]byte(full(r))) + `"}}}`
		})
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, time.Second)
	})
	t.Run("archive error", func(t *testing.T) {
		srv := echo(http.StatusOK, func(r *http.Request) string {
			return `{"result":{"response":{"code":18,"log":"` + full(r) + `","height":"50"}}}`
		})
		defer srv.Close()
		reads(t, srv.URL+"/"+archiveSecret, time.Second)
	})
	t.Run("origin", func(t *testing.T) {
		for _, origin := range []string{"https://user:" + archiveSecret + "@archive.example/x", "https://archive.example/" + archiveSecret + "?k=1", "ftp://archive.example/" + archiveSecret, "https://archive.example/%zz" + archiveSecret} {
			_, err := newLedgerArchive(origin, time.Second)
			leak(t, origin, err)
			leak(t, origin, StartLedgerArchive(origin, t.Logf))
		}
	})
}

// One walk spends at most archiveCallLimit archive calls; past them the
// dates stay refused, and each outcome is counted under its result.
func TestLedgerArchiveCallLimitAndCounts(t *testing.T) {
	wallet, val := bech(t, "cosmos", 1), bech(t, "cosmosvaloper", 2)
	fake := &fakeArchive{delegations: map[int64]map[string]int64{}}
	for h := int64(1); h <= 40; h++ {
		fake.delegations[h] = map[string]int64{val: 1000}
		if h >= 30 {
			fake.delegations[h][val] = 900
		}
	}
	srv := fake.serve(t)
	defer srv.Close()
	node := &ledgerNode{archive: testArchive(t, srv, 100)}

	ok, ceiling := archiveCount(archiveResultOK), archiveCount(archiveResultCeiling)
	b := newArchiveBudget()
	read := func(h int64) (map[string]*big.Int, error) { return node.delegationsAt(wallet, h, b) }
	// Ten blocks of someone else's records cost two calls each; the eleventh,
	// the wallet's own unbond, is past the limit and stays refused.
	var unmatched []ledgerEvent
	for i, h := range []int64{40, 39, 38, 37, 36, 35, 34, 33, 32, 31, 30} {
		unmatched = append(unmatched, ev(uint64(100+i), h, phaseTx, "unbond", "validator", val, "amount", "100uatom", "delegator", wallet))
	}
	owned, unattributed := settleUnnamedStakes(read, wallet, "", "uatom", nil, unmatched)
	require.Empty(t, owned)
	require.Equal(t, int64(30), unattributed)
	require.Len(t, fake.calls, archiveCallLimit)
	require.Equal(t, int64(archiveCallLimit), archiveCount(archiveResultOK)-ok)
	require.Equal(t, int64(1), archiveCount(archiveResultCeiling)-ceiling)

	// A fresh walk has its own limit.
	owned, unattributed = settleUnnamedStakes(func(h int64) (map[string]*big.Int, error) {
		return node.delegationsAt(wallet, h, newArchiveBudget())
	}, wallet, "", "uatom", nil, unmatched[len(unmatched)-1:])
	require.Zero(t, unattributed)
	require.Len(t, owned, 1)

	rec := httptest.NewRecorder()
	NewServer(nil, nil, DefaultNativeAsset).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	for _, r := range archiveResults {
		require.Contains(t, rec.Body.String(), "archive_calls_total{result=\""+r+"\"} "+strconv.FormatInt(archiveCount(r), 10)+"\n")
	}
	require.NotContains(t, rec.Body.String(), wallet)
}

// The bucket lets a burst through at once, then one call per 1/rate, and
// turns a call away, taking nothing, when its turn is further off than the
// wait allowed.
func TestTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	var slept []time.Duration
	b := newTokenBucket(2, 4)
	b.now = func() time.Time { return now }
	b.sleep = func(d time.Duration) { slept = append(slept, d) }
	for range 4 {
		require.True(t, b.take(0))
	}
	require.Empty(t, slept)
	require.False(t, b.take(400*time.Millisecond))
	require.True(t, b.take(time.Second))
	require.Equal(t, []time.Duration{500 * time.Millisecond}, slept)
	require.True(t, b.take(time.Second))
	require.Equal(t, time.Second, slept[1], "a second waiter queues behind the first")
	now = now.Add(10 * time.Second)
	for range 4 {
		require.True(t, b.take(0))
	}
	require.False(t, b.take(0), "the bucket refills to its burst, no further")
}

// The floor is the serving node's earliest_block_height from its RPC status.
func TestLedgerNodeEarliestHeight(t *testing.T) {
	answer := `{"jsonrpc":"2.0","id":-1,"result":{"sync_info":{"earliest_block_height":"33800001","latest_block_height":"34000000"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/status", r.URL.Path)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	rpc, err := servicehttp.New(http.DefaultClient, srv.URL)
	require.NoError(t, err)
	node := &ledgerNode{rpc: rpc}
	h, err := node.earliestHeight()
	require.NoError(t, err)
	require.Equal(t, int64(33800001), h)
	answer = `{"result":{"sync_info":{}}}`
	_, err = node.earliestHeight()
	require.Error(t, err)
}
