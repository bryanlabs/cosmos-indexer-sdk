package taxapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A fresh API must never call an unknown or empty expected-validator list a
// complete zero-of-zero run. This needs no financial data or database writes.
func TestCommissionReadinessSurvivesStatementBuild(t *testing.T) {
	for _, expected := range []map[string]bool{nil, {}} {
		s := &Server{flowExp: map[string]*flowExpected{apiMainnet: {set: expected, at: time.Now(), loading: true}}}
		rec := httptest.NewRecorder()
		s.handleCommissionFlowsHub(rec, httptest.NewRequest(http.MethodGet, "/commission-flows?chain=cosmoshub&rule=fifo&start=2026-01&end=2026-10", nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Equal(t, "60", rec.Header().Get("Retry-After"))
		require.NotContains(t, rec.Body.String(), `"complete":true`)
	}
}
