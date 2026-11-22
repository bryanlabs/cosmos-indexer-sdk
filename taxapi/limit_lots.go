package taxapi

import (
	"encoding/json"
	"sort"
	"strings"

	sdkmath "cosmossdk.io/math"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// limitLotSQL reads a wallet's limit order rows at any time, so a maker fill
// or cancel in the report period finds the order it closes even when the
// wallet placed it before the period.
const limitLotSQL = `WITH address_events AS MATERIALIZED (
 SELECT * FROM taxable_events WHERE from_addr = ? OR to_addr = ?
)
SELECT address_events.* FROM address_events
JOIN messages scope_m ON scope_m.id = address_events.message_id
JOIN txes scope_t ON scope_t.id = scope_m.tx_id
JOIN blocks scope_b ON scope_b.id = scope_t.block_id
JOIN chains scope_c ON scope_c.id = scope_b.chain_id
WHERE address_events.reward_trigger IN ? AND scope_c.chain_id IN ?
ORDER BY address_events.message_id, address_events.sub_index`

var limitLotTriggers = []string{tax.TriggerLimitOrder, tax.TriggerLimitFill, tax.TriggerLimitCancel}

// limitPriceScale is the order book's price unit: quote per base, times 1e12.
var limitPriceScale = sdkmath.NewIntWithDecimal(1, 12)

// limitLegSpan keeps the legs added for one stored row apart from the legs
// of every other row of its message.
const limitLegSpan = 1024

const (
	limitBasisMakerFill  = "maker_fill"
	limitBasisFeeReserve = "fee_reserve"
	reviewLimitLot       = "limit_order_lot_mismatch"
	limitBuy             = "buy"
	limitSell            = "sell"
)

// withLimitMakerLegs completes each maker fill and cancel the wallet
// received in the period from its open limit orders.
func (s *Server) withLimitMakerLegs(chain, addr string, events []tax.TaxableEvent) ([]tax.TaxableEvent, error) {
	need := false
	for _, e := range events {
		if limitMakerFill(e, addr) || limitCancel(e, addr) {
			need = true
			break
		}
	}
	if !need {
		return events, nil
	}
	var lots []tax.TaxableEvent
	if err := s.db.Raw(limitLotSQL, addr, addr, limitLotTriggers, chainIDs(chain)).Scan(&lots).Error; err != nil {
		return nil, err
	}
	return limitMakerLegs(events, addr, lots), nil
}

func limitMakerFill(e tax.TaxableEvent, addr string) bool {
	return e.RewardTrigger == tax.TriggerLimitFill && e.ToAddr == addr && e.FromAddr != addr && detailValue(e.Detail, "maker") == "true"
}

func limitCancel(e tax.TaxableEvent, addr string) bool {
	return e.RewardTrigger == tax.TriggerLimitCancel && e.ToAddr == addr && e.FromAddr != addr
}

type limitLot struct {
	contract, id, side, denom string
	price, open, held         sdkmath.Int
	// placed is the open quantity the deposit was for, filled how much of
	// it later orders filled, and reserve what the deposit held beyond
	// placed at the limit price: the fee reserve of a buy.
	placed, filled, reserve sdkmath.Int
	seq                     int
}

type limitRowKey struct {
	msg uint
	sub int
}

// limitOutcome is what the replay of the wallet's orders made of one stored
// maker fill or cancel.
type limitOutcome struct {
	legs     []tax.TaxableEvent
	orders   []string
	ok       bool
	mismatch bool
}

// limitMakerLegs replays the wallet's limit orders in time order. Each order
// left resting is a lot: its side, limit price, the base quantity still open
// and the coins it holds. A maker fill pays the wallet in the coin the
// matching order spent, so it fills the wallet's open orders of the other
// coin in the book's priority, best price first and then oldest: a buy
// order releases the quote its filled base cost at its price, a sell order
// the base whose price is the quote paid. A buy order's fill also spends
// the filled quantity's share of the order's fee reserve, since that fee
// paid for this trade, and the fill that closes the order spends all it
// still holds. The fill is then a swap paid from those lots. A cancel turns
// the reserve of the cancelled quantity, which no trade paid for, into a
// fee. A fill the wallet's open orders
// do not cover, or a cancel whose refunded quantity is not what the replay
// left open, keeps or gets a review flag and adds no legs.
func limitMakerLegs(events []tax.TaxableEvent, addr string, rows []tax.TaxableEvent) []tax.TaxableEvent {
	rows = append([]tax.TaxableEvent(nil), rows...)
	sort.SliceStable(rows, func(i, j int) bool { return limitBefore(rows[i], rows[j]) })
	lots := map[string]*limitLot{}
	outcomes := map[limitRowKey]limitOutcome{}
	for seq, r := range rows {
		key := limitRowKey{r.MessageID, r.SubIndex}
		switch {
		case r.RewardTrigger == tax.TriggerLimitOrder && r.FromAddr == addr && r.Category == string(tax.CategoryProtocolDeposit):
			if l, ok := newLimitLot(r, seq); ok {
				lots[l.contract+"/"+l.id] = l
			}
		case limitMakerFill(r, addr):
			outcomes[key] = fillLimitLots(r, lots)
		case limitCancel(r, addr):
			outcomes[key] = cancelLimitLot(r, lots)
		}
	}
	out := make([]tax.TaxableEvent, 0, len(events))
	for _, e := range events {
		if !limitMakerFill(e, addr) && !limitCancel(e, addr) {
			out = append(out, e)
			continue
		}
		o, seen := outcomes[limitRowKey{e.MessageID, e.SubIndex}]
		switch {
		case seen && o.ok:
			if limitMakerFill(e, addr) {
				e.Detail = limitDetail(e.Detail, map[string]any{"order_id": strings.Join(o.orders, ",")}, true)
			}
			out = append(out, e)
			for i, l := range o.legs {
				leg := e
				leg.ID, leg.SubIndex = 0, limitLegBase-(e.SubIndex*limitLegSpan+i)
				leg.Category, leg.RewardTrigger, leg.FromAddr, leg.ToAddr = l.Category, l.RewardTrigger, l.FromAddr, l.ToAddr
				leg.Amount, leg.Denom, leg.Asset, leg.Detail = l.Amount, l.Denom, "", l.Detail
				out = append(out, leg)
			}
		case seen && o.mismatch:
			e.Detail = limitDetail(e.Detail, map[string]any{"review_required": "true", "review_reason": reviewLimitLot}, false)
			out = append(out, e)
		default:
			out = append(out, e)
		}
	}
	return out
}

func limitBefore(a, b tax.TaxableEvent) bool {
	switch {
	case !a.Timestamp.Equal(b.Timestamp):
		return a.Timestamp.Before(b.Timestamp)
	case a.BlockHeight != b.BlockHeight:
		return a.BlockHeight < b.BlockHeight
	case a.MessageID != b.MessageID:
		return a.MessageID < b.MessageID
	}
	return a.SubIndex < b.SubIndex
}

func newLimitLot(r tax.TaxableEvent, seq int) (*limitLot, bool) {
	l := &limitLot{contract: r.ToAddr, id: detailValue(r.Detail, "order_id"), side: detailValue(r.Detail, "side"), denom: r.Denom, seq: seq}
	var ok1, ok2, ok3 bool
	l.price, ok1 = sdkmath.NewIntFromString(detailValue(r.Detail, "price"))
	l.open, ok2 = sdkmath.NewIntFromString(detailValue(r.Detail, "open_qty"))
	l.held, ok3 = sdkmath.NewIntFromString(r.Amount)
	if l.id == "" || (l.side != limitBuy && l.side != limitSell) || !ok1 || !ok2 || !ok3 || !l.price.IsPositive() || !l.open.IsPositive() || l.held.IsNegative() {
		return nil, false
	}
	l.placed, l.filled, l.reserve = l.open, sdkmath.ZeroInt(), sdkmath.ZeroInt()
	if l.side == limitBuy {
		l.reserve = l.held.Sub(l.open.Mul(l.price).Quo(limitPriceScale))
		if l.reserve.IsNegative() {
			return nil, false
		}
	}
	return l, true
}

func fillLimitLots(fill tax.TaxableEvent, lots map[string]*limitLot) limitOutcome {
	rem, ok := sdkmath.NewIntFromString(fill.Amount)
	if !ok || !rem.IsPositive() {
		return limitOutcome{}
	}
	var cand []*limitLot
	for _, l := range lots {
		if l.contract == fill.FromAddr && l.open.IsPositive() && l.denom != fill.Denom {
			cand = append(cand, l)
		}
	}
	sort.Slice(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		if !a.price.Equal(b.price) && a.side == b.side {
			if a.side == limitBuy {
				return a.price.GT(b.price)
			}
			return a.price.LT(b.price)
		}
		return a.seq < b.seq
	})
	type step struct {
		l             *limitLot
		take, release sdkmath.Int
	}
	var steps []step
	for _, l := range cand {
		if rem.IsZero() {
			break
		}
		var take, pay, release sdkmath.Int
		if l.side == limitBuy {
			take = sdkmath.MinInt(rem, l.open)
			pay, release = take, take.Mul(l.price).Quo(limitPriceScale).Add(l.reserveShare(take))
			if take.Equal(l.open) {
				release = l.held
			}
		} else {
			full := l.open.Mul(l.price).Quo(limitPriceScale)
			if rem.GTE(full) {
				take, pay = l.open, full
			} else {
				take = rem.Mul(limitPriceScale).Add(l.price).SubRaw(1).Quo(l.price)
				pay = rem
				if !take.Mul(l.price).Quo(limitPriceScale).Equal(rem) {
					return limitOutcome{}
				}
			}
			release = take
		}
		if release.GT(l.held) || !pay.IsPositive() {
			return limitOutcome{}
		}
		steps = append(steps, step{l, take, release})
		rem = rem.Sub(pay)
	}
	if !rem.IsZero() || len(steps) == 0 {
		return limitOutcome{}
	}
	o := limitOutcome{ok: true}
	for _, st := range steps {
		l := st.l
		l.open, l.held, l.filled = l.open.Sub(st.take), l.held.Sub(st.release), l.filled.Add(st.take)
		o.orders = append(o.orders, l.id)
		o.legs = append(o.legs, limitLeg(fill.ToAddr, l, tax.CategorySwap, st.release, limitBasisMakerFill))
		if l.open.IsZero() {
			delete(lots, l.contract+"/"+l.id)
		}
	}
	return o
}

// reserveShare is the part of a buy order's fee reserve that a fill of take
// more base spends: the reserve in proportion to the quantity filled so far,
// less what earlier fills spent, so the shares add up without drift.
func (l *limitLot) reserveShare(take sdkmath.Int) sdkmath.Int {
	if !l.reserve.IsPositive() {
		return sdkmath.ZeroInt()
	}
	upto := func(q sdkmath.Int) sdkmath.Int { return l.reserve.Mul(q).Quo(l.placed) }
	return upto(l.filled.Add(take)).Sub(upto(l.filled))
}

func cancelLimitLot(c tax.TaxableEvent, lots map[string]*limitLot) limitOutcome {
	key := c.FromAddr + "/" + detailValue(c.Detail, "order_id")
	l := lots[key]
	if l == nil {
		return limitOutcome{}
	}
	amt, ok := sdkmath.NewIntFromString(c.Amount)
	if q := detailValue(c.Detail, "refund_qty"); !ok || c.Denom != l.denom || amt.GT(l.held) || (q != "" && q != l.open.String()) {
		return limitOutcome{mismatch: true}
	}
	delete(lots, key)
	o := limitOutcome{ok: true}
	if rest := l.held.Sub(amt); rest.IsPositive() && l.side == limitBuy {
		o.legs = append(o.legs, limitLeg(c.ToAddr, l, tax.CategoryExpense, rest, limitBasisFeeReserve))
	}
	return o
}

// limitLeg is a row the report adds: coins leaving the lot of order l.
// limitMakerLegs places it in the message of the row it completes, with a
// negative sub index, since it is not a stored row.
func limitLeg(addr string, l *limitLot, cat tax.Category, amt sdkmath.Int, basis string) tax.TaxableEvent {
	var leg tax.TaxableEvent
	leg.Category, leg.RewardTrigger = string(cat), tax.TriggerLimitFill
	if cat == tax.CategoryExpense {
		leg.RewardTrigger = tax.TriggerLimitFee
	}
	leg.FromAddr, leg.ToAddr = addr, l.contract
	leg.Amount, leg.Denom, leg.Asset = amt.String(), l.denom, ""
	leg.Detail = limitDetail(`{}`, map[string]any{"contract": l.contract, "order_id": l.id, "lot_basis": basis}, false)
	return leg
}

// limitDetail sets fields on a row's detail and, when clearReview is set, drops its
// review flag.
func limitDetail(detail string, set map[string]any, clearReview bool) string {
	m := map[string]any{}
	if detail != "" && json.Unmarshal([]byte(detail), &m) != nil {
		return detail
	}
	if clearReview {
		delete(m, "review_required")
		delete(m, "review_reason")
	}
	for k, v := range set {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return detail
	}
	return string(b)
}
