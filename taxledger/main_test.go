package main

import (
	"reflect"
	"testing"
)

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
