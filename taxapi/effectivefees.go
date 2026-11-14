package taxapi

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type effectiveFeeRow struct {
	TxID     uint
	Hash     string
	Code     int64
	Denom    string
	Amount   string
	Ts       time.Time
	Resolved bool
	Charged  string
}

// effectiveWalletFees is used only by the private read-only ledger. Like
// ledgerFees, it attributes grants to their granter and uses actual recorded
// charges for failed transactions. Missing outcomes fail closed, with no node
// fallback or source writes inside the report snapshot.
func (s *Server) effectiveWalletFees(chain, addr string, start, end time.Time) ([]feeRec, error) {
	start, end = start.UTC(), end.UTC()
	var payer struct{ ID uint }
	payerResult := s.db.Table("addresses").Select("id").Where("address = ?", addr).Scan(&payer)
	if payerResult.Error != nil {
		return nil, payerResult.Error
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	args := map[string]any{"payer": payer.ID, "has_payer": payerResult.RowsAffected > 0, "addr": addr, "ids": ids, "start": start, "end": end}
	bounds := ""
	// Keep timestamps authoritative and add only proven whole-UTC-day bounds.
	// Multiple chain aliases or incomplete boundaries retain the time-only path.
	if len(ids) == 1 && start.Equal(start.Truncate(24*time.Hour)) && end.Equal(end.Truncate(24*time.Hour)) {
		lo, e := s.dayEndBlock(ids, start.AddDate(0, 0, -1))
		if e != nil && !feeBoundaryUnavailable(e) {
			return nil, e
		}
		if e == nil {
			hi, e := s.dayEndBlock(ids, end.AddDate(0, 0, -1))
			if e != nil && !feeBoundaryUnavailable(e) {
				return nil, e
			}
			if e == nil {
				args["lo"], args["hi"] = lo.Height, hi.Height
				bounds = " AND b.height > @lo AND b.height <= @hi"
			}
		}
	}
	// Resolve payer ID before planning, preserving per-wallet fee statistics.
	// The grant branch must run even when the granter has no addresses row.
	// EXISTS avoids multiplying rows even if source grant records repeat.
	const projection = `SELECT f.id AS fee_id, x.id AS tx_id, x.hash, x.code,
 d.base AS denom, f.amount::text AS amount, b.time_stamp AS ts,
 o.tx_id IS NOT NULL AS resolved, COALESCE(o.charged, '') AS charged
 FROM fees f JOIN txes x ON x.id=f.tx_id
 JOIN blocks b ON b.id=x.block_id
 JOIN denoms d ON d.id=f.denomination_id
 LEFT JOIN tax_failed_tx_fees o ON o.tx_id=x.id
 WHERE b.chain_id IN @ids AND b.time_stamp >= @start AND b.time_stamp < @end`
	query := `SELECT * FROM (` + projection + bounds + `
 AND @has_payer AND f.payer_address_id=@payer
 AND NOT EXISTS (SELECT 1 FROM tax_fee_grants g WHERE g.tx_id=f.tx_id)
 UNION ALL ` + projection + bounds + `
 AND EXISTS (SELECT 1 FROM tax_fee_grants g WHERE g.tx_id=f.tx_id AND g.granter=@addr)
 ) attributed ORDER BY ts, tx_id, denom, fee_id`
	var rows []effectiveFeeRow
	if err := s.db.Raw(query, args).Scan(&rows).Error; err != nil {
		return nil, err
	}
	var out []feeRec
	failed := make(map[uint]bool)
	for _, r := range rows {
		if r.Code == 0 {
			amount, ok := new(big.Int).SetString(r.Amount, 10)
			if !ok || amount.Sign() < 0 || sdk.ValidateDenom(r.Denom) != nil {
				return nil, fmt.Errorf("invalid effective fee amount or denomination")
			}
			out = append(out, feeRec{Amount: amount.String(), Denom: r.Denom, Ts: r.Ts, Hash: r.Hash})
			continue
		}
		if failed[r.TxID] {
			continue
		}
		failed[r.TxID] = true
		if !r.Resolved {
			return nil, fmt.Errorf("unresolved failed transaction fee")
		}
		coins, err := effectiveChargedCoins(r.Charged)
		if err != nil {
			return nil, err
		}
		for _, c := range coins {
			out = append(out, feeRec{Amount: c.Amount, Denom: c.Denom, Ts: r.Ts, Hash: r.Hash})
		}
	}
	return out, nil
}

// Parse raw integer bank coins, never decimal normalization or implicit native
// denomination. Empty is an explicitly resolved no-charge outcome.
func effectiveChargedCoins(raw string) ([]feeRec, error) {
	if raw == "" {
		return nil, nil
	}
	amounts := make(map[string]*big.Int)
	for _, coin := range strings.Split(raw, ",") {
		i := 0
		for i < len(coin) && coin[i] >= '0' && coin[i] <= '9' {
			i++
		}
		if i == 0 || i == len(coin) || sdk.ValidateDenom(coin[i:]) != nil {
			return nil, fmt.Errorf("invalid recorded failed transaction fee")
		}
		amount, ok := new(big.Int).SetString(coin[:i], 10)
		if !ok {
			return nil, fmt.Errorf("invalid recorded failed transaction fee")
		}
		denom := coin[i:]
		if amounts[denom] == nil {
			amounts[denom] = new(big.Int)
		}
		amounts[denom].Add(amounts[denom], amount)
	}
	var denoms []string
	for denom := range amounts {
		denoms = append(denoms, denom)
	}
	sort.Strings(denoms)
	out := make([]feeRec, 0, len(denoms))
	for _, denom := range denoms {
		out = append(out, feeRec{Amount: amounts[denom].String(), Denom: denom})
	}
	return out, nil
}
