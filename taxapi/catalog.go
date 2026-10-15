package taxapi

import (
	"net/http"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// handleCatalog publishes what the indexer watches for, in one machine
// readable document: categories with their tax meaning, the message types
// parsed, the Hub contracts recognised by name, the trigger vocabulary, the
// user tag vocabulary, the export formats and the Gaia releases reviewed. The
// web "What we track" page and llms.txt render from this.
func (s *Server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"methodology_version": MethodologyVersion,
		"methodology_date":    MethodologyDate,
		"chain":               "cosmoshub-4",
		"native_asset":        s.native,
		"categories":          tax.Catalog(),
		"triggers":            tax.Triggers(),
		"message_types":       tax.MessageTypeURLs,
		"contracts":           tax.KnownContracts,
		"tags":                TagVocabulary(),
		"export_formats": []map[string]string{
			{"format": "koinly", "scope": "taxable rows", "notes": "Label column carries staking, income, cost, gift, lost, airdrop."},
			{"format": "cointracker", "scope": "taxable rows", "notes": "Fixed eight columns; Tag carries staked, payment, airdrop, gift, lost. No description field."},
			{"format": "coinledger", "scope": "taxable rows", "notes": "Universal manual import; Type uses Deposit, Withdrawal, Staking, Income, Airdrop, Gift, Lost."},
			{"format": "cryptotaxcalculator", "scope": "taxable rows plus staking-deposit and staking-withdrawal records", "notes": "Also accepted by Summ. Carries the reference price per unit."},
			{"format": "cryptio", "scope": "taxable rows", "notes": "Enterprise sub-ledger custom CSV."},
			{"format": "bitwave", "scope": "taxable rows", "notes": "Enterprise accounting import."},
			{"format": "generic", "scope": "every row", "notes": "Audit CSV: category, taxable flag, detail JSON, user tag, implied price flag, validator attribution, asset review decisions."},
		},
		"reports": []map[string]string{
			{"path": "/income", "purpose": "Income report: staking rewards, commission, protocol income and tagged receipts, with validator jurisdiction subtotals."},
			{"path": "/8949", "purpose": "IRS Form 8949 disposals, FIFO per asset, per wallet."},
			{"path": "/schedule-d", "purpose": "Schedule D short and long term totals."},
			{"path": "/990t", "purpose": "Form 990-T unrelated business taxable income estimate for retirement account wallets."},
			{"path": "/journal", "purpose": "Double-entry general ledger journal in USD for a controller's books."},
			{"path": "/positions", "purpose": "Open lots and unrealized gain or loss per asset as of a date."},
			{"path": "/validator-statement", "purpose": "A validator operator's statement: commission earned, rewards paid to delegators, unique delegators, by month."},
			{"path": "/balance", "purpose": "Reconstructed balances as of a date."},
			{"path": "/delegator-report", "purpose": "Embeddable per-delegator summary for a validator's website."},
		},
		"gaia_releases":    tax.GaiaReleases,
		"upcoming_changes": tax.UpcomingChainChanges,
	})
}
