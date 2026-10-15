package taxapi

import "testing"

func TestChainParam(t *testing.T) {
	cases := map[string]string{
		"": "mainnet", "mainnet": "mainnet", "cosmoshub": "mainnet", "cosmoshub-4": "mainnet", " CosmosHub ": "mainnet",
		"testnet": "testnet", "theta-testnet-001": "testnet", "osmosis-1": "osmosis-1",
	}
	for in, want := range cases {
		if got := chainParam(in); got != want {
			t.Errorf("chainParam(%q)=%q want %q", in, got, want)
		}
	}
}
