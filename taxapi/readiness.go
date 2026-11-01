package taxapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
)

const unknownClaimSemantics = "unknown_claim_semantics"

// ReportReadiness covers the review and retained parser/decode evidence gates.
// It does not certify archive coverage, prices, basis or off-chain history.
type ReportReadiness struct {
	ExportReady            bool     `json:"export_ready"`
	ParserDecodeComplete   bool     `json:"parser_decode_complete"`
	PendingAssetReviews    int      `json:"pending_asset_reviews"`
	PendingSemanticReviews int      `json:"pending_semantic_reviews"`
	ParserErrors           int64    `json:"parser_errors"`
	DecodeErrors           int64    `json:"decode_errors"`
	OpaqueMessages         int64    `json:"opaque_messages"`
	Scope                  string   `json:"scope"`
	ReviewRequired         []string `json:"review_required,omitempty"`
}

func semanticReviewReason(row Row) string {
	if isIBCRefund(row.RewardTrigger) {
		return ibcRefundBasisRequired
	}
	var detail struct {
		Required string `json:"review_required"`
		Reason   string `json:"review_reason"`
	}
	// The trigger is a fail-closed fallback for legacy/malformed claim detail.
	if row.RewardTrigger == apiUnknownClaim {
		return unknownClaimSemantics
	}
	if json.Unmarshal([]byte(row.Detail), &detail) == nil && detail.Required == apiTrue {
		if detail.Reason != "" {
			return detail.Reason
		}
		return "classification_review_required"
	}
	return ""
}

func previewTaxableText(row Row) string {
	if semanticReviewReason(row) != "" {
		return apiUnknown
	}
	return boolStr(row.isTaxable())
}

func PendingSemanticReviews(rows []Row) int {
	pending := 0
	for _, row := range rows {
		if semanticReviewReason(row) != "" {
			pending++
		}
	}
	return pending
}

func readinessForRows(rows []Row) ReportReadiness {
	assets, semantics := PendingAssetReviews(rows), PendingSemanticReviews(rows)
	reasons := map[string]bool{}
	for _, row := range rows {
		if reason := semanticReviewReason(row); reason != "" {
			reasons[reason] = true
		}
	}
	var required []string
	for reason := range reasons {
		required = append(required, reason)
	}
	sort.Strings(required)
	return ReportReadiness{
		ReviewRequired: required,
		ExportReady:    assets == 0 && semantics == 0, ParserDecodeComplete: true,
		PendingAssetReviews: assets, PendingSemanticReviews: semantics,
		Scope: "requested chain, wallets and period; review and retained parser/decode evidence only, not archive coverage, prices or basis",
	}
}

type reportReadinessRequired struct{ Readiness ReportReadiness }

func (e reportReadinessRequired) Error() string {
	return fmt.Sprintf("report not ready: %d semantic review row(s), %d parser error(s), %d decode error(s), %d opaque message(s). Unknown claim semantics and IBC refund basis reconciliation require classification/basis review, not token identity, price, spam or tag decisions. Incomplete parsing/decoding requires successful reindexing; prior rows are not completeness evidence", e.Readiness.PendingSemanticReviews, e.Readiness.ParserErrors, e.Readiness.DecodeErrors, e.Readiness.OpaqueMessages)
}

// chainIDs uses the same aliases as chainParam, without conflating the Hub's
// separate mainnet and testnet databases if they are queried in one fixture.
func chainIDs(chain string) []string {
	switch chainParam(chain) {
	case apiMainnet:
		return []string{apiHubChainID, apiMainnet, apiCosmoshub}
	case apiTestnet:
		return []string{apiThetaTestnet001, apiProvider, "provider-1", apiTestnet}
	default:
		return []string{chainParam(chain)}
	}
}

func (s *Server) scopedTaxEvents(chain string) *gorm.DB {
	return s.db.Table(apiTaxableEvents).
		Joins("JOIN messages scope_m ON scope_m.id = taxable_events.message_id").
		Joins("JOIN txes scope_t ON scope_t.id = scope_m.tx_id").
		Joins("JOIN blocks scope_b ON scope_b.id = scope_t.block_id").
		Joins("JOIN chains scope_c ON scope_c.id = scope_b.chain_id").
		Where("scope_c.chain_id IN ?", chainIDs(chain))
}

// Errors without wallet attribution do not poison every wallet on the chain.
// Exact retained signer, payer, classified-leg or ordered-event evidence links
// a transaction to a requested wallet, including authz granters and recipients.
// No address substring search, moniker inference or unsupported-type tax guess.
func walletTxScope(q *gorm.DB, addresses []string) *gorm.DB {
	return q.Where(`(
 EXISTS (SELECT 1 FROM tx_signer_addresses tsa JOIN addresses a ON a.id=tsa.address_id WHERE tsa.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM fees f JOIN addresses a ON a.id=f.payer_address_id WHERE f.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM messages wm JOIN taxable_events te ON te.message_id=wm.id WHERE wm.tx_id=t.id AND (te.from_addr IN ? OR te.to_addr IN ? OR te.validator_address IN ?))
 OR EXISTS (SELECT 1 FROM messages wm JOIN message_events me ON me.message_id=wm.id JOIN message_event_attributes mea ON mea.message_event_id=me.id WHERE wm.tx_id=t.id AND mea.value IN ?)
 )`, addresses, addresses, addresses, addresses, addresses, addresses)
}

func (s *Server) classificationReadiness(chain string, addresses []string, start, end time.Time) (ReportReadiness, error) {
	r := readinessForRows(nil)
	base := func() *gorm.DB {
		q := s.db.Table("txes t").Joins("JOIN blocks b ON b.id=t.block_id").Joins("JOIN chains c ON c.id=b.chain_id").
			Where("c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ? AND t.code = 0", chainIDs(chain), start, end)
		if len(addresses) > 0 {
			q = walletTxScope(q, addresses)
		}
		return q
	}
	// Start with the sparse failure sets. Applying wallet event attribution to
	// every successful chain transaction would make long-period gates unbounded.
	if err := base().Joins("JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id) parser_scope ON parser_scope.tx_id=t.id").Count(&r.ParserErrors).Error; err != nil {
		return r, err
	}
	// Failed messages/transactions represent decoding/indexing failures, not
	// ordinary nonzero-code chain transactions, which still legitimately pay fees.
	if err := base().Joins(`JOIN (
 SELECT fm.tx_id FROM failed_messages fm
 UNION SELECT failed_t.id AS tx_id FROM failed_txes ft JOIN txes failed_t ON failed_t.hash=ft.hash AND failed_t.block_id=ft.block_id
 ) decode_scope ON decode_scope.tx_id=t.id`).Count(&r.DecodeErrors).Error; err != nil {
		return r, err
	}
	if err := base().Joins("JOIN (SELECT tx_id FROM messages WHERE opaque = true) opaque_scope ON opaque_scope.tx_id=t.id").Count(&r.OpaqueMessages).Error; err != nil {
		return r, err
	}
	if len(addresses) == 0 {
		// Some transactions failed before txes/signers could be persisted. They
		// are chain-wide coverage gaps, not attributable to an arbitrary wallet.
		var unattributed int64
		if err := s.db.Table("failed_txes ft").Joins("JOIN blocks b ON b.id=ft.block_id").Joins("JOIN chains c ON c.id=b.chain_id").
			Where("c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ?", chainIDs(chain), start, end).
			Where("NOT EXISTS (SELECT 1 FROM txes t WHERE t.hash=ft.hash AND t.block_id=ft.block_id)").Count(&unattributed).Error; err != nil {
			return r, err
		}
		r.DecodeErrors += unattributed
	}
	r.ParserDecodeComplete = r.ParserErrors == 0 && r.DecodeErrors == 0 && r.OpaqueMessages == 0
	r.ExportReady = r.ParserDecodeComplete
	return r, nil
}

func (s *Server) requireClassificationReady(chain string, addresses []string, start, end time.Time) error {
	r, err := s.classificationReadiness(chain, addresses, start, end)
	if err != nil {
		return err
	}
	if !r.ParserDecodeComplete {
		return reportReadinessRequired{r}
	}
	return nil
}
