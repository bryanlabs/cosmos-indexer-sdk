// taxindexer runs the cosmos-indexer SDK with the tax classification layer
// registered: it indexes a chain generically AND writes TaxableEvent rows for
// the events that matter for Cosmos tax reporting (bank transfers, staking
// rewards, validator commission, IBC in/out). Fees come from the SDK's generic
// Fee model. Run like the base indexer (config.toml or --flags), e.g.:
//
//	tax-indexer index --config config.toml
package main

import (
	"log"

	"github.com/DefiantLabs/cosmos-indexer/cmd"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func main() {
	indexer := cmd.GetBuiltinIndexer()

	// Custom table for classified taxable events (auto-migrated on startup).
	indexer.RegisterCustomModels([]any{&tax.TaxableEvent{}})

	// Gaia's x/liquid (LSM) messages are not in the SDK v0.47 codec this build
	// links, so without these the decoder cannot read them. See tax/gaialiquid.go.
	if err := indexer.RegisterCustomMsgTypesByTypeURLs(tax.GaiaLiquidMsgTypes()); err != nil {
		log.Fatalf("tax-indexer: registering x/liquid message types: %v", err)
	}

	// IBC channel v2 (ibc-go v10) likewise post-dates this build's ibc-go v7.
	// A v2 receive is an inbound transfer, so it has to decode. See tax/ibcv2.go.
	if err := indexer.RegisterCustomMsgTypesByTypeURLs(tax.IBCChannelV2MsgTypes()); err != nil {
		log.Fatalf("tax-indexer: registering IBC channel v2 message types: %v", err)
	}

	// Cosmos Hub's tokenfactory, which reuses the osmosis proto package name.
	// MsgMint credits real balances, so it has to decode. See tax/tokenfactory.go.
	if err := indexer.RegisterCustomMsgTypesByTypeURLs(tax.TokenFactoryMsgTypes()); err != nil {
		log.Fatalf("tax-indexer: registering tokenfactory message types: %v", err)
	}

	// The SDK requires each registered parser to have a globally-unique
	// identifier, so register a distinct instance (same logic) per type URL.
	for _, url := range tax.MessageTypeURLs {
		indexer.RegisterCustomMessageParser(url, &tax.Parser{ID: "tax:" + url})
	}

	// No message-type filter: the SDK still indexes ALL messages/events
	// generically (needed for the completeness/reconciliation guarantee), and
	// our parser layers taxable events on top of the handled types.

	if err := cmd.Execute(); err != nil {
		log.Fatalf("tax-indexer failed: %v", err)
	}
}
