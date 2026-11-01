package taxapi

import "testing"

func TestChainParam(t *testing.T) {
	if got := chainParam(" CosmosHub "); got != apiMainnet {
		t.Fatalf("whitespace/case normalization: %q", got)
	}
	cases := map[string]string{
		"": apiMainnet, apiMainnet: apiMainnet, apiCosmoshub: apiMainnet, apiHubChainID: apiMainnet,
		apiTestnet: apiTestnet, apiThetaTestnet001: apiTestnet, "osmosis-1": "osmosis-1",
	}
	for in, want := range cases {
		if got := chainParam(in); got != want {
			t.Errorf("chainParam(%q)=%q want %q", in, got, want)
		}
	}
}
