package taxapi

import (
	"fmt"
	"reflect"
	"testing"
)

// ledgerEventsOneRead is ledgerEvents as three single statements, the form
// the split read replaced, kept as the reference it must match.
func (s *Server) ledgerEventsOneRead(ids []int64, addr, operator string, lo, hi int64) ([]ledgerEvent, int64, error) {
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
  WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr
  UNION
  SELECT a1.message_event_id FROM message_event_attributes a1
  JOIN message_events me1 ON me1.id = a1.message_event_id
  JOIN message_event_types t1 ON t1.id = me1.message_event_type_id
  WHERE left(a1.value, 100) = left(@op, 100) AND a1.value = @op AND t1.type = 'create_validator')
AND (t.type IN @types OR t.type = 'create_validator')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, me.id, a.index`,
		map[string]any{"addr": addr, "op": operator, "types": ledgerMessageTypes, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&msgRows).Error; err != nil {
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
AND t.type IN @types
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, be.id, a.index`,
		map[string]any{"addr": addr, "types": ledgerBlockTypes, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&blockRows).Error; err != nil {
		return nil, 0, err
	}
	var redelMsgs []uint64
	if err := s.db.Raw(`
SELECT DISTINCT me0.message_id FROM message_event_attributes a0
JOIN message_events me0 ON me0.id = a0.message_event_id
JOIN message_event_types t0 ON t0.id = me0.message_event_type_id
JOIN messages m ON m.id = me0.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr AND t0.type IN ('withdraw_rewards', 'message')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
AND EXISTS (SELECT 1 FROM message_events r JOIN message_event_types rt ON rt.id = r.message_event_type_id
            WHERE r.message_id = me0.message_id AND rt.type = 'redelegate')`,
		map[string]any{"addr": addr, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&redelMsgs).Error; err != nil {
		return nil, 0, err
	}
	redelRows, err := s.redelegationRows(redelMsgs)
	if err != nil {
		return nil, 0, err
	}
	redelegations, unattributed := attributeRedelegations(redelRows, addr)
	events := append(groupLedgerRows(msgRows), groupLedgerRows(blockRows)...)
	return append(events, redelegations...), unattributed, nil
}

// The split read returns exactly what the three single statements return,
// over a wallet spread across many chunks, a height window that cuts both
// ends, a second chain, messages with and without a redelegate event, the
// operator's create_validator and the wallet's address on an event type the
// ledger does not read.
func TestLedgerEventsSplitMatchesOneRead(t *testing.T) {
	db, messages, blocks := bindListFixture(t)
	for _, q := range []string{
		"INSERT INTO chains(id,chain_id) VALUES(2,'other-1')",
		"UPDATE blocks SET chain_id = 2 WHERE id % 97 = 0",
		"INSERT INTO message_event_types(id,type) VALUES(5,'coin_received'),(6,'create_validator'),(7,'delegate')",
		// Every fifth event changes kind, and every third message loses its redelegate event.
		"UPDATE message_events SET message_event_type_id = 5 WHERE id % 5 = 1",
		"UPDATE message_events SET message_event_type_id = 7 WHERE id % 11 = 2",
		"UPDATE message_events SET message_event_type_id = 4 WHERE index = 0 AND message_id % 3 = 0",
		"UPDATE message_events SET message_event_type_id = 6 WHERE id % 1009 = 3",
		// The wallet is the delegator on most events; the operator signs the create_validator events.
		"UPDATE message_event_attributes SET value = 'cosmos1delegator' WHERE index = 2 AND message_event_id % 4 <> 0",
		"UPDATE message_event_attributes SET value = 'cosmosvaloper1operator' WHERE index = 1 AND message_event_id % 1009 = 3",
		"ANALYZE",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}
	defer func(n int) { ledgerReadWorkers = n }(ledgerReadWorkers)
	for _, workers := range []int{1, 2, 8} {
		ledgerReadWorkers = workers
		for _, c := range []struct {
			name   string
			lo, hi int64
		}{
			{"whole history", 0, 2000000},
			{"window", int64(2000000 - blocks + 300), int64(2000000 - 500)},
			{"empty window", 1, 2},
		} {
			t.Run(fmt.Sprintf("%d workers %s", workers, c.name), func(t *testing.T) {
				got, gotUn, err := s.ledgerEvents([]int64{1}, "cosmos1delegator", "cosmosvaloper1operator", c.lo, c.hi)
				if err != nil {
					t.Fatal(err)
				}
				want, wantUn, err := s.ledgerEventsOneRead([]int64{1}, "cosmos1delegator", "cosmosvaloper1operator", c.lo, c.hi)
				if err != nil {
					t.Fatal(err)
				}
				if c.name == "whole history" {
					redelegations := 0
					for _, e := range want {
						if e.Type == "redelegate" {
							redelegations++
						}
					}
					if len(want) < messages || redelegations == 0 {
						t.Fatalf("fixture too small: %d events, %d redelegations", len(want), redelegations)
					}
				}
				if gotUn != wantUn || !reflect.DeepEqual(got, want) {
					t.Fatalf("split read %d events (unattributed %d), one read %d events (unattributed %d), equal %v",
						len(got), gotUn, len(want), wantUn, reflect.DeepEqual(got, want))
				}
			})
		}
	}
}

func TestLedgerWorkersFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 8, "2": 2, "1": 1, "8": 8, "0": 8, "9": 8, "x": 8} {
		if got := ledgerWorkersFromEnv(v); got != want {
			t.Fatalf("ledgerWorkersFromEnv(%q) = %d, want %d", v, got, want)
		}
	}
}
