package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// refundFixture reads a real cosmoshub-4 refund message (body and its own
// event log, addresses replaced by placeholders).
func refundFixture(t *testing.T, fixture string) (string, []byte, []txlog.LogMessageEvent) {
	t.Helper()
	var f struct {
		MessageType  string                  `json:"message_type"`
		MessageBytes []byte                  `json:"message_bytes"`
		Events       []txlog.LogMessageEvent `json:"events"`
	}
	raw, err := os.ReadFile("../tax/testdata/ibc-refund-passthrough/" + fixture + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.MessageType, f.MessageBytes, f.Events
}

func TestReplayRefundLifecycleOnlyFromStoredBody(t *testing.T) {
	db := scratch(t)
	const id = 1
	stamp := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	// The stored refund row is the one the indexer wrote before the
	// pass-through fix.
	typ, wire, events := refundFixture(t, "pfm-retry-resend")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO blocks VALUES(?,?,?)", []any{id, 19814108, stamp}},
		{"INSERT INTO txes VALUES(?,?,?,0)", []any{id, id, "REFUND"}},
		{"INSERT INTO message_types VALUES(?,?)", []any{id, typ}},
		{"INSERT INTO messages VALUES(?,?,?,0,?)", []any{id, id, id, wire}},
		{"INSERT INTO taxable_events(message_id,sub_index,category,amount,denom,to_addr,block_height,timestamp,tx_hash,reward_trigger) VALUES(?,0,'protocol_withdrawal','10000000','uatom','intermediate',19814108,?,'REFUND','ibc_timeout_refund')", []any{id, stamp}},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, e := range events {
		var typeID, eventID uint
		if err := db.Raw("INSERT INTO message_event_types(type) VALUES(?) RETURNING id", e.Type).Scan(&typeID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Raw("INSERT INTO message_events(message_id,index,message_event_type_id) VALUES(?,?,?) RETURNING id", id, i, typeID).Scan(&eventID).Error; err != nil {
			t.Fatal(err)
		}
		for j, a := range e.Attributes {
			var keyID uint
			if err := db.Raw("INSERT INTO message_event_attribute_keys(key) VALUES(?) RETURNING id", a.Key).Scan(&keyID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO message_event_attributes(message_event_id,index,message_event_attribute_key_id,value) VALUES(?,?,?,?)", eventID, j, keyID, a.Value).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := load(db, 0, id, 100, []uint{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a timeout refund message must be replayable, load got %d", len(rows))
	}
	// With its body, the packet-forward retry is a pass-through: the stored
	// refund row goes.
	plans, skips, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 0 || len(plans) != 1 || len(plans[0].Old) != 1 || len(plans[0].Expected) != 0 {
		t.Fatalf("wrong plan: %+v skips %+v", plans, skips)
	}
	// Without it the message is skipped, never zeroed from its events.
	if err := db.Exec("UPDATE messages SET message_bytes=NULL").Error; err != nil {
		t.Fatal(err)
	}
	rows, err = load(db, 0, id, 100, []uint{id})
	if err != nil {
		t.Fatal(err)
	}
	plans, skips, err = planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 || len(skips) != 1 || skips[0].Reason != skipNoStoredBody {
		t.Fatalf("bodiless refund must be skipped: plans %+v skips %+v", plans, skips)
	}
}
