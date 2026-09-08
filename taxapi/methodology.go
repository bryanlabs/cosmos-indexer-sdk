package taxapi

// Methodology describes the FMV pricing pipeline in machine-readable form
// (INF-203), so the web methodology page and report footers cite one source
// of truth instead of duplicating this text across repos. Bump
// MethodologyVersion and MethodologyDate together whenever a change here
// would affect a previously-generated report's numbers.
type Methodology struct {
	Version              string   `json:"version"`
	EffectiveDate        string   `json:"effective_date"`
	Sources              []string `json:"sources"`
	TimestampGranularity string   `json:"timestamp_granularity"`
	AggregationRule      string   `json:"aggregation_rule"`
	FallbackOrder        []string `json:"fallback_order"`
	MissingPriceHandling string   `json:"missing_price_handling"`
	RewardClassification string   `json:"reward_classification"`
	ValidatorMetadata    string   `json:"validator_metadata"`
	AssetIdentity        string   `json:"asset_identity"`
}

const (
	MethodologyVersion = "v4"
	MethodologyDate    = "2026-09-08"
)

func currentMethodology() Methodology {
	return Methodology{
		AssetIdentity:        "Known Juno tokenfactory voucher ibc/3622BC03E5098BF3EC0A2DB13E5031668290B98020C5FADB7901207F44C4D717 is displayed as JUNO-TF-uatom, not ATOM. Its verified source denom is factory/juno1tm748xtl4wmxfn5hqn6r66tzv0csc3qsey04st/uatom, routed from Juno through Osmosis. No native ATOM price or FIFO identity is used. Display decimals remain an explicit assumption until source metadata is verified.",
		RewardClassification: "Delegator reward income includes only executed withdraw_rewards events, split by validator and denomination. Delegated, undelegated and redelegated principal is not income. Auto-withdrawals during staking operations, including both redelegation validators, are included. Fees are separate.",
		ValidatorMetadata:    "The operator address comes from the withdrawal event. Monikers are current chain REST labels at report generation, not historical names or evidence of validator jurisdiction. CoinTracker's fixed import schema has no metadata field; use the Generic or Income CSV for validator attribution.",
		Version:              MethodologyVersion,
		EffectiveDate:        MethodologyDate,
		Sources: []string{
			"wasm-indexer oracle /price endpoint (chain-registry + scraped IBC/CosmWasm-token price feeds)",
		},
		TimestampGranularity: "daily (UTC calendar day); every event on a given day for a given denom uses that day's single reference price, not an intraday price",
		AggregationRule:      "one reference USD price per (denom, UTC day), read once from the oracle and cached; every row for that denom/day reuses the same value",
		FallbackOrder: []string{
			"asset identity check first: the reviewed Juno tokenfactory uatom voucher is not native ATOM; keep it separately labelled and unpriced pending verified metadata",
			"wasm-indexer oracle price for the exact denom and day",
			"documented $1 USD peg for exact allowlisted USDC denominations when the oracle has no price",
			"no synthetic fallback for any other asset: the row is flagged price_missing rather than valued at $0",
		},
		MissingPriceHandling: "rows without a price are flagged price_missing and excluded from USD totals rather than counted as a real zero; report and export summaries surface a count so a gap is never silently understated (see INF-201)",
	}
}
