package main

import (
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
