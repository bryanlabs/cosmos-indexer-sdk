package taxapi

import (
	"encoding/json"

	sdkmath "cosmossdk.io/math"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// streamLotSQL reads a wallet's streamswap subscription rows at any time, so
// a stream exit in the report period finds the subscription it spends even
// when the wallet subscribed before the period.
const streamLotSQL = `WITH address_events AS MATERIALIZED (
 SELECT * FROM taxable_events WHERE from_addr = ? OR to_addr = ?
)
SELECT address_events.* FROM address_events
JOIN messages scope_m ON scope_m.id = address_events.message_id
JOIN txes scope_t ON scope_t.id = scope_m.tx_id
JOIN blocks scope_b ON scope_b.id = scope_t.block_id
JOIN chains scope_c ON scope_c.id = scope_b.chain_id
WHERE address_events.reward_trigger IN ? AND scope_c.chain_id IN ?
ORDER BY address_events.message_id, address_events.sub_index`

// streamLotTriggers are the rows that put coins into a subscription or take
// them back out.
var streamLotTriggers = []string{tax.TriggerStreamSubscribe, tax.TriggerStreamWithdraw, tax.TriggerStreamRefund}

// withStreamExitLegs completes each stream exit the wallet received in the
// period with the coins its subscription spent, read from the subscription
// lot, so the exit is a swap with both legs.
func (s *Server) withStreamExitLegs(chain, addr string, events []tax.TaxableEvent) ([]tax.TaxableEvent, error) {
	exits := false
	for _, e := range events {
		if e.RewardTrigger == tax.TriggerStreamExit && e.ToAddr == addr {
			exits = true
			break
		}
	}
	if !exits {
		return events, nil
	}
	var lots []tax.TaxableEvent
	if err := s.db.Raw(streamLotSQL, addr, addr, streamLotTriggers, chainIDs(chain)).Scan(&lots).Error; err != nil {
		return nil, err
	}
	return streamExitLegs(events, addr, lots), nil
}

// streamExitLegs adds, after each stream exit the wallet received, the
// swap leg paying for it from the wallet's subscription to that stream:
// what the exit says it spent (streamswap v2), or else what was left of the
// subscription once its withdrawals and the exit's refund came back (v1).
// The exit's review flag is cleared once that leg is written. An exit with
// no subscription row, a subscription in more than one coin, or a spend the
// lot does not cover keeps its review flag.
func streamExitLegs(events []tax.TaxableEvent, addr string, lots []tax.TaxableEvent) []tax.TaxableEvent {
	out := make([]tax.TaxableEvent, 0, len(events))
	for _, e := range events {
		if e.RewardTrigger != tax.TriggerStreamExit || e.ToAddr != addr || e.FromAddr == addr {
			out = append(out, e)
			continue
		}
		leg, ok := streamExitLeg(e, addr, lots)
		if !ok {
			out = append(out, e)
			continue
		}
		e.Detail = streamExitDetail(e.Detail, "")
		out = append(out, e, leg)
	}
	return out
}

func streamExitLeg(exit tax.TaxableEvent, addr string, lots []tax.TaxableEvent) (tax.TaxableEvent, bool) {
	lot := flowStreamLot(exit)
	if lot == "" {
		return tax.TaxableEvent{}, false
	}
	denom := ""
	subscribed, before, all := sdkmath.ZeroInt(), sdkmath.ZeroInt(), sdkmath.ZeroInt()
	for _, r := range lots {
		order := streamOrder(r, exit)
		if flowStreamLot(r) != lot || order > 0 {
			continue
		}
		amt, ok := sdkmath.NewIntFromString(r.Amount)
		if !ok || amt.IsNegative() {
			return tax.TaxableEvent{}, false
		}
		switch {
		case r.RewardTrigger == tax.TriggerStreamSubscribe && r.FromAddr == addr && order < 0:
			if denom != "" && denom != r.Denom {
				return tax.TaxableEvent{}, false
			}
			denom = r.Denom
			subscribed = subscribed.Add(amt)
		case (r.RewardTrigger == tax.TriggerStreamWithdraw || r.RewardTrigger == tax.TriggerStreamRefund) && r.ToAddr == addr:
			if denom != "" && r.Denom != denom {
				continue
			}
			all = all.Add(amt)
			if order < 0 {
				before = before.Add(amt)
			}
		}
	}
	if denom == "" || !subscribed.IsPositive() {
		return tax.TaxableEvent{}, false
	}
	spent, basis := subscribed.Sub(all), "remainder"
	if v := detailValue(exit.Detail, "spent"); v != "" {
		s, ok := sdkmath.NewIntFromString(v)
		if !ok || s.GT(subscribed.Sub(before)) {
			return tax.TaxableEvent{}, false
		}
		spent, basis = s, "spent"
	}
	if !spent.IsPositive() {
		return tax.TaxableEvent{}, false
	}
	leg := exit
	// The leg is not a stored row. A negative sub index keeps its
	// message and sub index key apart from every stored row.
	leg.ID, leg.SubIndex = 0, -1-exit.SubIndex
	leg.Category = string(tax.CategorySwap)
	leg.FromAddr, leg.ToAddr = addr, exit.FromAddr
	leg.Amount, leg.Denom, leg.Asset = spent.String(), denom, ""
	leg.Detail = streamExitDetail(exit.Detail, basis)
	return leg, true
}

// streamOrder places r before (-1), in (0) or after (1) the exit's message.
func streamOrder(r, exit tax.TaxableEvent) int {
	switch {
	case r.Timestamp.Before(exit.Timestamp), r.Timestamp.Equal(exit.Timestamp) && r.MessageID < exit.MessageID:
		return -1
	case r.Timestamp.Equal(exit.Timestamp) && r.MessageID == exit.MessageID:
		return 0
	}
	return 1
}

// streamExitDetail drops the exit's review flag and, for the paying leg,
// names how its amount was read.
func streamExitDetail(detail, basis string) string {
	var m map[string]any
	if json.Unmarshal([]byte(detail), &m) != nil {
		return detail
	}
	delete(m, "review_required")
	delete(m, "review_reason")
	if basis != "" {
		m["lot_basis"] = basis
	}
	b, err := json.Marshal(m)
	if err != nil {
		return detail
	}
	return string(b)
}
