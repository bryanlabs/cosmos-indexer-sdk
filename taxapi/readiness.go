package taxapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const unknownClaimSemantics = "unknown_claim_semantics"

// ReportReadiness covers the review and retained parser/decode evidence gates.
// It does not certify archive coverage, prices, basis or off-chain history.
type ReportReadiness struct {
	ExportReady              bool     `json:"export_ready"`
	ParserDecodeComplete     bool     `json:"parser_decode_complete"`
	PendingAssetReviews      int      `json:"pending_asset_reviews"`
	PendingSemanticReviews   int      `json:"pending_semantic_reviews"`
	ParserErrors             int64    `json:"parser_errors"`
	ReclassifiedParserErrors int64    `json:"private_reclassified_parser_errors,omitempty"`
	RemainingParserErrors    int64    `json:"private_remaining_parser_errors"`
	PrivateProfileSHA256     string   `json:"private_profile_sha256,omitempty"`
	DecodeErrors             int64    `json:"decode_errors"`
	OpaqueMessages           int64    `json:"opaque_messages"`
	UnreviewedMessages       int64    `json:"unreviewed_messages"`
	Scope                    string   `json:"scope"`
	ReviewRequired           []string `json:"review_required,omitempty"`
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
	return fmt.Sprintf("report not ready: %d semantic review row(s), %d parser error(s), %d decode error(s), %d opaque message(s), %d unreviewed message type(s). Unknown claim semantics and IBC refund basis reconciliation require classification/basis review, not token identity, price, spam or tag decisions. Incomplete parsing/decoding requires successful reindexing; prior rows are not completeness evidence", e.Readiness.PendingSemanticReviews, e.Readiness.ParserErrors, e.Readiness.DecodeErrors, e.Readiness.OpaqueMessages, e.Readiness.UnreviewedMessages)
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
// The set is found from the addresses, one index lookup per kind of evidence,
// so a full-history scope costs the wallet's history rather than a pass over
// every transaction and event attribute (114s on the live database). The ids
// go through ARRAY() so Postgres collects them first and reads txes by key;
// as IN it misjudged the chain filter and scanned every block and
// transaction (24s per pass for a 34-transaction wallet once 2024 and 2025
// were indexed, against 0.6s this way).
func walletTxScope(q *gorm.DB, addresses []string) *gorm.DB {
	return q.Where(`t.id = ANY(ARRAY(
 SELECT tsa.tx_id FROM tx_signer_addresses tsa JOIN addresses a ON a.id=tsa.address_id WHERE a.address IN ?
 UNION SELECT f.tx_id FROM fees f JOIN addresses a ON a.id=f.payer_address_id WHERE a.address IN ?
 UNION SELECT wm.tx_id FROM taxable_events te JOIN messages wm ON wm.id=te.message_id WHERE te.from_addr IN ? OR te.to_addr IN ? OR te.validator_address IN ?
 UNION SELECT wm.tx_id FROM message_event_attributes mea JOIN message_events me ON me.id=mea.message_event_id JOIN messages wm ON wm.id=me.message_id WHERE left(mea.value, 100) IN ? AND mea.value IN ?
 ))`, addresses, addresses, addresses, addresses, addresses, valuePrefixes(addresses), addresses)
}

// valuePrefixes matches the attribute value index, which is on a prefix
// because values can be long (wasm payloads).
func valuePrefixes(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		if r := []rune(v); len(r) > 100 {
			v = string(r[:100])
		}
		out[i] = v
	}
	return out
}

// walletFewTxScope applies the same attribution to a handful of candidate
// transactions. The message fence reads classified legs by message ID, rather
// than repeating a large wallet-history scan for each candidate.
// OFFSET 0 keeps the event-attribute check a per-transaction index lookup;
// without it Postgres may hash the whole attribute table instead, which took
// 88s on the live database for eight candidates.
func walletFewTxScope(q *gorm.DB, addresses []string) *gorm.DB {
	return q.Where(walletTxCondition(" OFFSET 0"), addresses, addresses, addresses, addresses, addresses, addresses)
}

func walletTxCondition(attributeSuffix string) string {
	return `(
 EXISTS (SELECT 1 FROM tx_signer_addresses tsa JOIN addresses a ON a.id=tsa.address_id WHERE tsa.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM fees f JOIN addresses a ON a.id=f.payer_address_id WHERE f.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM messages wm JOIN LATERAL (SELECT from_addr,to_addr,validator_address FROM taxable_events WHERE message_id=wm.id OFFSET 0) te ON true WHERE wm.tx_id=t.id AND (te.from_addr IN ? OR te.to_addr IN ? OR te.validator_address IN ?))
 OR EXISTS (SELECT 1 FROM messages wm JOIN message_events me ON me.message_id=wm.id JOIN message_event_attributes mea ON mea.message_event_id=me.id WHERE wm.tx_id=t.id AND mea.value IN ?` + attributeSuffix + `)
 )`
}

// ReadinessMigrate creates the indexes that find messages of unreviewed types
// and a wallet's transactions without scanning every row.
func ReadinessMigrate(db *gorm.DB) error {
	for _, sql := range []string{
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_message_type_id ON messages (message_type_id)`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_tx_signer_addresses_address_id ON tx_signer_addresses (address_id)`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_message_event_attr_value ON message_event_attributes (left(value, 100))`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) classificationReadiness(chain string, addresses []string, start, end time.Time) (ReportReadiness, error) {
	r := readinessForRows(nil)
	// Start with the sparse failure sets. Applying wallet event attribution to
	// every successful chain transaction would make long-period gates unbounded.
	// The failure set and period are fenced in a subquery (OFFSET 0) so the
	// wallet checks run only on what is left; otherwise Postgres applies them
	// to every failure-set transaction before it looks at the block time.
	count := func(out *int64, failureSet string, args ...interface{}) error {
		candidates := s.db.Table("txes t").Select("t.id").
			Joins("JOIN blocks b ON b.id=t.block_id").Joins("JOIN chains c ON c.id=b.chain_id").
			Joins(failureSet, args...).
			Where("c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ? AND t.code = 0", chainIDs(chain), start, end)
		q := s.db.Table("(? OFFSET 0) t", candidates)
		if len(addresses) > 0 {
			q = walletFewTxScope(q, addresses)
		}
		return q.Count(out).Error
	}
	if err := count(&r.ParserErrors, "JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id) parser_scope ON parser_scope.tx_id=t.id"); err != nil {
		return r, err
	}
	// Failed messages/transactions represent decoding/indexing failures, not
	// ordinary nonzero-code chain transactions, which still legitimately pay fees.
	if err := count(&r.DecodeErrors, `JOIN (
 SELECT fm.tx_id FROM failed_messages fm
 UNION SELECT failed_t.id AS tx_id FROM failed_txes ft JOIN txes failed_t ON failed_t.hash=ft.hash AND failed_t.block_id=ft.block_id
 ) decode_scope ON decode_scope.tx_id=t.id`); err != nil {
		return r, err
	}
	if err := count(&r.OpaqueMessages, "JOIN (SELECT tx_id FROM messages WHERE opaque = true) opaque_scope ON opaque_scope.tx_id=t.id"); err != nil {
		return r, err
	}
	// A decoded message of a type the tax layer has no decision for may have
	// moved coins. Such types are rare, so find them first and only scan
	// messages when one exists.
	var unreviewed []int64
	if err := s.db.Table("message_types").Where("message_type NOT IN ?", tax.ReviewedTypeURLs()).Pluck("id", &unreviewed).Error; err != nil {
		return r, err
	}
	if len(unreviewed) > 0 {
		if err := count(&r.UnreviewedMessages, "JOIN (SELECT DISTINCT tx_id FROM messages WHERE message_type_id IN ? AND opaque = false) unreviewed_scope ON unreviewed_scope.tx_id=t.id", unreviewed); err != nil {
			return r, err
		}
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
	r.RemainingParserErrors = r.ParserErrors
	if p := s.commissionPrivateProfile; p != nil {
		if !s.readOnlyLedger || s.privateProjectionSnapshot == "" || chainParam(chain) != apiMainnet || start.Before(p.From) || end.After(p.AsOf) || !start.Before(end) {
			return r, fmt.Errorf("private projection scope mismatch")
		}
		var reclassified int64
		if err := count(&reclassified, "JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id WHERE mpe.id=10262 AND m.id=27907375 AND m.tx_id=18802568) parser_scope ON parser_scope.tx_id=t.id"); err != nil {
			return r, err
		}
		if reclassified > 1 || reclassified > r.ParserErrors {
			return r, fmt.Errorf("private projection error cardinality mismatch")
		}
		r.ReclassifiedParserErrors = reclassified
		r.RemainingParserErrors -= reclassified
		r.PrivateProfileSHA256 = p.digest
		if reclassified != 0 && s.privateProjectionReceipts != nil {
			s.privateProjectionReceipts.retain(addresses, start, end, CommissionPrivateProjectionReceipt{ProfileSHA256: p.digest, SourceSHA256: p.SourceSHA256, Snapshot: s.privateProjectionSnapshot, OriginalParserErrors: r.ParserErrors, ReclassifiedParserErrors: reclassified, RemainingParserErrors: r.RemainingParserErrors})
		}
	}
	r.ParserDecodeComplete = r.RemainingParserErrors == 0 && r.DecodeErrors == 0 && r.OpaqueMessages == 0 && r.UnreviewedMessages == 0
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
