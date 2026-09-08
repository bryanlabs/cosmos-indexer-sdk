package taxapi

const (
	junoFactoryUatomVoucher = "ibc/3622BC03E5098BF3EC0A2DB13E5031668290B98020C5FADB7901207F44C4D717"
	junoFactoryUatomBase    = "factory/juno1tm748xtl4wmxfn5hqn6r66tzv0csc3qsey04st/uatom"
	junoFactoryUatomSymbol  = "JUNO-TF-uatom"
)

// This asset's archive denom trace resolves through Cosmos Hub channel-141
// (osmosis-1), then Osmosis channel-42 (juno-1), to the full tokenfactory denom.
// Its subdenom and oracle symbol resemble ATOM, but it is NOT native uatom.
// Until independently verified token metadata/prices exist, retain the raw
// denomination, use a distinct identity, and keep amount/price uncertainty.
func isUnverifiedJunoFactoryUatom(chain, denom, base string) bool {
	return chain == "mainnet" && (denom == junoFactoryUatomVoucher || base == junoFactoryUatomBase)
}
