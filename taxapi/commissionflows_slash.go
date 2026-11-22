package taxapi

import (
	"fmt"
	"strings"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/shopspring/decimal"
)

func (l *dbFlowLoader) slashes(start, end time.Time) ([]flowEvent, error) {
	key := [2]time.Time{start, end}
	l.mu.Lock()
	if v, ok := l.slashed[key]; ok {
		l.mu.Unlock()
		return v, nil
	}
	l.mu.Unlock()
	out, err := flowRead(func() ([]flowEvent, error) { return l.s.flowSlashes(l.chain, l.node, start, end) })
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.slashed == nil {
		l.slashed = map[[2]time.Time][]flowEvent{}
	}
	l.slashed[key] = out
	l.mu.Unlock()
	return out, nil
}

// flowSlashes loads validator slashes in [start, end) as flow events. Slash
// events are found by their reason attribute, which the attribute value
// index covers, as ledgerSlashes does. The fraction a slash took from every
// delegation is the tokens it burned from the validator over the validator's
// tokens at the infraction, which the event gives as power. Events from
// before the chain emitted burned_coins take the slashing parameters.
func (s *Server) flowSlashes(chain string, node *ledgerNode, start, end time.Time) ([]flowEvent, error) {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	reasons := []string{"missing_signature", "double_sign"}
	var rows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT be.id AS event_id, b.height, b.time_stamp, 0 AS phase, t.type, k.key, a.value
FROM block_events be
JOIN block_event_types t ON t.id = be.block_event_type_id
JOIN blocks b ON b.id = be.block_id
JOIN block_event_attributes a ON a.block_event_id = be.id
JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
WHERE be.id IN (
  SELECT a0.block_event_id FROM block_event_attributes a0
  WHERE left(a0.value, 100) IN @reasons AND a0.value IN @reasons)
AND t.type = 'slash' AND b.chain_id IN @ids AND b.time_stamp >= @start AND b.time_stamp < @end
ORDER BY b.height, be.id, a.index`,
		map[string]any{"reasons": reasons, "ids": ids, "start": start, "end": end}).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("slashes: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var known []ValidatorConsensus
	if err := s.db.Find(&known).Error; err != nil {
		return nil, fmt.Errorf("recorded validators: %w", err)
	}
	cons := map[string]string{}
	for _, k := range known {
		cons[k.ConsAddress] = k.Operator
	}
	var params *[2]decimal.Decimal
	var out []flowEvent
	for _, ev := range groupLedgerRows(rows) {
		for _, rec := range ev.records() {
			op, ok := cons[rec["address"]]
			if !ok && node != nil {
				h, err := node.latestHeight()
				if err == nil {
					var live map[string]string
					if live, err = s.consensusOperators(node, h); err == nil {
						cons = live
						op, ok = cons[rec["address"]]
					}
				}
				if err != nil {
					return nil, fmt.Errorf("slashed validator operators: %w", err)
				}
			}
			if !ok {
				return nil, fmt.Errorf("slashed validator at height %d cannot be matched to an operator", ev.Height)
			}
			doubleSign := rec["reason"] == "double_sign"
			power, err := decimal.NewFromString(rec["power"])
			if err != nil || !power.IsPositive() {
				return nil, fmt.Errorf("slash at height %d has no power", ev.Height)
			}
			tokens := power.Mul(decimal.NewFromBigInt(sdk.DefaultPowerReduction.BigInt(), 0))
			var burned, fraction decimal.Decimal
			if v, has := rec["burned_coins"]; has {
				if burned, err = decimal.NewFromString(strings.TrimSuffix(v, s.native.Denom)); err != nil {
					return nil, fmt.Errorf("slash at height %d: burned coins: %w", ev.Height, err)
				}
				fraction = burned.DivRound(tokens, flowProRataPrecision*2)
			} else {
				if params == nil {
					if node == nil {
						return nil, fmt.Errorf("slash at height %d names no burned coins and no node gives the slashing parameters", ev.Height)
					}
					h, err := node.latestHeight()
					if err != nil {
						return nil, fmt.Errorf("slashing params: %w", err)
					}
					downtime, double, err := node.slashFractions(h)
					if err != nil {
						return nil, fmt.Errorf("slashing params: %w", err)
					}
					params = &[2]decimal.Decimal{downtime, double}
				}
				fraction = params[0]
				if doubleSign {
					fraction = params[1]
				}
				burned = tokens.Mul(fraction).Floor()
			}
			if !fraction.IsPositive() || !burned.IsPositive() {
				continue
			}
			if fraction.GreaterThan(decimal.NewFromInt(1)) {
				return nil, fmt.Errorf("slash at height %d burned more than the validator's power", ev.Height)
			}
			out = append(out, flowEvent{
				Key:      flowKey{Time: ev.Time.UTC(), Height: ev.Height, Sub: flowSlashSub},
				Category: flowCatSlash, Dir: apiOut, Amount: burned, Validator: op,
				Fraction: fraction, DoubleSign: doubleSign,
			})
		}
	}
	return out, nil
}
