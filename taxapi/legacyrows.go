package taxapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// legacyEventRow is one legacy.taxable_events row.
type legacyEventRow struct {
	ChainID          string
	Height           int64
	Phase            int
	TxIndex          int
	MsgIndex         int
	SubIndex         int
	Category         string
	Amount           string
	Denom            string
	Asset            string
	FromAddr         string
	ToAddr           string
	ValidatorAddress string
	RewardTrigger    string
	Detail           string
	Timestamp        time.Time
	TxHash           string
}

// legacyFeeRow is one charged fee of a legacy transaction.
type legacyFeeRow struct {
	ChainID string
	Fee     string
	Ts      time.Time
	Hash    string
}

// legacyWalletRows reads addr's rows on the eras in reports, in [start, end).
// Events come in ledger order (era, height, phase, tx_index, msg_index,
// sub_index); heights restart on every chain, so height alone never orders
// them. Rows past an era's export height never reached the next chain and
// are left out. A genesis row (phase 0) is read only on the earliest era in
// reports: on a later era it restates coins the earlier era already holds.
func (s *Server) legacyWalletRows(st *legacyEraState, addr string, start, end time.Time) ([]tax.TaxableEvent, []feeRec, error) {
	q := func(table string) string { return fmt.Sprintf("%q.%s", st.schema, table) }
	chains := st.chainIDs()
	var rows []legacyEventRow
	if err := s.db.Raw(`SELECT e.chain_id, e.height, e.phase, e.tx_index, e.msg_index, e.sub_index, e.category, e.amount, e.denom, e.asset,
       e.from_addr, e.to_addr, e.validator_address, e.reward_trigger, e.detail, e."timestamp", e.tx_hash
FROM `+q("taxable_events")+` e JOIN `+q("eras")+` r ON r.chain_id = e.chain_id
WHERE e.chain_id IN ? AND (e.from_addr = ? OR e.to_addr = ?) AND e."timestamp" >= ? AND e."timestamp" < ?
  AND e.height <= r.export_height AND (e.phase <> 0 OR e.chain_id = ?)
ORDER BY r.era, e.height, e.phase, e.tx_index, e.msg_index, e.sub_index`,
		chains, addr, addr, start, end, st.earliest().ChainID).Scan(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("read earlier Hub chain rows: %w", err)
	}
	events := make([]tax.TaxableEvent, 0, len(rows))
	for _, r := range rows {
		events = append(events, tax.TaxableEvent{
			SubIndex: r.SubIndex, Category: r.Category, Amount: r.Amount, Denom: r.Denom, Asset: r.Asset,
			FromAddr: r.FromAddr, ToAddr: r.ToAddr, ValidatorAddress: r.ValidatorAddress, RewardTrigger: r.RewardTrigger,
			Detail: r.Detail, BlockHeight: r.Height, Timestamp: r.Timestamp.UTC(), TxHash: r.TxHash, ChainID: r.ChainID,
		})
	}
	var feeRows []legacyFeeRow
	if err := s.db.Raw(`SELECT t.chain_id, t.fee, b.time AS ts, t.hash
FROM `+q("tx_inclusions")+` t
JOIN `+q("blocks")+` b ON b.chain_id = t.chain_id AND b.height = t.height
JOIN `+q("eras")+` r ON r.chain_id = t.chain_id
WHERE t.chain_id IN ? AND t.fee_payer = ? AND t.fee_charged AND b.time >= ? AND b.time < ? AND t.height <= r.export_height
ORDER BY r.era, t.height, t.tx_index`, chains, addr, start, end).Scan(&feeRows).Error; err != nil {
		return nil, nil, fmt.Errorf("read earlier Hub chain fees: %w", err)
	}
	var fees []feeRec
	for _, f := range feeRows {
		coins, err := parseLegacyFee(f.Fee)
		if err != nil {
			return nil, nil, fmt.Errorf("%s transaction %s: %w", f.ChainID, f.Hash, err)
		}
		for _, c := range coins {
			fees = append(fees, feeRec{Amount: c[0], Denom: c[1], Ts: f.Ts.UTC(), Hash: f.Hash, ChainID: f.ChainID})
		}
	}
	return events, fees, nil
}

// parseLegacyFee splits a stored fee ("5000uatom" or "1uatom,2stake") into
// (amount, denom) pairs. Zero amounts move nothing and are dropped.
func parseLegacyFee(fee string) ([][2]string, error) {
	var out [][2]string
	for _, coin := range strings.Split(fee, ",") {
		coin = strings.TrimSpace(coin)
		if coin == "" {
			continue
		}
		i := 0
		for i < len(coin) && coin[i] >= '0' && coin[i] <= '9' {
			i++
		}
		amount, denom := strings.TrimLeft(coin[:i], "0"), coin[i:]
		if amount == "" {
			continue
		}
		if denom == "" || i == 0 {
			return nil, fmt.Errorf("unreadable fee %q", fee)
		}
		out = append(out, [2]string{amount, denom})
	}
	return out, nil
}

// withLegacyRows puts the earlier chains' rows in front of the cosmoshub-4
// rows. Every earlier chain ended before the next began, so this is ledger
// order across the restarts; within an era the rows keep their stored order.
func withLegacyRows(hub4 walletRows, events []tax.TaxableEvent, fees []feeRec) walletRows {
	out := hub4
	out.stampChain = true
	out.Events = append(append(make([]tax.TaxableEvent, 0, len(events)+len(hub4.Events)), events...), hub4.Events...)
	out.Fees = append(append(make([]feeRec, 0, len(fees)+len(hub4.Fees)), fees...), hub4.Fees...)
	return out
}

// rowBefore orders rows by chain era, then time. Rows of the current chain
// (and every row while no earlier chain is in reports) have rank 4, so for
// them this is the plain time order the engines always used.
func rowBefore(a, b Row) bool {
	if ra, rb := eraRank(a.ChainID), eraRank(b.ChainID); ra != rb {
		return ra < rb
	}
	return a.Time.Before(b.Time)
}

// rowChainID is the chain a row came from, or "" when no earlier chain is in
// reports (rows then carry no chain id, exactly as before eras existed).
func rowChainID(stamp bool, chainID string) string {
	if !stamp {
		return ""
	}
	if chainID == "" {
		return apiHubChainID
	}
	return chainID
}
