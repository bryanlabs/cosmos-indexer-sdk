package taxapi

import (
	"fmt"
	"math/big"
	"sort"
)

// earlyStopWindow is the height span of the first window unnamedStopAbove
// reads below the tip. Each further window is twice as wide.
const earlyStopWindow int64 = 100_000

// unattributedReason is the refusal of a date below height h, where the walk
// stops on a staking record no delegator is named for.
func unattributedReason(h int64) string {
	return fmt.Sprintf("a staking event at height %d after this date that does not name its delegator cannot be attributed to one delegator", h)
}

// earlyDelegationsAt reads addr's delegations for unnamedStopAbove from the
// node, or the archive below its floor, with one archive budget for the whole
// pre-check, as the walk has one for its own.
func earlyDelegationsAt(node *ledgerNode, addr string) func(int64) (map[string]*big.Int, error) {
	if node == nil {
		return nil
	}
	budget := newArchiveBudget()
	return func(h int64) (map[string]*big.Int, error) { return node.delegationsAt(addr, h, budget) }
}

// maxTargetHeight is the highest height among targets[usable].
func maxTargetHeight(targets []heightTime, usable []int) int64 {
	var h int64
	for _, i := range usable {
		h = max(h, targets[i].Height)
	}
	return h
}

// earlyStopMessages is how many candidate messages the first window
// unnamedStopAbove reads below the tip holds, on the wallet's attribute index.
// Each further window holds twice as many.
const earlyStopMessages = 64

// datedMessage is one dating of a candidate message.
type datedMessage struct {
	MessageID uint64
	Height    int64
}

// datedMessagesQuery dates the messages @msgs at their effective inclusions
// in (@lo, @hi] on @ids, newest first, as redelegationRows dates their rows.
func datedMessagesQuery(rel string) string {
	return `SELECT DISTINCT message_id, height FROM (` + datedBranches(`
SELECT m.id AS message_id, b.height
FROM messages m
JOIN txes x ON x.id = m.tx_id
{{BLOCK}}
WHERE m.id IN @msgs AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi {{OWN}}`, rel) + `) u
ORDER BY height DESC, message_id`
}

// unnamedStopAbove is the height above floor at which rebuildHoldings' walk
// over (lowest, hi] stops on a staking record no delegator is named for, or 0
// when it does not stop above floor. It reads the same candidate messages,
// rows and events as ledgerStakeEvents, in windows from hi down to floor, and
// settles each window's blocks with settleUnnamedStakes, newest first, with
// one archive budget for every window (delegationsAt's), as the walk spends
// its own. A block is settled only after every window that holds it has been
// read: its unmatched records, the wallet's named events in it and the
// records attributeStakes gave the wallet in it are then all at hand. Each
// message is read once, whole, as in the walk's single read, so
// attributeStakes decides it on the same rows. The walk's stop is its first
// block, newest first, that does not settle; the same blocks are settled
// here in the same order with the same inputs, so a stop found above floor
// is the walk's stop.
//
// On the wallet's attribute index, finding the candidate messages costs the
// same for any span, so they are found once for (floor, hi] and dated, and
// the windows are cut from their datings: the first holds earlyStopMessages
// of them, each further one twice as many, and a window never ends inside a
// block. Window reads that are driven from the blocks (blockReads) cost what
// the span holds, so those windows are cut by height.
func (s *Server) unnamedStopAbove(ids []int64, addr, operator string, floor, hi int64, delegationsAt func(int64) (map[string]*big.Int, error)) (int64, error) {
	rel, rp := s.effectiveRel()
	msgQuery, blockQuery, redelQuery := s.ledgerQueries(rel)
	base := map[string]any{qpAddr: addr, "op": operator, "types": ledgerMessageTypes, qpIDs: ids}
	var dated []datedMessage
	if !s.blockReads {
		var msgs []uint64
		if err := s.db.Raw(redelQuery, withParams(rp, base, map[string]any{"lo": floor, "hi": hi})).Scan(&msgs).Error; err != nil {
			return 0, fmt.Errorf("redelegation messages: %w", err)
		}
		if err := inChunks(msgs, func(chunk []uint64) error {
			var part []datedMessage
			if err := s.db.Raw(datedMessagesQuery(rel), withParams(rp, base, map[string]any{"msgs": chunk, "lo": floor, "hi": hi})).Scan(&part).Error; err != nil {
				return fmt.Errorf("redelegation message heights: %w", err)
			}
			dated = append(dated, part...)
			return nil
		}); err != nil {
			return 0, err
		}
		sort.Slice(dated, func(i, j int) bool {
			if dated[i].Height != dated[j].Height {
				return dated[i].Height > dated[j].Height
			}
			return dated[i].MessageID < dated[j].MessageID
		})
	}
	seen := map[uint64]bool{}
	unmatched := map[int64][]ledgerEvent{}
	attributed := map[int64][]ledgerEvent{}
	width, count, next := earlyStopWindow, earlyStopMessages, 0
	if s.earlyMessages > 0 {
		count = s.earlyMessages
	}
	for top := hi; top > floor; {
		var bot int64
		var msgs []uint64
		params := withParams(rp, base, map[string]any{"hi": top})
		if s.blockReads {
			bot = max(floor, top-width)
			width *= 2
			if err := s.db.Raw(redelQuery, withParams(params, map[string]any{"lo": bot})).Scan(&msgs).Error; err != nil {
				return 0, fmt.Errorf("redelegation messages: %w", err)
			}
		} else {
			// The window holds the next count datings, and every other
			// dating in the last one's block; it ends at the next dating's
			// block, or at floor after the last.
			end := min(next+count, len(dated))
			for end < len(dated) && end > next && dated[end].Height == dated[end-1].Height {
				end++
			}
			bot = floor
			if end < len(dated) {
				bot = dated[end].Height
			}
			for _, d := range dated[next:end] {
				msgs = append(msgs, d.MessageID)
			}
			next, count = end, count*2
		}
		params = withParams(params, map[string]any{"lo": bot})
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
		// The window's blocks are settled; what was read for them is done.
		for h := range unmatched {
			if h > bot {
				delete(unmatched, h)
			}
		}
		for h := range attributed {
			if h > bot {
				delete(attributed, h)
			}
		}
		top = bot
	}
	return 0, nil
}
