package taxapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidatorMonikersPaginationAndCaching(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/cosmos/staking/v1beta1/validators" || r.URL.Query().Get("pagination.limit") != "1000" {
			t.Error("bad validator route")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pagination.key") == "" {
			_, _ = w.Write([]byte(`{"validators":[{"operator_address":"val1","description":{"moniker":"First"}}],"pagination":{"next_key":"abc+/="}}`))
		} else {
			if r.URL.Query().Get("pagination.key") != "abc+/=" {
				t.Error("pagination key corrupted")
			}
			_, _ = w.Write([]byte(`{"validators":[{"operator_address":"val2","description":{"moniker":"Second"}}],"pagination":{"next_key":""}}`))
		}
	}))
	defer srv.Close()
	o := NewOracle("", srv.URL)
	names := o.ValidatorMonikers()
	if names["val1"] != "First" || names["val2"] != "Second" || calls != 2 {
		t.Fatalf("bad pagination: %v calls=%d", names, calls)
	}
	if names = o.ValidatorMonikers(); len(names) != 2 || calls != 2 {
		t.Fatal("cache not used")
	}
}

func TestValidatorLookupFailureDoesNotInventNames(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer srv.Close()
	o := NewOracle("", srv.URL)
	for i := 0; i < 5; i++ {
		if len(o.ValidatorMonikers()) != 0 {
			t.Fatal("invented name")
		}
	}
	if calls != 1 {
		t.Fatalf("failure cache not used: %d", calls)
	}
}

func TestIncomeCSVIncludesValidatorAndTrigger(t *testing.T) {
	rows := []Row{{Category: "reward", Symbol: "ATOM", Amount: d(2), ValueUSD: d(4), ValidatorAddress: "cosmosvaloper1test", ValidatorMoniker: "Validator Name", RewardTrigger: "redelegate"}}
	csv := buildIncomeCSV(rows, "wallet", RecognitionPolicyDefault)
	if !strings.Contains(csv, "validator_address,validator_moniker,reward_trigger") || !strings.Contains(csv, "cosmosvaloper1test,Validator Name,redelegate") {
		t.Fatal(csv)
	}
}
