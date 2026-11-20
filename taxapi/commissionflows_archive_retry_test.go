package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func flowArchiveRetryServer(t *testing.T, failures int64, body any) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n := hits.Add(1); {
		case n <= failures && n%2 == 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case n <= failures:
			_, _ = w.Write([]byte(`{"result":`))
		default:
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestFlowArchiveRetriesReadsWithoutResponse(t *testing.T) {
	header := map[string]any{"result": map[string]any{"block": map[string]any{"header": map[string]string{"chain_id": "cosmoshub-4", "height": "123", "time": "2026-11-12T23:59:59Z"}}}}
	server, hits := flowArchiveRetryServer(t, 3, header)
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	var waits []time.Duration
	a.pause = func(d time.Duration) { waits = append(waits, d) }
	at, err := a.header(flowRowsHolder, 123)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 12, 23, 59, 59, 0, time.UTC), at)
	require.Equal(t, int64(4), hits.Load())
	require.Equal(t, int64(4), a.callAttempts.Load())
	require.Equal(t, flowArchiveRetryWaits[:3], waits)
	receipts := a.evidence(map[string]bool{flowRowsHolder: true})
	require.Len(t, receipts, 1, "only the response that arrived is a receipt")
	require.Equal(t, "block", receipts[0].Method)
}

func TestFlowArchiveRetriesAreBounded(t *testing.T) {
	server, hits := flowArchiveRetryServer(t, 1000, nil)
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	var waited time.Duration
	a.pause = func(d time.Duration) { waited += d }
	_, err = a.header(flowRowsHolder, 123)
	require.EqualError(t, err, "commission archive block request failed")
	require.Equal(t, int64(len(flowArchiveRetryWaits)+1), hits.Load())
	require.Equal(t, 14*time.Minute+52*time.Second, waited)
	require.Empty(t, a.evidence(map[string]bool{flowRowsHolder: true}))
}

func TestFlowArchiveDoesNotRetryAnAnsweredError(t *testing.T) {
	server, hits := flowArchiveRetryServer(t, 0, map[string]any{"error": map[string]any{"code": -32603, "message": "unavailable"}})
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	a.pause = func(time.Duration) { t.Fatal("an answered error must not be retried") }
	var body json.RawMessage
	err = a.response(flowRowsHolder, 123, "abci_query", nil, &body)
	require.EqualError(t, err, "invalid commission archive abci_query envelope")
	require.Equal(t, int64(1), hits.Load())
	require.Len(t, a.evidence(map[string]bool{flowRowsHolder: true}), 1)
}
