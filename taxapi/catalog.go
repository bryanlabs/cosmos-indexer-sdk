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
		apiMethodologyVersion: MethodologyVersion,
		"methodology_date":    MethodologyDate,
		apiChain:              apiHubChainID,
		"native_asset":        s.native,
		"categories":          tax.Catalog(),
		"triggers":            tax.Triggers(),
		"message_types":       tax.MessageTypeURLs,
		"contracts":           tax.KnownContracts,
		apiTags:               TagVocabulary(),
		"export_formats": []map[string]string{
			{apiFormat: apiKoinly, apiScope: apiTaxableRows, apiNotes: "Label column carries staking, income, cost, gift, lost, airdrop."},
			{apiFormat: apiCointracker, apiScope: apiTaxableRows, apiNotes: "Fixed eight columns; Tag carries staked, payment, airdrop, gift, lost. No description field."},
			{apiFormat: apiCoinledger, apiScope: apiTaxableRows, apiNotes: "Universal manual import; Type uses Deposit, Withdrawal, Staking, Income, Airdrop, Gift, Lost."},
			{apiFormat: apiCryptotaxcalculator, apiScope: "taxable rows plus staking-deposit and staking-withdrawal records", apiNotes: "Also accepted by Summ. Carries the reference price per unit."},
			{apiFormat: apiCryptio, apiScope: apiTaxableRows, apiNotes: "Enterprise sub-ledger custom CSV."},
			{apiFormat: apiBitwave, apiScope: apiTaxableRows, apiNotes: "Enterprise accounting import."},
			{apiFormat: apiGeneric, apiScope: "every row", apiNotes: "Audit CSV: category, taxable flag, detail JSON, user tag, implied price flag, validator attribution, asset review decisions."},
		},
		"reports": []map[string]string{
			{apiPath: apiIncome, apiPurpose: "Income report: staking rewards, commission, protocol income and tagged receipts, with validator jurisdiction subtotals."},
			{apiPath: api8949Path, apiPurpose: "IRS Form 8949 disposals, FIFO per asset, per wallet."},
			{apiPath: apiScheduleD, apiPurpose: "Schedule D short and long term totals."},
			{apiPath: api990TPath, apiPurpose: "Form 990-T unrelated business taxable income estimate for retirement account wallets."},
			{apiPath: "/journal", apiPurpose: "Double-entry general ledger journal in USD for a controller's books."},
			{apiPath: "/positions", apiPurpose: "Open lots and unrealized gain or loss per asset as of a date."},
			{apiPath: "/validator-statement", apiPurpose: "A validator operator's statement: commission earned, rewards paid to delegators, unique delegators, by month."},
			{apiPath: "/balance", apiPurpose: "Reconstructed balances as of a date."},
			{apiPath: "/delegator-report", apiPurpose: "Embeddable per-delegator summary for a validator's website."},
		},
		"gaia_releases":    tax.GaiaReleases,
		"upcoming_changes": tax.UpcomingChainChanges,
	})
}
