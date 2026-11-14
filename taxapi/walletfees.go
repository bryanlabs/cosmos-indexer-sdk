package taxapi

import (
	"errors"
	"time"
)

// walletFees preserves every payer-side fee in [start,end), including fees on
// failed transactions and transactions with no classified taxable event.
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
		Select("fees.amount::text AS amount, denoms.base AS denom, blocks.time_stamp AS ts, txes.hash AS hash").
		Joins("JOIN denoms ON denoms.id = fees.denomination_id").
		Joins("JOIN txes ON txes.id = fees.tx_id").
		Joins("JOIN blocks ON blocks.id = txes.block_id").
		Joins("JOIN chains ON chains.id = blocks.chain_id").
		Where("chains.chain_id IN ?", chainIDs(chain)).
		Where("fees.payer_address_id = ? AND blocks.time_stamp >= ? AND blocks.time_stamp < ?", payer.ID, start, end)

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
