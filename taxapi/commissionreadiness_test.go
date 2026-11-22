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
	for _, expected := range []map[string]time.Time{nil, {}} {
		s := &Server{flowExp: map[string]*flowExpected{apiMainnet: {first: expected, at: time.Now(), loading: true}}}
		rec := httptest.NewRecorder()
		s.handleCommissionFlowsHub(rec, httptest.NewRequest(http.MethodGet, "/commission-flows?chain=cosmoshub&rule=fifo&start=2026-01&end=2026-10", nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Equal(t, "60", rec.Header().Get("Retry-After"))
		require.NotContains(t, rec.Body.String(), `"complete":true`)
	}
}

// The served as_of is the last day a trace includes (a run built with
// exclusive end 2026-11-13 stores as_of 2026-11-12). A validator whose first
// withdrawal falls on that day is in the trace and expected; one whose first
// withdrawal is on a later day cannot be in it, so it is not expected yet.
func TestFlowExpectedByAsOf(t *testing.T) {
	asOf := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	first := map[string]time.Time{
		"early":    time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC),
		"onasof":   asOf,
		"lateasof": time.Date(2026, 11, 12, 15, 14, 0, 0, time.UTC),
		"lastin":   asOf.AddDate(0, 0, 1).Add(-time.Nanosecond),
		"nextday":  asOf.AddDate(0, 0, 1),
		"later":    time.Date(2026, 11, 21, 15, 14, 0, 0, time.UTC),
	}
	require.Equal(t, map[string]bool{"early": true, "onasof": true, "lateasof": true, "lastin": true}, flowExpectedBy(first, asOf))
	require.Len(t, flowExpectedBy(first, time.Time{}), 6, "with nothing traced yet every withdrawer is expected")
}
