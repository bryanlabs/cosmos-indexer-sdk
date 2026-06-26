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

	// One parser instance handles every taxable message type.
	parser := &tax.Parser{ID: "tax-parser"}
	for _, url := range tax.MessageTypeURLs {
		indexer.RegisterCustomMessageParser(url, parser)
	}

	// No message-type filter: the SDK still indexes ALL messages/events
	// generically (needed for the completeness/reconciliation guarantee), and
	// our parser layers taxable events on top of the handled types.

	if err := cmd.Execute(); err != nil {
		log.Fatalf("tax-indexer failed: %v", err)
	}
}
