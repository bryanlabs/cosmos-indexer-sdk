package taxapi

import (
	"fmt"
	"strings"
	"time"
)

// eraAtDayEnd is the earlier Hub chain whose state stands at the end of day
// d: the latest era that started before the next day. Every Hub restart
// happened inside one UTC day, so on a restart day it is the new chain. ok
// is false on cosmoshub-4 days and before the first chain.
func (st *legacyEraState) eraAtDayEnd(d time.Time) (legacyEra, bool) {
	if st == nil || !utcDay(d).Before(hubGenesisDate) {
		return legacyEra{}, false
	}
	cutoff := utcDay(d).AddDate(0, 0, 1)
	var at legacyEra
	found := false
	for _, e := range st.eras {
		if !e.Start.IsZero() && e.Start.Before(cutoff) {
			at, found = e, true
		}
	}
	return at, found
}

// legacyBalanceDay fills hb from legacy.balance_days for a day an earlier
// Hub chain is in reports. The builder writes a row only on a day the
// holdings change, so the day's holdings are the latest row on or before it;
// an address with no row by then held nothing on that chain. A wallet whose
// history on the chain did not reconcile with the next genesis file is
// refused rather than shown.
func (s *Server) legacyBalanceDay(chain string, meta map[string]DenomMeta, st *legacyEraState, e legacyEra, addr string, d time.Time, hb *HistoricalBalance) error {
	q := func(table string) string { return fmt.Sprintf("%q.%s", st.schema, table) }
	hb.ChainID = e.ChainID
	if !e.HasBalances {
		hb.Status, hb.Reason = balanceIncompleteS, fmt.Sprintf("balances on %s are still being built", e.ChainID)
		return nil
	}
	var open []string
	if err := s.db.Raw("SELECT component || ': ' || status FROM "+q("reconciliation")+" WHERE chain_id = ? AND address = ? AND status NOT LIKE 'closed%' ORDER BY component", e.ChainID, addr).Scan(&open).Error; err != nil {
		return err
	}
	if len(open) > 0 {
		hb.Status, hb.Reason = balanceIncompleteS, fmt.Sprintf("this wallet's %s history does not reconcile with the next chain's genesis file (%s), so its balances on that chain are not shown", e.ChainID, strings.Join(open, ", "))
		return nil
	}
	ht, err := s.legacyDayEndBlock(q("blocks"), e, d)
	if err != nil {
		return err
	}
	var rows []struct{ Holdings string }
	if err := s.db.Raw("SELECT holdings::text AS holdings FROM "+q("balance_days")+" WHERE chain_id = ? AND address = ? AND day <= ? ORDER BY day DESC LIMIT 1", e.ChainID, addr, utcDay(d).Format(apiDateLayout)).Scan(&rows).Error; err != nil {
		return err
	}
	stored := "{}"
	if len(rows) > 0 {
		stored = rows[0].Holdings
	}
	h, err := decodeHoldings(ht.Height, stored)
	if err != nil {
		return fmt.Errorf("stored %s balance on %s: %w", e.ChainID, utcDay(d).Format(apiDateLayout), err)
	}
	bt := ht.TimeStamp.UTC()
	hb.Height, hb.BlockTime, hb.AnchorHeight = ht.Height, &bt, e.FirstHeight
	s.fillBalanceLines(chain, meta, hb, h, d)
	return nil
}

// legacyDayEndBlock is the last block of e at or before the end of day d.
func (s *Server) legacyDayEndBlock(blocks string, e legacyEra, d time.Time) (heightTime, error) {
	cutoff := utcDay(d).AddDate(0, 0, 1)
	if e.End.Before(cutoff) {
		return heightTime{Height: e.ExportHeight, TimeStamp: e.End}, nil
	}
	at := func(h int64) (time.Time, error) {
		var ts []time.Time
		if err := s.db.Raw("SELECT time FROM "+blocks+" WHERE chain_id = ? AND height = ?", e.ChainID, h).Scan(&ts).Error; err != nil {
			return time.Time{}, err
		}
		if len(ts) == 0 {
			return time.Time{}, fmt.Errorf("%s block %d is not loaded", e.ChainID, h)
		}
		return ts[0], nil
	}
	// Smallest height at or after the cutoff; the block before it ends the day.
	l, r := e.FirstHeight, e.ExportHeight
	for l < r {
		mid := l + (r-l)/2
		t, err := at(mid)
		if err != nil {
			return heightTime{}, err
		}
		if !t.Before(cutoff) {
			r = mid
		} else {
			l = mid + 1
		}
	}
	h := l - 1
	t, err := at(h)
	if err != nil {
		return heightTime{}, err
	}
	return heightTime{Height: h, TimeStamp: t}, nil
}
