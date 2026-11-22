package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestCommissionReadTimeoutPrivateOnly(t *testing.T) {
	for _, tc := range []struct {
		flows   bool
		version string
		timeout time.Duration
		valid   bool
	}{
		{false, "", 0, true}, {true, "", 0, true},
		{true, "private", time.Minute, true}, {true, "private", 0, true},
		{true, "", time.Minute, false}, {true, "  ", time.Minute, false},
		{false, "private", time.Minute, false}, {true, "private", -time.Second, false},
	} {
		if err := validateCommissionReadTimeout(tc.flows, tc.version, tc.timeout); (err == nil) != tc.valid {
			t.Fatalf("flows=%v version=%q timeout=%v: %v", tc.flows, tc.version, tc.timeout, err)
		}
	}
}

func TestCommissionRuntimeParamsPrivateOnly(t *testing.T) {
	for _, c := range []struct {
		name    string
		flows   bool
		version string
		want    []string
	}{
		{"ordinary_ledger", false, "", nil},
		{"version_without_flow_mode", false, "private-version", nil},
		{"flow_without_private_version", true, "", []string{"jit=off"}},
		{"private_commission", true, "private-version", []string{"jit=off", "effective_io_concurrency=200"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := commissionRuntimeParams(c.flows, c.version); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("runtime params = %v, want %v", got, c.want)
			}
		})
	}
}

func TestReadCommissionHopPlan(t *testing.T) {
	path := t.TempDir() + "/plan"
	if err := os.WriteFile(path, []byte("10 v1\n8 v1\n10 v2\n\n3 v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, err := readCommissionHopPlan(path, "ops847-proof-x-hops{hops}")
	if err != nil {
		t.Fatal(err)
	}
	want := []commissionHopRun{{hops: 10, only: "v1,v2", version: "ops847-proof-x-hops10"}, {hops: 8, only: "v1", version: "ops847-proof-x-hops8"}, {hops: 3, only: "v2", version: "ops847-proof-x-hops3"}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("plan %+v, want %+v", runs, want)
	}
	for _, bad := range []string{"", "10\n", "x v1\n", "0 v1\n", "5 v1\n5 v1\n", "5 v1 extra\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCommissionHopPlan(path, "v-hops{hops}"); err == nil {
			t.Fatalf("plan %q accepted", bad)
		}
	}
}
