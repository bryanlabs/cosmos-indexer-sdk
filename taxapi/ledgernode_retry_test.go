package taxapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLedgerNodeRetryCarriesAnchorThroughANodeRestart(t *testing.T) {
	var bank atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/v1beta1/balances/"):
			if bank.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"balances":[{"denom":"uatom","amount":"5"}],"pagination":{}}`))
		case strings.HasPrefix(r.URL.Path, "/cosmos/staking/v1beta1/delegations/"):
			_, _ = w.Write([]byte(`{"delegation_responses":[],"pagination":{}}`))
		default:
			_, _ = w.Write([]byte(`{"unbonding_responses":[],"pagination":{}}`))
		}
	}))
	defer srv.Close()
	t.Setenv("NODE_REST_API", srv.URL)
	t.Setenv("NODE_RPC_API", srv.URL)

	SetLedgerNodeRetry(0, nil)
	node, err := newLedgerNode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.anchor("cosmos1test", 10); err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("without retry: %v", err)
	}

	var logs atomic.Int32
	SetLedgerNodeRetry(10*time.Minute, func(string, ...any) { logs.Add(1) })
	defer SetLedgerNodeRetry(0, nil)
	bank.Store(0)
	node, err = newLedgerNode()
	if err != nil {
		t.Fatal(err)
	}
	a, err := node.anchor("cosmos1test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if a.Bank["uatom"].Int64() != 5 || bank.Load() != 2 || logs.Load() != 1 {
		t.Fatalf("anchor %v bank reads %d retry logs %d", a.Bank, bank.Load(), logs.Load())
	}
}
