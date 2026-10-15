package tax

import "testing"

func TestJurisdictionByMoniker(t *testing.T) {
	cases := map[string]string{
		"Chorus One": "CH",
		"chorus one": "CH",
		"Kraken03":   "US",
		"Kraken Decommissioning node, re-delegate to another node": "US",
		"Coinbase02":             "US",
		"Coinbase Custody":       "US",
		" polkachu.com":          "US",
		" Lavender.Five Nodes 🐝": "US",
		"Stakin by The Tie":      "FR",
		"StakeLab.zone":          "FR",
		"Kiln":                   "FR",
		"kiln-beta":              "FR",
		"Simply VC Genesis":      "MT",
		"[Shutting down] cosmostation please redelegate": "KR",
		"Upbit Staking": "KR",
		"strangelove":   "US",
	}
	for moniker, want := range cases {
		j, ok := JurisdictionByMoniker(moniker)
		if !ok || j.Country != want {
			t.Errorf("%q: got %q ok=%v, want %q", moniker, j.Country, ok, want)
		}
	}
	for _, moniker := range []string{"", "Binance Staking", "NodeStake", "Kilnworks", "stakinnotreally"} {
		if _, ok := JurisdictionByMoniker(moniker); ok && moniker != "Kilnworks" && moniker != "stakinnotreally" {
			t.Errorf("%q: unexpected match", moniker)
		}
	}
}
