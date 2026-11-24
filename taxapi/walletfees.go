package taxapi

import (
	"errors"
	"time"
)

// feeChargedCondition keeps a fee row unless the chain is recorded as having
// charged nothing for it. A successful transaction paid its declared fee. A
// failed one paid it only when the ante handler passed and a message then
// failed; when the ante handler rejected the transaction (a wrong sequence,
// for one) its state was reverted and nothing was charged.
// tax_failed_tx_fees holds the outcome read from the chain's block results,
// with an empty charged column for nothing charged. The balance ledger applies
// the same rule (ledgerFees).
//
// The chain can include the same bytes twice, a rejected copy beside the copy
// that ran, and the index keeps one row per hash. A failed row with decoded
// messages is such a copy: the indexer stores decoded messages only for a
// transaction that ran, so those bytes paid their fee in another block even
// though the outcome recorded at the stored block is nothing (OPS-821).
const feeChargedCondition = `(txes.code = 0 OR NOT EXISTS (
 SELECT 1 FROM tax_failed_tx_fees uncharged
 WHERE uncharged.tx_id = fees.tx_id AND uncharged.charged = '')
 OR EXISTS (SELECT 1 FROM messages ran WHERE ran.tx_id = fees.tx_id AND NOT ran.opaque))`

// feeOutcomeUnknownColumn marks a failed transaction's fee whose outcome is
// not recorded: the chain may have charged it or nothing. A failed row with
// decoded messages is a copy that ran (see feeChargedCondition) and paid. The
// unique index on txes.hash keeps that payment from appearing twice.
const feeOutcomeUnknownColumn = `(txes.code <> 0 AND NOT EXISTS (
 SELECT 1 FROM tax_failed_tx_fees settled WHERE settled.tx_id = fees.tx_id)
 AND NOT EXISTS (SELECT 1 FROM messages ran WHERE ran.tx_id = fees.tx_id AND NOT ran.opaque)) AS outcome_unknown`

// walletFees loads every payer-side fee the chain charged in [start,end),
// including fees on failed transactions that passed the ante handler and
// transactions with no classified taxable event, and leaving out failed
// transactions recorded as charged nothing. A failed transaction with no
// recorded outcome comes back with OutcomeUnknown set: reports count it as
// nothing and show its declared amount beside the flag (OPS-810).
func (s *Server) walletFees(chain, addr string, start, end time.Time) ([]feeRec, error) {
	if s.readOnlyLedger {
		return s.effectiveWalletFees(chain, addr, start, end)
	}
	start, end = start.UTC(), end.UTC()
	var payer struct{ ID uint }
	res := s.db.Table("addresses").Select("id").Where("address = ?", addr).Scan(&payer)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	// Resolve the unique address before planning the fee query. A join hides
	// heavy payers from PostgreSQL's per-payer cardinality statistics.
	q := s.db.Table("fees").
		Select("fees.amount::text AS amount, denoms.base AS denom, blocks.time_stamp AS ts, txes.hash AS hash, "+feeOutcomeUnknownColumn).
		Joins("JOIN denoms ON denoms.id = fees.denomination_id").
		Joins("JOIN txes ON txes.id = fees.tx_id").
		Joins("JOIN blocks ON blocks.id = txes.block_id").
		Joins("JOIN chains ON chains.id = blocks.chain_id").
		Where("chains.chain_id IN ?", chainIDs(chain)).
		Where("fees.payer_address_id = ? AND blocks.time_stamp >= ? AND blocks.time_stamp < ?", payer.ID, start, end).
		Where(feeChargedCondition)

	// On one chain, consensus timestamps are monotonic with height. Proven day
	// boundaries provide a redundant height range, letting a heavy payer's
	// narrow period use block/transaction indexes instead of lifetime fees.
	// Keep the original timestamp predicates and fall back without height
	// bounds when a boundary is absent, incomplete or not a whole UTC day.
	if start.Equal(start.UTC().Truncate(24*time.Hour)) && end.Equal(end.UTC().Truncate(24*time.Hour)) {
		var ids []int64
		err := s.db.Table("chains").Where("chain_id IN ?", chainIDs(chain)).Pluck("id", &ids).Error
		if err != nil {
			return nil, err
		}
		if len(ids) == 1 {
			lo, err := s.dayEndBlock(ids, start.AddDate(0, 0, -1))
			if err != nil && !feeBoundaryUnavailable(err) {
				return nil, err
			}
			if err == nil {
				hi, err := s.dayEndBlock(ids, end.AddDate(0, 0, -1))
				if err != nil && !feeBoundaryUnavailable(err) {
					return nil, err
				}
				if err == nil {
					q = q.Where("blocks.height > ? AND blocks.height <= ?", lo.Height, hi.Height)
				}
			}
		}
	}
	var fees []feeRec
	err := q.Order("blocks.time_stamp asc, fees.id asc").Scan(&fees).Error
	return fees, err
}

func feeBoundaryUnavailable(err error) bool {
	var incomplete balanceIncomplete
	return errors.Is(err, errBalanceBeforeIndex) || errors.Is(err, errBalanceNotReached) || errors.As(err, &incomplete)
}
