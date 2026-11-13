package taxapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	querytypes "github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"
)

func TestFlowArchiveDayBoundaryPrecision(t *testing.T) {
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	at := end.Add(-time.Second).Add(123456789 * time.Nanosecond)
	indexed := at.Truncate(time.Microsecond)
	require.True(t, flowArchiveDayBoundary(at, end, indexed, end))
	require.True(t, flowArchiveDayBoundary(at, end, indexed.Add(time.Microsecond), end))
	require.False(t, flowArchiveDayBoundary(at, end, indexed.Add(2*time.Microsecond), end))
	require.False(t, flowArchiveDayBoundary(at, end.Add(-time.Nanosecond), indexed, end))
	require.False(t, flowArchiveDayBoundary(end, end.Add(time.Second), end, end))
}

func TestFlowArchiveExactHeightAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, height, value string
		code                uint32
		wantErr             bool
		rpcError            bool
	}{
		{name: "exact", height: "123", value: "coins"},
		{name: "exact_empty_balance", height: "123", value: "empty"},
		{name: "missing_payload", height: "123", wantErr: true},
		{name: "rpc_error_with_result", height: "123", value: "coins", rpcError: true, wantErr: true},
		{name: "wrong_height", height: "124", value: "coins", wantErr: true},
		{name: "error_response", height: "123", code: 38, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/abci_query", r.URL.Path)
				require.Equal(t, "123", r.URL.Query().Get("height"))
				require.Equal(t, strconv.Quote("/cosmos.bank.v1beta1.Query/AllBalances"), r.URL.Query().Get("path"))
				data, err := hex.DecodeString(strings.TrimPrefix(r.URL.Query().Get("data"), "0x"))
				require.NoError(t, err)
				var request banktypes.QueryAllBalancesRequest
				require.NoError(t, request.Unmarshal(data))
				require.Equal(t, flowRowsHolder, request.Address)
				response := banktypes.QueryAllBalancesResponse{}
				if tc.value != "" {
					response.Pagination = &querytypes.PageResponse{}
				}
				if tc.value == "coins" {
					response.Balances = sdk.NewCoins(sdk.NewInt64Coin("uatom", 1234))
				}
				raw, err := response.Marshal()
				require.NoError(t, err)
				body := map[string]any{"result": map[string]any{"response": map[string]any{"code": tc.code, "height": tc.height, "value": base64.StdEncoding.EncodeToString(raw)}}}
				if tc.rpcError {
					body["error"] = map[string]any{"code": -32603, "message": "unavailable"}
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			a, err := newFlowArchive(server.URL)
			require.NoError(t, err)
			var response banktypes.QueryAllBalancesResponse
			err = a.query(flowRowsHolder, 123, "/cosmos.bank.v1beta1.Query/AllBalances", &banktypes.QueryAllBalancesRequest{Address: flowRowsHolder}, &response)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			evidence := a.evidence(map[string]bool{flowRowsHolder: true})
			require.Len(t, evidence, 1)
			require.Len(t, evidence[0].SHA256, 64)
			require.NotEmpty(t, evidence[0].RequestData)
			require.NotContains(t, evidence[0].Response, server.URL)
			// JSONB may reorder the containing document but cannot rewrite
			// the response now that it is stored as an opaque string.
			encoded, err := json.Marshal(evidence)
			require.NoError(t, err)
			var normalized any
			require.NoError(t, json.Unmarshal(encoded, &normalized))
			encoded, err = json.Marshal(normalized)
			require.NoError(t, err)
			var restored []FlowArchiveReceipt
			require.NoError(t, json.Unmarshal(encoded, &restored))
			require.Equal(t, evidence[0].Response, restored[0].Response)
			hash := sha256.Sum256([]byte(restored[0].Response))
			require.Equal(t, restored[0].SHA256, hex.EncodeToString(hash[:]))
		})
	}
}

func TestFlowArchiveRejectsWrongChainOrHeader(t *testing.T) {
	for _, tc := range []struct {
		chain, height string
		wantErr       bool
	}{
		{chain: "cosmoshub-4", height: "123"},
		{chain: "wrong-chain", height: "123", wantErr: true},
		{chain: "cosmoshub-4", height: "124", wantErr: true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"block": map[string]any{"header": map[string]string{"chain_id": tc.chain, "height": tc.height, "time": "2026-11-12T23:59:59Z"}}}})
		}))
		a, err := newFlowArchive(server.URL)
		require.NoError(t, err)
		_, err = a.header(flowRowsHolder, 123)
		if tc.wantErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		server.Close()
	}
}
