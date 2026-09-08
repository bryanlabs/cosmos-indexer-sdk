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
}

const (
	MethodologyVersion = "v2"
	MethodologyDate    = "2026-09-08"
)

func currentMethodology() Methodology {
	return Methodology{
		Version:       MethodologyVersion,
		EffectiveDate: MethodologyDate,
		Sources: []string{
			"wasm-indexer oracle /price endpoint (chain-registry + scraped IBC/CosmWasm-token price feeds)",
		},
		TimestampGranularity: "daily (UTC calendar day); every event on a given day for a given denom uses that day's single reference price, not an intraday price",
		AggregationRule:      "one reference USD price per (denom, UTC day), read once from the oracle and cached; every row for that denom/day reuses the same value",
		FallbackOrder: []string{
			"wasm-indexer oracle price for the exact denom and day",
			"documented $1 USD peg for exact allowlisted USDC denominations when the oracle has no price",
			"no synthetic fallback for any other asset: the row is flagged price_missing rather than valued at $0",
		},
		MissingPriceHandling: "rows without a price are flagged price_missing and excluded from USD totals rather than counted as a real zero; report and export summaries surface a count so a gap is never silently understated (see INF-201)",
	}
}
