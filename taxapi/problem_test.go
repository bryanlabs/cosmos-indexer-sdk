package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func problemServe(t *testing.T, h http.HandlerFunc, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/delegator-report?address=x&api_key=secret", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	withProblems(h).ServeHTTP(rec, req)
	return rec
}

// problemDetail decodes a problem body and returns its detail.
func problemDetail(t *testing.T, body string) string {
	t.Helper()
	var p Problem
	require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	require.NotEmpty(t, p.Code, body)
	return p.Detail
}

func TestPlainTextErrorBecomesProblem(t *testing.T) {
	rec := problemServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "this key's hourly limit is used up <try later>", http.StatusTooManyRequests)
	}, map[string]string{"X-Request-Id": "req-123"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, problemContentType, rec.Header().Get("Content-Type"))
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "https://bryanlabs.net/api-docs/conventions#rate_limited", p["type"])
	require.Equal(t, "Too Many Requests", p["title"])
	require.EqualValues(t, 429, p["status"])
	require.Equal(t, "this key's hourly limit is used up <try later>", p["detail"])
	require.Equal(t, "rate_limited", p["code"])
	require.Equal(t, "/delegator-report", p["instance"], "the query, with its api_key, is never echoed")
	require.Equal(t, "req-123", p["request_id"])
	require.EqualValues(t, 60, p["retry_after"])
	require.Contains(t, rec.Body.String(), "<try later>", "detail is not HTML-escaped")
}

func TestProblemWithoutRequestIDOrDetail(t *testing.T) {
	rec := problemServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "internal_error", p["code"])
	require.Equal(t, "Internal Server Error", p["detail"])
	require.NotContains(t, p, "request_id")
	require.NotContains(t, p, "retry_after")
}

func TestJSONErrorBodyPassesUnchanged(t *testing.T) {
	body := `{"error":"asset review required","pending":3}`
	rec := problemServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(body))
	}, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, apiJSONContentType, rec.Header().Get("Content-Type"))
	require.Equal(t, body, rec.Body.String())
}

func TestSuccessAndCSVErrorsStreamThrough(t *testing.T) {
	rec := problemServe(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())

	rec = problemServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", apiCSVContentType)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("a,b\n"))
	}, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, "a,b\n", rec.Body.String())
}

func TestUnknownRouteIsAProblem(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	rec := httptest.NewRecorder()
	withProblems(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, problemContentType, rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Body.String(), `"code":"not_found"`)
}

func TestPublicChainNamesTheHubCosmoshub(t *testing.T) {
	require.Equal(t, "cosmoshub", publicChain(chainParam("mainnet")))
	require.Equal(t, "cosmoshub", publicChain(chainParam("cosmoshub-4")))
	require.Equal(t, apiTestnet, publicChain(chainParam("provider")))
}

func TestStatementUSDInCents(t *testing.T) {
	m := ValidatorStatementMonth{Month: "2025-04", CommissionUSD: decimal.RequireFromString("13991.0451234567890123456789"), DelegatorRewardsUSD: decimal.RequireFromString("12.5")}
	b, err := json.Marshal(m.centsUSD())
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, "13991.05", got["commission_usd"])
	require.Equal(t, "12.50", got["delegator_rewards_usd"])
	require.Equal(t, "2025-04", got["month"])
	require.Len(t, got, 11, "the view keeps every statement column once")
}
