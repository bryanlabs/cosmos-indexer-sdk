package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const cachedHeaderBody = `{"result":{"block":{"header":{"chain_id":"cosmoshub-4","height":"123","time":"2026-11-12T23:59:59.123456789Z"}}}}`

func TestFlowHeaderCacheSharesExactResponseAndWalletReceipts(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(cachedHeaderBody))
	}))
	defer server.Close()
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	const clients = 12
	var began, done sync.WaitGroup
	began.Add(clients)
	done.Add(clients)
	receipts := make([][]FlowArchiveReceipt, clients)
	errors := make([]error, clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			defer done.Done()
			began.Done()
			_, errors[i] = a.header(fmt.Sprintf("wallet-%d", i), 123, &receipts[i])
		}(i)
	}
	<-started
	began.Wait()
	close(release)
	done.Wait()
	require.EqualValues(t, 1, calls.Load())
	hash := sha256.Sum256([]byte(cachedHeaderBody))
	var reused int
	for i := 0; i < clients; i++ {
		require.NoError(t, errors[i])
		require.Len(t, receipts[i], 1)
		r := receipts[i][0]
		require.Equal(t, fmt.Sprintf("wallet-%d", i), r.Address)
		require.Equal(t, cachedHeaderBody, r.Response)
		require.Equal(t, hex.EncodeToString(hash[:]), r.SHA256)
		require.Len(t, a.evidence(map[string]bool{r.Address: true}), 1)
		if r.Reused {
			reused++
		}
	}
	require.Equal(t, clients-1, reused)
	require.Len(t, a.evidence(nil), clients)
}

func TestFlowHeaderCacheNeverCachesInvalidResponses(t *testing.T) {
	for _, invalid := range []string{
		strings.Replace(cachedHeaderBody, "cosmoshub-4", "wrong-chain", 1),
		strings.Replace(cachedHeaderBody, `"123"`, `"124"`, 1),
		strings.Replace(cachedHeaderBody, `"result":`, `"error":{"code":-1},"result":`, 1),
		`{"result":{}}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := cachedHeaderBody
				if calls.Add(1) == 1 {
					body = invalid
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			a, err := newFlowArchive(server.URL)
			require.NoError(t, err)
			var local []FlowArchiveReceipt
			_, err = a.header("bad", 123, &local)
			require.Error(t, err)
			at, err := a.header("good", 123, &local)
			require.NoError(t, err)
			require.Equal(t, "2026-11-12T23:59:59.123456789Z", at.Format(time.RFC3339Nano))
			_, err = a.header("reuse", 123, &local)
			require.NoError(t, err)
			require.EqualValues(t, 2, calls.Load())
			require.Len(t, local, 3)
			require.Equal(t, invalid, local[0].Response)
			require.False(t, local[0].Reused)
			require.True(t, local[2].Reused)
		})
	}
}

func TestFlowHeaderCacheFailedFlightDoesNotFailWaitingWallets(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	invalid := strings.Replace(cachedHeaderBody, "cosmoshub-4", "wrong-chain", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := cachedHeaderBody
		if calls.Add(1) == 1 {
			close(started)
			<-release
			body = invalid
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	const waiters = 8
	var done sync.WaitGroup
	done.Add(waiters + 1)
	receipts := make([][]FlowArchiveReceipt, waiters+1)
	errors := make([]error, waiters+1)
	request := func(i int) {
		defer done.Done()
		_, errors[i] = a.header(fmt.Sprintf("wallet-%d", i), 123, &receipts[i])
	}
	go request(0)
	<-started
	for i := 1; i <= waiters; i++ {
		go request(i)
	}
	require.Eventually(t, func() bool {
		a.headers.mu.Lock()
		defer a.headers.mu.Unlock()
		f := a.headers.flights[123]
		return f != nil && f.waiters == waiters
	}, 5*time.Second, time.Millisecond, "all wallets must actually join the failed flight")
	close(release)
	done.Wait()
	require.Error(t, errors[0])
	require.Len(t, receipts[0], 1)
	require.Equal(t, invalid, receipts[0][0].Response)
	require.False(t, receipts[0][0].Reused)
	hash := sha256.Sum256([]byte(cachedHeaderBody))
	for i := 1; i <= waiters; i++ {
		require.NoError(t, errors[i])
		require.Len(t, receipts[i], 1)
		r := receipts[i][0]
		require.Equal(t, fmt.Sprintf("wallet-%d", i), r.Address)
		require.Equal(t, cachedHeaderBody, r.Response)
		require.Equal(t, hex.EncodeToString(hash[:]), r.SHA256)
		require.Len(t, a.evidence(map[string]bool{r.Address: true}), 1)
	}
	require.EqualValues(t, 2, calls.Load())
}

func TestFlowHeaderCacheEndpointIsolation(t *testing.T) {
	for _, year := range []string{"2025", "2026"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Replace(cachedHeaderBody, "2026", year, 1)))
		}))
		a, err := newFlowArchive(server.URL)
		require.NoError(t, err)
		at, err := a.header("wallet", 123)
		require.NoError(t, err)
		require.Equal(t, year, at.Format("2006"))
		server.Close()
	}
}

func TestFlowHeaderCacheBoundsAndRetries(t *testing.T) {
	var c flowHeaderCache
	calls := 0
	get := func(height int64, raw string) (bool, error) {
		_, reused, err := c.get(height, func() (string, error) {
			calls++
			return raw, nil
		})
		return reused, err
	}
	for i := int64(0); i < flowHeaderCacheEntries+1; i++ {
		reused, err := get(i, "body")
		require.NoError(t, err)
		require.False(t, reused)
	}
	require.Len(t, c.entries, flowHeaderCacheEntries)
	_, retained := c.entries[0]
	require.False(t, retained)
	require.Equal(t, flowHeaderCacheEntries*4, c.bytes)
	_, _, err := c.get(500, func() (string, error) { return "failure bytes", fmt.Errorf("unavailable") })
	require.Error(t, err)
	_, retained = c.entries[500]
	require.False(t, retained)
	reused, err := get(500, "recovered")
	require.NoError(t, err)
	require.False(t, reused)

	var byteBound flowHeaderCache
	large := strings.Repeat("x", flowHeaderCacheBytes/2+1)
	for i := int64(0); i < 2; i++ {
		_, _, err = byteBound.get(i, func() (string, error) { return large, nil })
		require.NoError(t, err)
	}
	require.Len(t, byteBound.entries, 1)
	require.LessOrEqual(t, byteBound.bytes, flowHeaderCacheBytes)
	oversize := strings.Repeat("x", flowHeaderCacheBytes+1)
	_, _, err = byteBound.get(3, func() (string, error) { return oversize, nil })
	require.NoError(t, err)
	_, retained = byteBound.entries[3]
	require.False(t, retained)
}
