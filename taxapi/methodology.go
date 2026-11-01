package taxapi

// Methodology describes the FMV pricing pipeline in machine-readable form
// (INF-203), so the web methodology page and report footers cite one source
// of truth instead of duplicating this text across repos. Bump
// MethodologyVersion and MethodologyDate together whenever a change here
// would affect a previously-generated report's numbers.
type Methodology struct {
	Version                    string   `json:"version"`
	EffectiveDate              string   `json:"effective_date"`
	Sources                    []string `json:"sources"`
	TimestampGranularity       string   `json:"timestamp_granularity"`
	AggregationRule            string   `json:"aggregation_rule"`
	FallbackOrder              []string `json:"fallback_order"`
	MissingPriceHandling       string   `json:"missing_price_handling"`
	RewardClassification       string   `json:"reward_classification"`
	ValidatorMetadata          string   `json:"validator_metadata"`
	AssetIdentity              string   `json:"asset_identity"`
	ProtocolActivity           string   `json:"protocol_activity"`
	RecordCategories           string   `json:"record_categories"`
	UserTags                   string   `json:"user_tags"`
	ClassificationCompleteness string   `json:"classification_completeness"`
}

const (
	MethodologyVersion = "v9"
	MethodologyDate    = "2026-11-01"
)

func currentMethodology() Methodology {
	return Methodology{
		ClassificationCompleteness: "Unknown contract claims retain observed transfer legs with unresolved income-versus-principal semantics. They are not certified as income or principal. Tax downloads are blocked until classification review; token identity, valuation, spam exclusions and transaction tags cannot clear this gate. Preview CSV marks their taxability unknown. Retained parser and decode errors linked by signer, payer, classified legs or exact message-event addresses block affected wallet/chain/period reports, including cached previews, until successfully reindexed. Errors without retained wallet attribution are visible in chain coverage but cannot establish a wallet-specific result. These gates do not certify archive coverage, prices, basis or off-chain history.",
		AssetIdentity:              "Suspected-spam and token-identity mismatches retain their reported labels, raw denominations, source traces and transaction memos. Names are issuer-controlled display text, not unique asset identity or proof of native ATOM equivalence. Their preview is provisional and tax downloads are blocked until explicit per-record decisions. Users may exclude a receipt as valueless or enter any token ticker, quantity, total USD value, total USD cost basis and acquisition date. Overrides are user-supplied, not oracle-verified, and apply only to that report. Original on-chain records are never changed. Valueless receipts remain visible with explicit zero value and basis in the preview and Generic CSV, with source evidence retained in review audit JSON. For excluded identity mismatches, Generic CSV displays a trimmed non-ATOM issuer token name, or an unverified label with the full raw denomination when the name is empty or claims ATOM/uatom without an official match. They remain excluded from tax calculations and other tax-software imports to prevent automatic pricing.",
		RewardClassification:       "Delegator reward income includes only executed withdraw_rewards events, split by validator and denomination. Delegated, undelegated and redelegated principal is not income and is recorded separately as delegate, undelegate, redelegate and cancel_unbonding records. Auto-withdrawals during staking operations, including both redelegation validators and liquid staking tokenize and redeem operations, are included. Fees are separate.",
		ProtocolActivity:           "CosmWasm protocol activity on Cosmos Hub is classified by contract and action: marketplace, auction and offer settlements are nft_sale; Hydro tributes, DAO reward distributions, governance-score rewards and prizes paid in coins are income with the source named; name registrations, upload fees, game fees and tributes paid are expense; Inflow vault deposits and withdrawals are swap legs between ATOM and vault shares, with the unpriced leg valued from the priced leg of the same transaction (price_implied). Skip Go swaps leaving the Hub keep the memo intent (venue, destination asset, minimum amount, final receiver) on the ibc_out row; routes that only pass through the Hub are tagged skip_forward.",
		RecordCategories:           "Record-only categories (protocol_deposit, protocol_withdrawal, delegate, undelegate, redelegate, cancel_unbonding, tokenize_shares, redeem_shares, share_record, gov_deposit, nft_transfer, nft_burn) describe where the wallet's own assets went. They appear in the preview, the Generic CSV and the journal with taxable=false, and are excluded from tax-software imports and from Form 8949 so they are never read as disposals.",
		UserTags:                   "A user may tag a transaction as internal_transfer, payment, income, gift, airdrop, protocol_deposit, expense, spam or lost. Tags change the label sent to tax software and whether Form 8949 treats the row as a disposal; they never change the on-chain record, and the Generic CSV shows both the classification and the tag.",
		ValidatorMetadata:          "The operator address comes from the withdrawal event. Monikers are current chain REST labels at report generation, not historical names. A validator's jurisdiction is only what its operator account attested on chain (bryanlabs-tax:jurisdiction memo, transaction hash as source); otherwise it is undisclosed. CoinTracker's fixed import schema has no metadata field; use the Generic or Income CSV for validator attribution.",
		Version:                    MethodologyVersion,
		EffectiveDate:              MethodologyDate,
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
		MissingPriceHandling: "rows without a price are flagged price_missing and excluded from USD totals rather than counted as a real zero; report and export summaries surface a count so a gap is never silently understated",
	}
}
