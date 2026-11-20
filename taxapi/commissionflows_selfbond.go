package taxapi

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

const typeURLCreateValidator = "/cosmos.staking.v1beta1.MsgCreateValidator"

// selfBonds is every new validator's self-bond on the chain, by the
// operator's account, read once per loader. The parser has classified a
// self-bond as a delegate row only since 210e7f4, so messages indexed before
// it left the ledger without one, and a trace kept the bond as liquid coin
// that the chain had already staked.
func (l *dbFlowLoader) selfBonds() (map[string][]flowEvent, error) {
	l.mu.Lock()
	if l.bonds != nil {
		v := l.bonds
		l.mu.Unlock()
		return v, nil
	}
	l.mu.Unlock()
	out, err := l.s.flowSelfBonds(l.chain)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.bonds = out
	l.mu.Unlock()
	return out, nil
}

// flowSelfBonds loads the create_validator events of successful
// MsgCreateValidator messages. There are only a few hundred, found by
// message type; the event type has no index and the operator's address
// names far more attributes than its own create_validator, so neither is
// used to find them. Each read is a per-row lookup (LATERAL with OFFSET 0).
func (s *Server) flowSelfBonds(chain string) (map[string][]flowEvent, error) {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	var typeIDs []int64
	if err := s.db.Table("message_types").Where("message_type = ?", typeURLCreateValidator).Pluck("id", &typeIDs).Error; err != nil {
		return nil, fmt.Errorf("self-bond message type: %w", err)
	}
	if len(typeIDs) == 0 {
		return map[string][]flowEvent{}, nil
	}
	var rows []ledgerAttrRow
	if err := s.db.Raw(`
WITH m AS MATERIALIZED (
  SELECT m.id, b.height, b.time_stamp FROM messages m
  JOIN LATERAL (SELECT block_id, code FROM txes WHERE id = m.tx_id OFFSET 0) x ON x.code = 0
  JOIN LATERAL (SELECT height, time_stamp, chain_id FROM blocks WHERE id = x.block_id OFFSET 0) b ON true
  WHERE m.message_type_id = @type AND b.chain_id IN @ids),
ev AS MATERIALIZED (
  SELECT me.id, m.id AS message_id, m.height, m.time_stamp FROM m
  JOIN LATERAL (SELECT id, message_event_type_id FROM message_events WHERE message_id = m.id OFFSET 0) me ON true
  JOIN message_event_types t ON t.id = me.message_event_type_id AND t.type = 'create_validator')
SELECT ev.id AS event_id, ev.message_id, ev.height, ev.time_stamp, 1 AS phase, 'create_validator' AS type, k.key, a.value
FROM ev
JOIN LATERAL (SELECT message_event_attribute_key_id, index, value FROM message_event_attributes WHERE message_event_id = ev.id OFFSET 0) a ON true
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
ORDER BY ev.id, a.index`,
		map[string]any{"type": typeIDs[0], "ids": ids}).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("self-bonds: %w", err)
	}
	return selfBondsFrom(groupLedgerRows(rows), s.native.Denom)
}

// selfBondsFrom reads create_validator events as delegations of the bond
// from the operator's account to its validator. The SDK requires the
// delegator of a new validator's self-bond to be the operator's account.
func selfBondsFrom(events []ledgerEvent, denom string) (map[string][]flowEvent, error) {
	out := map[string][]flowEvent{}
	for _, ev := range events {
		for _, rec := range ev.records() {
			acct := operatorAccount(rec["validator"])
			if acct == "" {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], denom)
			if err != nil {
				return nil, fmt.Errorf("self-bond at height %d: %w", ev.Height, err)
			}
			v := decimal.NewFromBigInt(coins.AmountOf(denom).BigInt(), 0)
			if !v.IsPositive() {
				continue
			}
			out[acct] = append(out[acct], flowEvent{
				Key:      flowKey{Time: ev.Time.UTC(), Height: ev.Height, Message: uint(ev.Msg)},
				Category: flowCatDelegate, Dir: apiOut, Amount: v, From: acct, Validator: rec["validator"],
			})
		}
	}
	return out, nil
}

// withSelfBonds adds to addr's events each of its self-bonds in [start,end)
// whose message has no delegate row in the ledger.
func withSelfBonds(events, bonds []flowEvent, start, end time.Time) []flowEvent {
	booked := map[uint]bool{}
	for _, e := range events {
		if e.Category == flowCatDelegate && e.Key.Message != 0 {
			booked[e.Key.Message] = true
		}
	}
	added := false
	for _, b := range bonds {
		if b.Key.Time.Before(start) || !b.Key.Time.Before(end) || booked[b.Key.Message] {
			continue
		}
		events = append(events, b)
		added = true
	}
	if added {
		sort.SliceStable(events, func(i, j int) bool { return events[i].Key.before(events[j].Key) })
	}
	return events
}
