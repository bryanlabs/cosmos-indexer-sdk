package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBlockResultsJSONRPCPreservesBasePath(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/private-example", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer example", r.Header.Get("Authorization"))
		var body struct {
			Method string            `json:"method"`
			ID     json.RawMessage   `json:"id"`
			Params map[string]string `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "block_results", body.Method)
		require.Equal(t, "25002500", body.Params["height"])
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(body.ID) + `,"result":{"height":"25002500","txs_results":[{"code":0}]}}`))
		require.NoError(t, err)
	}))
	defer server.Close()
	client := URIClient{Address: server.URL + "/private-example", Client: server.Client(), AuthHeader: "Bearer example", JSONRPC: true}
	height := int64(25002500)
	result, err := client.DoBlockResults(context.Background(), &height)
	require.NoError(t, err)
	require.Equal(t, height, result.Height)
	require.Len(t, result.TxsResults, 1)
	require.Equal(t, 1, calls)
}

func TestBlockResultsJSONRPCRejectsBadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client := URIClient{Address: server.URL, Client: server.Client(), JSONRPC: true}
	height := int64(1)
	_, err := client.DoBlockResults(context.Background(), &height)
	require.ErrorContains(t, err, "HTTP 502")
}
