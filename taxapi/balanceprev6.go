package taxapi

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// earlyHubLedger turns on rebuilding past-date balances across the Hub's
// earlier event shapes. Each part stays off until its rebuild has been
// matched against chain state; while it is off, dates that need it are
// refused as before.
type earlyHubLedger struct {
	// transfers rebuilds blocks before Gaia v6 from their transfer events.
	transfers bool
	// unnamedStaking attributes staking events before Gaia v15, which do not
	// name the delegator, from the wallet's own staking rows.
	unnamedStaking bool
}

func earlyHubLedgerFromEnv() earlyHubLedger {
	return earlyHubLedger{
		transfers:      os.Getenv("TAX_BALANCE_PRE_V6_TRANSFERS") == "on",
		unnamedStaking: os.Getenv("TAX_BALANCE_UNNAMED_STAKING") == "on",
	}
}

// SetEarlyHubLedger turns the rebuild before Gaia v6 from transfer events,
// and the attribution of staking events before Gaia v15, on or off.
func (s *Server) SetEarlyHubLedger(transfers, unnamedStaking bool) {
	s.earlyHub = earlyHubLedger{transfers: transfers, unnamedStaking: unnamedStaking}
}

// prev6Events loads the wallet's coin movements in (lo, hi] before Gaia v6:
// the transfer events that name it, the transfer events of every message in
// which it was a bank sender, and the block-level transfer and message events
// that name it. It also returns the highest height of a movement whose share
// for the wallet the events cannot tell.
func (s *Server) prev6Events(ids []int64, addr string, lo, hi int64) ([]ledgerEvent, int64, error) {
	hi = min(hi, int64(hubCoinEventsFrom-1))
	if lo >= hi {
		return nil, 0, nil
	}
	var msgRows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.id IN (
  SELECT a0.message_event_id FROM message_event_attributes a0
  WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr)
AND t.type IN ('transfer', 'message')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, me.id, a.index`,
		map[string]any{"addr": addr, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&msgRows).Error; err != nil {
		return nil, 0, fmt.Errorf("pre-v6 message events: %w", err)
	}
	named := groupLedgerRows(msgRows)
	var senderMsgs []uint64
	seenMsg := map[uint64]bool{}
	for _, ev := range named {
		if ev.Type == "message" && ev.Msg != 0 && !seenMsg[ev.Msg] && hasAttr(ev, "sender", addr) {
			seenMsg[ev.Msg] = true
			senderMsgs = append(senderMsgs, ev.Msg)
		}
	}
	var extra []ledgerEvent
	if err := inChunks(senderMsgs, func(chunk []uint64) error {
		var part []ledgerAttrRow
		if err := s.db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.message_id IN @msgs AND t.type = 'transfer'
ORDER BY b.height, me.id, a.index`,
			map[string]any{"msgs": chunk}).
			Scan(&part).Error; err != nil {
			return fmt.Errorf("pre-v6 sender transfers: %w", err)
		}
		extra = append(extra, groupLedgerRows(part)...)
		return nil
	}); err != nil {
		return nil, 0, err
	}
	var blockRows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT be.id AS event_id, b.height, b.time_stamp, CASE WHEN be.lifecycle_position = 0 THEN 0 ELSE 2 END AS phase, t.type, k.key, a.value
FROM block_events be
JOIN block_event_types t ON t.id = be.block_event_type_id
JOIN blocks b ON b.id = be.block_id
JOIN block_event_attributes a ON a.block_event_id = be.id
JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
WHERE be.id IN (
  SELECT a0.block_event_id FROM block_event_attributes a0
  WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr)
AND t.type IN ('transfer', 'message')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, be.id, a.index`,
		map[string]any{"addr": addr, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&blockRows).Error; err != nil {
		return nil, 0, fmt.Errorf("pre-v6 block events: %w", err)
	}
	events, unknown := prev6Ledger(append(named, extra...), groupLedgerRows(blockRows), addr, s.native.Denom)
	return events, unknown, nil
}

func hasAttr(ev ledgerEvent, key, value string) bool {
	for _, a := range ev.Attrs {
		if a.Key == key && a.Value == value {
			return true
		}
	}
	return false
}

// prev6Ledger reduces the transfer and message events of blocks before Gaia
// v6 to the transfers the walk undoes. SDK 0.42 writes a transfer record
// (recipient, sender, amount) for every send. MultiSend and the other
// InputOutputCoins callers write their outputs without a sender and name each
// input only as a sender on the message event. When the wallet is the only
// sender in such a message, the sender-less outputs are its spend, since the
// inputs equal the outputs; with other senders its share is unknown and the
// height is returned so dates before it are refused. A block-level message
// event naming the wallet as a sender is unknown for the same reason.
func prev6Ledger(msgEvents, blockEvents []ledgerEvent, addr, bondDenom string) ([]ledgerEvent, int64) {
	var out []ledgerEvent
	var unknown int64
	seen := map[uint64]bool{}
	type message struct {
		height    int64
		senders   map[string]bool
		transfers []ledgerEvent
	}
	byMsg := map[uint64]*message{}
	var order []uint64
	for _, ev := range msgEvents {
		if seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		m, ok := byMsg[ev.Msg]
		if !ok {
			m = &message{height: ev.Height, senders: map[string]bool{}}
			byMsg[ev.Msg] = m
			order = append(order, ev.Msg)
		}
		switch ev.Type {
		case "message":
			for _, a := range ev.Attrs {
				if a.Key == "sender" {
					m.senders[a.Value] = true
				}
			}
		case "transfer":
			m.transfers = append(m.transfers, ev)
		}
	}
	for _, id := range order {
		m := byMsg[id]
		var outputs sdk.Coins
		var first *ledgerEvent
		bad := false
		for i := range m.transfers {
			ev := m.transfers[i]
			out = append(out, ev)
			for _, rec := range ev.records() {
				if _, ok := rec["sender"]; ok {
					continue
				}
				coins, err := ledgerCoins(rec["amount"], bondDenom)
				if err != nil {
					bad = true
					continue
				}
				outputs = outputs.Add(coins...)
				if first == nil {
					first = &m.transfers[i]
				}
			}
		}
		if !m.senders[addr] || (first == nil && !bad) {
			continue
		}
		if bad || len(m.senders) != 1 {
			unknown = max(unknown, m.height)
			continue
		}
		out = append(out, ledgerEvent{
			ID: first.ID, Msg: first.Msg, Height: first.Height, Time: first.Time, Phase: first.Phase, Type: "transfer",
			Attrs: []ledgerAttr{{Key: "sender", Value: addr}, {Key: "amount", Value: outputs.String()}},
		})
	}
	for _, ev := range blockEvents {
		switch ev.Type {
		case "transfer":
			out = append(out, ev)
		case "message":
			if hasAttr(ev, "sender", addr) {
				unknown = max(unknown, ev.Height)
			}
		}
	}
	return out, unknown
}

// stakingTaxRow is one staking principal row the classifier wrote from a
// message, which names the delegator on every SDK.
type stakingTaxRow struct {
	MessageID        uint64
	BlockHeight      int64
	Category         string
	Amount           string
	Denom            string
	FromAddr         string
	ToAddr           string
	ValidatorAddress string
	Detail           string
}

// stakingEventType is the module event each staking category records.
var stakingEventType = map[string]string{
	string(tax.CategoryDelegate):        "delegate",
	string(tax.CategoryUndelegate):      "unbond",
	string(tax.CategoryRedelegate):      "redelegate",
	string(tax.CategoryCancelUnbonding): "cancel_unbonding_delegation",
}

func (r stakingTaxRow) owner() string {
	if r.Category == string(tax.CategoryUndelegate) {
		return r.ToAddr
	}
	return r.FromAddr
}

func (r stakingTaxRow) key() (string, bool) {
	typ, ok := stakingEventType[r.Category]
	if !ok {
		return "", false
	}
	amt, ok := sdk.NewIntFromString(r.Amount)
	if !ok || r.Denom == "" {
		return "", false
	}
	coin := sdk.Coins{sdk.NewCoin(r.Denom, amt)}.String()
	if typ != "redelegate" {
		return typ + "|" + r.ValidatorAddress + "|" + coin, true
	}
	var d struct {
		Source string `json:"source_validator"`
		Dest   string `json:"destination_validator"`
	}
	if err := json.Unmarshal([]byte(r.Detail), &d); err != nil || d.Source == "" {
		return "", false
	}
	dst := d.Dest
	if dst == "" {
		dst = r.ValidatorAddress
	}
	return typ + "|" + d.Source + ">" + dst + "|" + coin, true
}

func stakingRecordKey(typ string, rec map[string]string, bondDenom string) (string, bool) {
	coins, err := ledgerCoins(rec["amount"], bondDenom)
	if err != nil || coins.Empty() {
		return "", false
	}
	if typ == "redelegate" {
		return typ + "|" + rec["source_validator"] + ">" + rec["destination_validator"] + "|" + coins.String(), true
	}
	return typ + "|" + rec["validator"] + "|" + coins.String(), true
}

// unnamedStakingEvents loads the wallet's staking events in (lo, hi] before
// Gaia v15, with the delegator added from its staking rows, and the highest
// height of a message whose events and rows do not match.
func (s *Server) unnamedStakingEvents(ids []int64, addr string, lo, hi int64) ([]ledgerEvent, int64, error) {
	hi = min(hi, int64(hubStakingDelegatorFrom-1))
	if lo >= hi {
		return nil, 0, nil
	}
	var rows []stakingTaxRow
	if err := s.db.Raw(`
SELECT te.message_id, te.block_height, te.category, te.amount, te.denom, te.from_addr, te.to_addr, te.validator_address, te.detail
FROM taxable_events te
WHERE te.message_id IN (
  SELECT message_id FROM taxable_events WHERE from_addr = @wallet AND category IN @cats AND block_height > @lo AND block_height <= @hi
  UNION
  SELECT message_id FROM taxable_events WHERE to_addr = @wallet AND category IN @cats AND block_height > @lo AND block_height <= @hi)
AND te.category IN @cats
ORDER BY te.message_id, te.sub_index`,
		map[string]any{"wallet": addr, "cats": stakingCategories, "lo": lo, "hi": hi}).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("staking rows: %w", err)
	}
	var msgs []uint64
	for i, r := range rows {
		if i == 0 || rows[i-1].MessageID != r.MessageID {
			msgs = append(msgs, r.MessageID)
		}
	}
	var evRows []ledgerAttrRow
	if err := inChunks(msgs, func(chunk []uint64) error {
		var part []ledgerAttrRow
		if err := s.db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.message_id IN @msgs AND t.type IN ('delegate', 'unbond', 'redelegate', 'cancel_unbonding_delegation')
AND b.chain_id IN @ids
ORDER BY b.height, me.message_id, me.id, a.index`,
			map[string]any{"msgs": chunk, "ids": ids}).
			Scan(&part).Error; err != nil {
			return fmt.Errorf("staking events: %w", err)
		}
		evRows = append(evRows, part...)
		return nil
	}); err != nil {
		return nil, 0, err
	}
	sort.SliceStable(evRows, func(i, j int) bool {
		if evRows[i].Height != evRows[j].Height {
			return evRows[i].Height < evRows[j].Height
		}
		return evRows[i].MessageID < evRows[j].MessageID
	})
	events, unattributed := attributeUnnamedStaking(groupLedgerRows(evRows), rows, addr, s.native.Denom)
	return events, unattributed, nil
}

// attributeUnnamedStaking gives the wallet its staking events from messages
// whose events do not name the delegator. The classifier wrote one staking
// row per event record from the message itself, naming whose stake moved, so
// a message's records and rows match one to one by type, validators and
// amount. The wallet's rows pick its records; records alike in all three are
// interchangeable, so which of them it takes does not change its holdings.
// A message whose records and rows do not match, or whose events already
// name a delegator, is not attributed, and the highest height of one that
// holds a row of the wallet's is returned so dates before it are refused.
func attributeUnnamedStaking(events []ledgerEvent, rows []stakingTaxRow, addr, bondDenom string) ([]ledgerEvent, int64) {
	type message struct {
		height int64
		owned  bool
		mine   map[string]int
		all    map[string]int
		bad    bool
		events []ledgerEvent
	}
	byMsg := map[uint64]*message{}
	var order []uint64
	get := func(id uint64) *message {
		m, ok := byMsg[id]
		if !ok {
			m = &message{mine: map[string]int{}, all: map[string]int{}}
			byMsg[id] = m
			order = append(order, id)
		}
		return m
	}
	for _, r := range rows {
		m := get(r.MessageID)
		m.height = max(m.height, r.BlockHeight)
		m.owned = m.owned || r.owner() == addr
		k, ok := r.key()
		if !ok {
			m.bad = true
			continue
		}
		m.all[k]++
		if r.owner() == addr {
			m.mine[k]++
		}
	}
	for _, ev := range events {
		m := get(ev.Msg)
		m.height = max(m.height, ev.Height)
		m.events = append(m.events, ev)
	}
	var out []ledgerEvent
	var unattributed int64
	for _, id := range order {
		m := byMsg[id]
		if !m.owned {
			continue
		}
		seen := map[string]int{}
		for _, ev := range m.events {
			for _, rec := range ev.records() {
				k, ok := stakingRecordKey(ev.Type, rec, bondDenom)
				if !ok || rec["delegator"] != "" {
					m.bad = true
					continue
				}
				seen[k]++
			}
		}
		if !m.bad && len(seen) == len(m.all) {
			for k, n := range m.all {
				if seen[k] != n {
					m.bad = true
					break
				}
			}
		} else {
			m.bad = true
		}
		if m.bad {
			unattributed = max(unattributed, m.height)
			continue
		}
		for _, ev := range m.events {
			for _, rec := range ev.records() {
				k, _ := stakingRecordKey(ev.Type, rec, bondDenom)
				if m.mine[k] == 0 {
					continue
				}
				m.mine[k]--
				keys := make([]string, 0, len(rec))
				for key := range rec {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				attrs := make([]ledgerAttr, 0, len(keys)+1)
				for _, key := range keys {
					attrs = append(attrs, ledgerAttr{Key: key, Value: rec[key]})
				}
				attrs = append(attrs, ledgerAttr{Key: "delegator", Value: addr})
				out = append(out, ledgerEvent{ID: ev.ID, Msg: ev.Msg, Height: ev.Height, Time: ev.Time, Phase: ev.Phase, Type: ev.Type, Attrs: attrs})
			}
		}
	}
	return out, unattributed
}
