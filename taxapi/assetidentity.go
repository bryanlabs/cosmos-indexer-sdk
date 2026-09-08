package taxapi

// AssetIdentity preserves evidence separately from an optional user treatment.
// An unresolved mismatch is never silently merged into the native ATOM asset.
type AssetIdentity struct {
	ReportedLabel     string `json:"reported_label"`
	TokenName         string `json:"token_name"`
	RawDenom          string `json:"raw_denom"`
	RawAmount         string `json:"raw_amount"`
	SourceChain       string `json:"source_chain"`
	ChannelPath       string `json:"channel_path"`
	BaseToken         string `json:"base_token"`
	OfficialAtomMatch bool   `json:"official_atom_match"`
	Treatment         string `json:"treatment"`
	Note              string `json:"note"`
}

const (
	junoFactoryUatomVoucher = "ibc/3622BC03E5098BF3EC0A2DB13E5031668290B98020C5FADB7901207F44C4D717"
	junoFactoryUatomBase    = "factory/juno1tm748xtl4wmxfn5hqn6r66tzv0csc3qsey04st/uatom"
	junoFactoryUatomSymbol  = "ATOMREWARDS"
)

// This asset's archive denom trace resolves through Cosmos Hub channel-141
// (osmosis-1), then Osmosis channel-42 (juno-1), to the full tokenfactory denom.
// Its subdenom and oracle symbol resemble ATOM, but it is NOT native uatom.
// Juno bank metadata names it ATOMREWARDS with six decimals. That verifies its
// display units, not native ATOM equivalence or a market price. Retain the raw
// denomination, use a distinct identity, and keep the price unknown.
func isUnverifiedJunoFactoryUatom(chain, denom, base string) bool {
	return chain == "mainnet" && (denom == junoFactoryUatomVoucher || base == junoFactoryUatomBase)
}
