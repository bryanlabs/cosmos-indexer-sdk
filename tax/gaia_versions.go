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
	{"v21.0.1", "2024-11-21", "0.50.9-lsm", 0, "Carry-in release for 2025: SDK LSM fork, ibc-go 8.5.1, ICS 6.3.0 and wasmd 0.53.0.", "Historical LSM uses cosmos.staking and cosmos.distribution namespaces. Historical compatibility review is in progress; this is not a completeness attestation.", false},
	{"v22.0.0", "2025-01-10", "0.50.11-lsm", 24181000, "SDK LSM fork, ibc-go 8.5.2, ICS 6.4.0 and wasmd 0.53.2.", "Legacy LSM namespaces and consumer-denominated reward withdrawals must remain decodable. Full historical review is pending.", false},
	{"v23.0.0", "2025-03-14", "0.50.11-lsm", 25002500, "ibc-go 10.1.0, ICS 7.0.0 and wasmd 0.55.0. The declared SDK 0.50.13 is replaced by the LSM fork.", "IBC v2 appears alongside v1; historical provider messages remain evidence even after the provider module is removed. Full historical review is pending.", false},
	{"v24.0.0", "2025-05-30", "0.50.11-lsm.0.20250523173923-11578258172e", 26131400, "Moves LSM messages to Gaia x/liquid; ibc-go 10.1.1 and ICS 7.0.1.", "Both the old SDK namespaces and gaia.liquid.v1beta1 must be supported across the migration. Full historical review is pending.", false},
	{"v25.0.0", "2025-06-20", "0.53.2", 26440900, "Cosmos SDK 0.53, wasmd 0.60.1. Continues x/liquid introduced in v24.", "Liquid staking message types (/gaia.liquid.v1beta1.*) are parsed for rewards and share records.", true},
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
	{"v28.0.0", "2026-08-07", "0.53.4", 0, "Interchain Security provider module removed.", "Provider (ICS) queries return stubs. Preserve historical provider messages and consumer reward denominations. Module reward allocation is not a second wallet income event; actual reward withdrawals remain classified by distribution events.", true},
	{"v28.1.1", "2026-09-30", "0.53.4", 0, "CWA-2026-006 response: CosmWasm code upload and instantiation disabled.", "None. Existing contracts kept executing; coverage of execute events is unchanged.", true},
	{"v28.2.1", "2026-10-01", "0.53.4", 33073000, "Re-enables CosmWasm upload and instantiation (Prop 1055). Adds the v28.2.0 upgrade handler.", "None.", true},
	{"v28.3.1", "2026-10-01", "0.53.4", 33086741, "One-time bank balance migration on cosmoshub-4 at height 33,086,741 after the 24.8 hour halt.", "The migration changed balances without transactions. Balance reconstruction treats the halt block as a boundary; no taxable rows are affected.", true},
	{"v29.0.0", "2026-10-09", "0.53.8", 33618500, "Removes x/params and the legacy ParameterChangeProposal governance route; deletes the deprecated provider store; CometBFT 0.38.25; Cosmos SDK 0.53.4 to 0.53.8.", "None (reviewed 2026-10-31 after the upgrade). Attribute keys of withdraw_rewards, withdraw_commission, delegate, unbond, redelegate, cancel_unbonding_delegation and proposal_deposit are unchanged in SDK 0.53.8. New withdraw_addr_redirected event: staking messages now fail instead of paying rewards to a blocked withdraw address, and module-driven withdrawals reroute to the owner or the community pool; neither path applies to a wallet that signs its own transactions, and none fired in the first 53,364 blocks. Every height since the upgrade is indexed with no failed blocks, and a taxrepair dry run over those messages matched the stored rows.", true},
	{"v29.0.1", "2026-10-23", "0.53.8", 0, "Binary-only release: Go 1.26.10.", "None. No state or event changes.", true},
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
	"Cosmos SDK 0.54 or 0.55 is not adopted yet: v29.0.1 and gaia main remain on SDK 0.53.8 (checked 2026-10-31). The tax layer depends on event shapes (coin_spent, coin_received, transfer, withdraw_rewards, delegate, unbond, redelegate, proposal_deposit, wasm) that have been stable since SDK 0.47; the release watcher flags the first release on a new SDK line for review of event attribute names.",
	"gaia#4199 (merged 2026-10-29, backport #4200) restores decoding of historical ParameterChangeProposal content for node queries; expect it in a v29 patch release. The indexer decodes proposals with its own codec, so no tax-layer effect is expected.",
}
