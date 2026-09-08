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
	MethodologyVersion = "v5"
	MethodologyDate    = "2026-09-08"
)

func currentMethodology() Methodology {
	return Methodology{
		AssetIdentity:        "Suspected-spam and token-identity mismatches retain their reported labels, raw denominations, source traces and transaction memos. Their preview is provisional and tax downloads are blocked until explicit per-record decisions. Users may exclude a receipt as valueless or enter any token ticker, quantity, total USD value, total USD cost basis and acquisition date. Overrides are user-supplied, not oracle-verified, and apply only to that report. Original on-chain records are never changed. Excluded receipts remain in preview and review audit JSON.",
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
			"asset review first: suspected-spam receipts remain unreviewed until the user explicitly excludes them or supplies token identity, quantity, value and basis; no default exclusion or override",
			"for an explicit override, use the user's exact total USD value and basis with their chosen token and acquisition date, not a guessed native ATOM price",
			"wasm-indexer oracle price for the exact denom and day",
			"documented $1 USD peg for exact allowlisted USDC denominations when the oracle has no price",
			"no synthetic fallback for any other asset: the row is flagged price_missing rather than valued at $0",
		},
		MissingPriceHandling: "rows without a price are flagged price_missing and excluded from USD totals rather than counted as a real zero; report and export summaries surface a count so a gap is never silently understated (see INF-201)",
	}
}
