package taxapi

import (
	"fmt"
	"math/big"
)

// earlyStopWindow is the height span of the first window unnamedStopAbove
// reads below the tip. Each further window is twice as wide.
const earlyStopWindow int64 = 100_000

// unattributedReason is the refusal of a date below height h, where the walk
// stops on a staking record no delegator is named for.
func unattributedReason(h int64) string {
	return fmt.Sprintf("a staking event at height %d after this date that does not name its delegator cannot be attributed to one delegator", h)
}

// maxTargetHeight is the highest height among targets[usable].
func maxTargetHeight(targets []heightTime, usable []int) int64 {
	var h int64
	for _, i := range usable {
		h = max(h, targets[i].Height)
	}
	return h
}

// unnamedStopAbove is the height above floor at which rebuildHoldings' walk
// over (lowest, hi] stops on a staking record no delegator is named for, or 0
// when it does not stop above floor. It reads the same candidate messages,
// rows and events as ledgerStakeEvents, in windows from hi down to floor, and
// settles each window's blocks with settleUnnamedStakes, newest first, with
// one archive budget for every window, as the walk spends its own. A block
// is settled only after every window that holds it has been read: its
// unmatched records, the wallet's named events in it and the records
// attributeStakes gave the wallet in it are then all at hand. Each message is
// read once, whole, as in the walk's single read, so attributeStakes decides
// it on the same rows. The walk's stop is its first block, newest first, that
// does not settle; the same blocks are settled here in the same order with
// the same inputs, so a stop found above floor is the walk's stop.
func (s *Server) unnamedStopAbove(ids []int64, addr, operator string, floor, hi int64, node *ledgerNode) (int64, error) {
	var delegationsAt func(int64) (map[string]*big.Int, error)
	if node != nil {
		budget := newArchiveBudget()
		delegationsAt = func(h int64) (map[string]*big.Int, error) { return node.delegationsAt(addr, h, budget) }
	}
	rel, rp := s.effectiveRel()
	msgQuery, blockQuery, redelQuery := s.ledgerQueries(rel)
	seen := map[uint64]bool{}
	unmatched := map[int64][]ledgerEvent{}
	attributed := map[int64][]ledgerEvent{}
	width := earlyStopWindow
	for top := hi; top > floor; width *= 2 {
		bot := max(floor, top-width)
		params := withParams(rp, map[string]any{qpAddr: addr, "op": operator, "types": ledgerMessageTypes, qpIDs: ids, "lo": bot, "hi": top})
		var msgs []uint64
		if err := s.db.Raw(redelQuery, params).Scan(&msgs).Error; err != nil {
			return 0, fmt.Errorf("redelegation messages: %w", err)
		}
		var fresh []uint64
		for _, m := range msgs {
			if !seen[m] {
				seen[m] = true
				fresh = append(fresh, m)
			}
		}
		if len(fresh) > 0 {
			rows, err := s.redelegationRows(fresh, rel, rp)
			if err != nil {
				return 0, err
			}
			got, left := attributeStakes(rows, addr)
			for _, ev := range got {
				attributed[ev.Height] = append(attributed[ev.Height], ev)
			}
			for _, ev := range left {
				unmatched[ev.Height] = append(unmatched[ev.Height], ev)
			}
		}
		var cands []ledgerEvent
		for h, evs := range unmatched {
			if h > bot && h <= top {
				cands = append(cands, evs...)
			}
		}
		if len(cands) > 0 {
			var msgRows, blockRows []ledgerAttrRow
			if err := s.db.Raw(msgQuery, params).Scan(&msgRows).Error; err != nil {
				return 0, fmt.Errorf("message events: %w", err)
			}
			if err := s.db.Raw(blockQuery, withParams(params, map[string]any{"types": ledgerBlockTypes})).Scan(&blockRows).Error; err != nil {
				return 0, fmt.Errorf("block events: %w", err)
			}
			events := append(groupLedgerRows(msgRows), groupLedgerRows(blockRows)...)
			for h, evs := range attributed {
				if h > bot && h <= top {
					events = append(events, evs...)
				}
			}
			if _, stop := settleUnnamedStakes(delegationsAt, addr, operator, s.native.Denom, events, cands); stop > 0 {
				return stop, nil
			}
		}
		top = bot
	}
	return 0, nil
}
