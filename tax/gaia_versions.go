package tax

// GaiaRelease records what a Cosmos Hub binary release changed that matters
// to an indexer: new message types to parse, removed modules whose rows stop
// appearing, and anything that interrupted the chain. The release watcher
// compares this registry with GitHub and the node so a new release that is
// not described here raises attention before it is applied on chain.
type GaiaRelease struct {
	Version       string `json:"version"`
	Date          string `json:"date"`
	SDK           string `json:"cosmos_sdk"`
	UpgradeHeight int64  `json:"upgrade_height,omitempty"`
	Summary       string `json:"summary"`
	IndexerImpact string `json:"indexer_impact"`
	// Reviewed is true once the tax layer has been checked against this
	// release. The watcher treats an unreviewed or unknown release as
	// needing attention.
	Reviewed bool `json:"reviewed"`
}

// GaiaReleases is ordered oldest to newest. Dates are the CHANGELOG dates.
var GaiaReleases = []GaiaRelease{
	{"v25.0.0", "2025-06-20", "0.53.2", 0, "Cosmos SDK 0.53, wasmd 0.60.1. Liquid staking module (x/liquid) carries tokenize shares.", "Liquid staking message types (/gaia.liquid.v1beta1.*) are parsed for rewards and share records.", true},
	{"v25.1.0", "2025-07-08", "0.53.3", 0, "Dependency release.", "None.", true},
	{"v25.2.0", "2025-11-06", "0.53.3", 0, "Dependency release, wasmd 0.60.2.", "None.", true},
	{"v25.3.0", "2025-12-19", "0.53.4", 0, "Dependency release.", "None.", true},
	{"v25.3.1", "2026-01-15", "0.53.4", 0, "Binary-only security fix.", "None.", true},
	{"v25.3.2", "2026-01-23", "0.53.4", 0, "CometBFT 0.38.21.", "None.", true},
	{"v26.0.0", "2026-02-09", "0.53.4", 0, "Adds x/tokenfactory. Stake validation on wasm vote messages.", "Token factory denoms (factory/<creator>/<subdenom>) appear as bank coins and are priced through the oracle when known; token factory message types are registered.", true},
	{"v27.0.0", "2026-02-20", "0.53.4", 0, "Gas surcharge on multi-send, stake validation for ICA votes, no new consumer chains.", "None. Multi-send rows unchanged.", true},
	{"v27.1.0", "2026-03-13", "0.53.4", 0, "Go 1.25, wasmd 0.60.5.", "None.", true},
	{"v27.2.0", "2026-04-06", "0.53.4", 0, "Dependency release.", "None.", true},
	{"v27.3.0", "2026-04-30", "0.53.4", 0, "ibc-go 10.6, packet-forward-middleware 10.6, 08-wasm 10.5.", "None. PFM forwards are tagged from the memo.", true},
	{"v27.4.0", "2026-05-21", "0.53.4", 0, "Dependency release.", "None.", true},
	{"v27.5.0", "2026-06-19", "0.53.4", 0, "ibc-go 10.7.", "None.", true},
	{"v27.6.0", "2026-07-17", "0.53.4", 0, "Max wasm contract size 1.6 MiB; contracts can query validators and proposals.", "None.", true},
	{"v28.0.0", "2026-08-07", "0.53.4", 0, "Interchain Security provider module removed.", "Provider (ICS) queries return stubs. No tax rows came from the provider module.", true},
	{"v28.1.1", "2026-09-30", "0.53.4", 0, "CWA-2026-006 response: CosmWasm code upload and instantiation disabled.", "None. Existing contracts kept executing; coverage of execute events is unchanged.", true},
	{"v28.2.1", "2026-10-01", "0.53.4", 33073000, "Re-enables CosmWasm upload and instantiation (Prop 1055). Adds the v28.2.0 upgrade handler.", "None.", true},
	{"v28.3.1", "2026-10-01", "0.53.4", 33086741, "One-time bank balance migration on cosmoshub-4 at height 33,086,741 after the 24.8 hour halt.", "The migration changed balances without transactions. Balance reconstruction treats the halt block as a boundary; no taxable rows are affected.", true},
	{"v29.0.0", "2026-10-09", "0.53.8", 0, "Removes x/params and the legacy ParameterChangeProposal governance route; deletes the deprecated provider store; CometBFT 0.38.25.", "None. The tax layer never read legacy governance parameters. Gov deposits and proposals are parsed from gov v1 and v1beta1 messages, both unchanged.", true},
}

// LatestKnownGaiaRelease is the newest reviewed release.
func LatestKnownGaiaRelease() GaiaRelease {
	return GaiaReleases[len(GaiaReleases)-1]
}

// KnownGaiaRelease looks a release up by tag ("v29.0.0").
func KnownGaiaRelease(tag string) (GaiaRelease, bool) {
	for _, r := range GaiaReleases {
		if r.Version == tag {
			return r, true
		}
	}
	return GaiaRelease{}, false
}

// UpcomingChainChanges records announced changes that are not yet released,
// so a reviewer knows what to watch for.
var UpcomingChainChanges = []string{
	"Cosmos SDK 0.54 or 0.55 is planned for the Hub within weeks of v29. The tax layer depends on event shapes (coin_spent, coin_received, transfer, withdraw_rewards, delegate, unbond, redelegate, wasm) that have been stable since SDK 0.47; the release watcher flags the first release on a new SDK line for review of event attribute names.",
}
