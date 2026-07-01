package taxapi

import "testing"

func TestIBCBaseDenom(t *testing.T) {
	cases := []struct {
		in    string
		base  string
		isIBC bool
	}{
		{"transfer/channel-0/uatom", "uatom", true},
		{"transfer/channel-1/uatom", "uatom", true},
		{"transfer/channel-0/transfer/08-wasm-1369/0xabc", "0xabc", true},
		{"transfer/channel-0/factory/cosmos1xyz/ustars", "factory/cosmos1xyz/ustars", true},
		{"uatom", "uatom", false},
		{"ibc/ABC123", "ibc/ABC123", false},
	}
	for _, c := range cases {
		base, isIBC := ibcBaseDenom(c.in)
		if base != c.base || isIBC != c.isIBC {
			t.Fatalf("ibcBaseDenom(%q) = (%q,%v), want (%q,%v)", c.in, base, isIBC, c.base, c.isIBC)
		}
	}
}
