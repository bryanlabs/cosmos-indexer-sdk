//go:build ops818harness

package taxapi

import (
	"encoding/json"
	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestOPS818ParallelActualCompleteWallets(t *testing.T) {
	var scope []struct{ Address, Since, Input string }
	b, err := os.ReadFile(os.Getenv("H_PARALLEL_SCOPE"))
	if err != nil || json.Unmarshal(b, &scope) != nil || len(scope) != 2 {
		t.Fatal("two actual complete cases required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-two-actual-wallets-30s-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=0", "lock_timeout=2s", "effective_io_concurrency=200")
	if err != nil {
		t.Fatal("source connection failed")
	}
	type result struct {
		Milliseconds   int64
		Events, Fees   int
		ByteEquivalent bool
		Error          string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	startAll := make(chan struct{})
	for i, scope := range scope {
		i, scope := i, scope
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startAll
			s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
			s.readOnlyLedger = true
			start, e := time.Parse(time.DateOnly, scope.Since)
			if e != nil {
				results[i].Error = "invalid private day"
				return
			}
			var want reportInput
			data, e := os.ReadFile(scope.Input)
			if e != nil || json.Unmarshal(data, &want) != nil {
				results[i].Error = "retained actual input unavailable"
				return
			}
			began := time.Now()
			got, e := s.readReportInput(apiMainnet, []string{scope.Address}, start, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), false)
			results[i].Milliseconds = time.Since(began).Milliseconds()
			if e != nil {
				results[i].Error = e.Error()
				return
			}
			results[i].Events = len(got.Wallets[0].Events)
			results[i].Fees = len(got.Wallets[0].Fees)
			results[i].ByteEquivalent = reflect.DeepEqual(got.Wallets, want.Wallets)
		}()
	}
	close(startAll)
	wg.Wait()
	if err = privateJSON(os.Getenv("H_PARALLEL_OUTPUT"), results); err != nil {
		t.Fatal("private result save failed")
	}
	for i, r := range results {
		if r.Error != "" || !r.ByteEquivalent {
			t.Errorf("actual concurrent case %d failed or changed rows; private details retained", i+1)
		} else {
			t.Log("actual concurrent complete wallet", i+1, "ms", r.Milliseconds, "events", r.Events, "fees", r.Fees, "same_all_rows", r.ByteEquivalent)
		}
	}
}
