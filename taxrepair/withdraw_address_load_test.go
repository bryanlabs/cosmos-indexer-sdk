package main

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"gorm.io/gorm"
)

const (
	sqlSeedBlock    = "INSERT INTO blocks VALUES(?,?,?)"
	sqlSeedTx       = "INSERT INTO txes VALUES(?,?,?,0)"
	sqlSeedType     = "INSERT INTO message_types VALUES(?,?)"
	sqlSeedMessage  = "INSERT INTO messages VALUES(?,?,?,0,?)"
	attrMsgIndex    = "msg_index"
	waLoadDelegator = "cosmos1loaddelegatorxxxxxxxxxxxxxxxxxxxxxxxxxx"
	waLoadWithdraw  = "cosmos1loadwithdrawxxxxxxxxxxxxxxxxxxxxxxxxxxx"
)

// seedWaMessage stores a claim whose 627uatom reward the module paid to
// recipient, with the reward row the indexer booked. Event types and keys
// are unique by name, as in production, but numbered differently.
func seedWaMessage(t *testing.T, db *gorm.DB, id uint, recipient string) {
	t.Helper()
	stamp := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{sqlSeedBlock, []any{id, 24000000 + id, stamp}},
		{sqlSeedTx, []any{id, id, "HASH" + string(rune('A'+id%26))}},
		{"INSERT INTO message_types VALUES(?,?) ON CONFLICT DO NOTHING", []any{1, waClaimURL}},
		{"INSERT INTO messages VALUES(?,?,1,0,NULL)", []any{id, id}},
		{
			"INSERT INTO taxable_events(message_id,sub_index,category,amount,denom,asset,from_addr,to_addr,block_height,timestamp,tx_hash,validator_address,reward_trigger,detail) VALUES(?,0,'reward','627','uatom','','',?,?,?,?,?,'claim','')",
			[]any{id, waLoadDelegator, 24000000 + id, stamp, "HASH" + string(rune('A'+id%26)), waValidator},
		},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	log := []txlog.LogMessageEvent{
		waEvent("message", "action", waClaimURL, "sender", waLoadDelegator, "module", "distribution"),
		waEvent("transfer", "recipient", recipient, "sender", waDistribution, "amount", "627uatom"),
		waEvent("withdraw_rewards", "amount", "627uatom", "validator", waValidator, "delegator", waLoadDelegator),
	}
	seedWaEvents(t, db, id, log)
}

// seedWaEvents stores a message's event log with its event types and keys.
func seedWaEvents(t *testing.T, db *gorm.DB, id uint, log []txlog.LogMessageEvent) {
	t.Helper()
	for i, e := range log {
		var typeID, eventID uint
		if err := db.Raw("INSERT INTO message_event_types(type) VALUES(?) ON CONFLICT (type) DO UPDATE SET type = excluded.type RETURNING id", e.Type).Scan(&typeID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Raw("INSERT INTO message_events(message_id,index,message_event_type_id) VALUES(?,?,?) RETURNING id", id, i, typeID).Scan(&eventID).Error; err != nil {
			t.Fatal(err)
		}
		for j, a := range e.Attributes {
			var keyID uint
			if err := db.Raw("INSERT INTO message_event_attribute_keys(key) VALUES(?) ON CONFLICT (key) DO UPDATE SET key = excluded.key RETURNING id", a.Key).Scan(&keyID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO message_event_attributes(message_event_id,index,message_event_attribute_key_id,value) VALUES(?,?,?,?)", eventID, j, keyID, a.Value).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
}

func waLoadScratch(t *testing.T) *gorm.DB {
	t.Helper()
	db := scratch(t)
	// Unique names and the event indexes as in production, and ids that are
	// not production's.
	for _, q := range []string{
		"CREATE UNIQUE INDEX ON message_event_types(type)",
		"CREATE UNIQUE INDEX ON message_event_attribute_keys(key)",
		`CREATE UNIQUE INDEX "messageEventIndex" ON message_events(message_id, index)`,
		`CREATE UNIQUE INDEX "messageAttributeIndex" ON message_event_attributes(message_event_id, index)`,
		"INSERT INTO message_event_types(type) VALUES ('coin_spent'), ('coin_received'), ('burn')",
		"INSERT INTO message_event_attribute_keys(key) VALUES ('spender'), ('receiver'), ('memo'), ('fee')",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func waRowCount(t *testing.T, db *gorm.DB, where string) int {
	t.Helper()
	var n int
	if err := db.Raw("SELECT count(*) FROM taxable_events WHERE " + where).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func writeWaPlan(t *testing.T, db *gorm.DB, ids []uint) string {
	t.Helper()
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	plans, counts, err := planWithdrawAddressBatch(db, codec, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if counts[waExact] != len(ids) {
		t.Fatalf("plan classes %v", counts)
	}
	var b strings.Builder
	for _, p := range plans {
		b.WriteString(waPlanLine(p))
	}
	path := filepath.Join(t.TempDir(), "plan.tsv")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The loader on a real Postgres: ids resolved by name, a dry run that
// writes nothing, a read-only check, parts in their own transactions, the
// moves inserted exactly as planned, and a rerun that skips them.
func TestWithdrawAddressLoadIntegration(t *testing.T) {
	db := waLoadScratch(t)
	for _, id := range []uint{10, 11, 12} {
		seedWaMessage(t, db, id, waLoadWithdraw)
	}
	path := writeWaPlan(t, db, []uint{10, 11, 12})
	sum, parts, err := readWithdrawAddressPlan(path, 2)
	if err != nil || len(parts) != 2 || parts[0].First != 10 || parts[0].Last != 11 || parts[1].First != 12 || len(sum) != 64 {
		t.Fatalf("parts %+v %v", parts, err)
	}
	ids, err := resolveWaIDs(db)
	if err != nil || ids.Transfer < 4 || ids.Recipient < 5 {
		t.Fatalf("ids %+v %v", ids, err)
	}

	// The read-only check runs in a read-only transaction and finds nothing
	// to refuse.
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
			return err
		}
		for _, p := range parts {
			v, err := checkWaPart(tx, p, ids)
			if err != nil {
				return err
			}
			if v.failures() != 0 || v.Moved != 0 || v.Rows != len(p.Rows) {
				t.Fatalf("check %+v", v)
			}
		}
		text, err := explainWaPart(tx, parts[0], ids)
		if err != nil || !strings.Contains(text, "taxable_events") {
			t.Fatalf("explain %v\n%s", err, text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without the event index the check plans a sequential scan on
	// message_events, and the guard refuses it.
	errGuard := errors.New("rolled back")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DROP INDEX "messageEventIndex"`).Error; err != nil {
			return err
		}
		if _, err := explainWaPart(tx, parts[0], ids); err == nil || !strings.Contains(err.Error(), "Seq Scan on message_events") {
			t.Fatalf("explain guard without the index: %v", err)
		}
		return errGuard
	})
	if !errors.Is(err, errGuard) {
		t.Fatal(err)
	}

	// A dry run checks, inserts and post-checks, then rolls back.
	for _, p := range parts {
		if _, n, err := loadWaPart(db, p, false); err != nil || n != len(p.Rows) {
			t.Fatalf("dry run part %d: %d %v", p.N, n, err)
		}
	}
	if n := waRowCount(t, db, "reward_trigger = 'withdraw_address'"); n != 0 {
		t.Fatalf("dry run left %d rows", n)
	}

	// Apply: one move per message, after the reward, as planned.
	for _, p := range parts {
		if _, n, err := loadWaPart(db, p, true); err != nil || n != len(p.Rows) {
			t.Fatalf("apply part %d: %d %v", p.N, n, err)
		}
	}
	if n := waRowCount(t, db, "reward_trigger = 'withdraw_address' AND sub_index = 1 AND from_addr = '"+waLoadDelegator+"' AND to_addr = '"+waLoadWithdraw+"' AND amount = '627' AND denom = 'uatom' AND detail = '{\"protocol\":\"distribution\",\"reward_trigger\":\"claim\"}'"); n != 3 {
		t.Fatalf("%d moves", n)
	}

	// A rerun skips every message that already holds its move.
	for _, p := range parts {
		v, n, err := loadWaPart(db, p, true)
		if err != nil || n != 0 || v.Moved != len(waMessageIDs(p.Rows)) {
			t.Fatalf("rerun part %d: %+v %d %v", p.N, v, n, err)
		}
	}
	if n := waRowCount(t, db, "true"); n != 6 {
		t.Fatalf("%d rows after rerun", n)
	}
}

// A message whose stored rows changed since the plan, a planned move the
// stored events do not pay, and a plan file edited after the dry run are
// all refused, and nothing in the refused part is written.
func TestWithdrawAddressLoadRefusals(t *testing.T) {
	db := waLoadScratch(t)
	for _, id := range []uint{20, 21} {
		seedWaMessage(t, db, id, waLoadWithdraw)
	}
	path := writeWaPlan(t, db, []uint{20, 21})
	_, parts, err := readWithdrawAddressPlan(path, 10)
	if err != nil || len(parts) != 1 {
		t.Fatal(err)
	}
	ids, err := resolveWaIDs(db)
	if err != nil {
		t.Fatal(err)
	}

	// The stored reward changed after planning: the state check refuses.
	if err := db.Exec("UPDATE taxable_events SET amount = '626' WHERE message_id = 21").Error; err != nil {
		t.Fatal(err)
	}
	if v, err := checkWaPart(db, parts[0], ids); err != nil || v.State != 1 || v.Booked != 1 {
		t.Fatalf("changed rows %+v %v", v, err)
	}
	if _, _, err := loadWaPart(db, parts[0], true); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("load after change: %v", err)
	}
	if n := waRowCount(t, db, "reward_trigger = 'withdraw_address'"); n != 0 {
		t.Fatalf("refused part wrote %d rows", n)
	}
	if err := db.Exec("UPDATE taxable_events SET amount = '627' WHERE message_id = 21").Error; err != nil {
		t.Fatal(err)
	}

	// The module paid another account in the stored events.
	if err := db.Exec(`UPDATE message_event_attributes SET value = 'cosmos1someoneelsexxxxxxxxxxxxxxxxxxxxxxxxxx'
 WHERE value = ? AND message_event_id IN (SELECT id FROM message_events WHERE message_id = 20)`, waLoadWithdraw).Error; err != nil {
		t.Fatal(err)
	}
	if v, err := checkWaPart(db, parts[0], ids); err != nil || v.Paid != 1 || v.State != 0 {
		t.Fatalf("unpaid move %+v %v", v, err)
	}

	// An event key renamed away leaves the ids unresolved.
	if err := db.Exec("UPDATE message_event_attribute_keys SET key = 'recipient_old' WHERE key = 'recipient'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWaIDs(db); err == nil {
		t.Fatal("ids resolved without a recipient key")
	}
	if err := db.Exec("UPDATE message_event_attribute_keys SET key = 'recipient' WHERE key = 'recipient_old'").Error; err != nil {
		t.Fatal(err)
	}

	// A plan file edited after the dry run hashes differently.
	sum, _, err := readWithdrawAddressPlan(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "\t627\t", "\t628\t", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil { //nolint:gosec // the test's own temp file
		t.Fatal(err)
	}
	if edited, _, err := readWithdrawAddressPlan(path, 10); err != nil || edited == sum {
		t.Fatalf("edited plan hashes the same: %v", err)
	}
}

// A load waits on the same messages row lock taxrepair's applyPlans takes,
// and gives up at its lock timeout instead of interleaving.
func TestWithdrawAddressLoadWaitsOnMessageRowLocks(t *testing.T) {
	db := waLoadScratch(t)
	seedWaMessage(t, db, 30, waLoadWithdraw)
	path := writeWaPlan(t, db, []uint{30})
	_, parts, err := readWithdrawAddressPlan(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	held := db.Begin()
	defer held.Rollback()
	var locked []uint
	if err := held.Raw("SELECT id FROM messages WHERE id IN ? ORDER BY id FOR UPDATE", []uint{30}).Scan(&locked).Error; err != nil || len(locked) != 1 {
		t.Fatal(err)
	}
	start := time.Now()
	if _, _, err := loadWaPart(db, parts[0], true); err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("load under messages rows held FOR UPDATE: %v", err)
	}
	if time.Since(start) < 9*time.Second {
		t.Fatal("load did not wait for the lock")
	}
	if n := waRowCount(t, db, "reward_trigger = 'withdraw_address'"); n != 0 {
		t.Fatalf("%d rows written under a held lock", n)
	}
}

// On the combined tree, the repair that replays messages (the wasm repair's
// applyPlans) writes the moves itself: a message it replays keeps a move the
// loader put there, and one without gets it, so the loader then has nothing
// left to add.
func TestRepairReplayKeepsWithdrawAddressMoves(t *testing.T) {
	db := waLoadScratch(t)
	for _, id := range []uint{40, 41} {
		seedWaMessage(t, db, id, waLoadWithdraw)
	}
	_, parts, err := readWithdrawAddressPlan(writeWaPlan(t, db, []uint{41}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, n, err := loadWaPart(db, parts[0], true); err != nil || n != 1 {
		t.Fatalf("load: %d %v", n, err)
	}
	// Give 41's reward row a stale detail so the replay rewrites it.
	if err := db.Exec("UPDATE taxable_events SET detail = '{\"stale\":true}' WHERE message_id = 41 AND sub_index = 0").Error; err != nil {
		t.Fatal(err)
	}
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := load(db, 0, 100, 100, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("load rows %d %v", len(rows), err)
	}
	plans, _, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("%d plans", len(plans))
	}
	for _, p := range plans {
		if len(p.Expected) != 2 || p.Expected[1].RewardTrigger != tax.TriggerWithdrawAddress || p.Expected[1].ToAddr != waLoadWithdraw || p.Expected[1].SubIndex != 1 {
			t.Fatalf("message %d replays to %+v", p.MessageID, p.Expected)
		}
	}
	if err := applyPlans(db, plans); err != nil {
		t.Fatal(err)
	}
	if n := waRowCount(t, db, "reward_trigger = 'withdraw_address' AND sub_index = 1 AND amount = '627' AND to_addr = '"+waLoadWithdraw+"'"); n != 2 {
		t.Fatalf("%d moves after the replay", n)
	}
	if n := waRowCount(t, db, "detail = '{\"stale\":true}'"); n != 0 {
		t.Fatal("replay left the stale row")
	}
	// The planner now finds both messages already moved, exactly as the
	// parser books them, and plans nothing.
	planned, counts, err := planWithdrawAddressBatch(db, codec, []uint{40, 41}, nil)
	if err != nil || len(planned) != 0 || counts[waAlreadyExact] != 2 {
		t.Fatalf("plan after the replay: %d rows %v %v", len(planned), counts, err)
	}
}

// An authz Exec stored without bytes plans as no_body. With its archive
// transaction it is classed from the parser's rows like any message, and
// counted as recovered, but only when that transaction hashes to the stored
// tx hash and holds a MsgExec at the stored message index; any other body is
// refused and the message stays no_body.
func TestWithdrawAddressPlanRecoveredBodies(t *testing.T) {
	db := mbScratch(t)
	raws := map[uint][]byte{}
	for _, id := range []uint{50, 51, 52, 53} {
		raws[id], _ = seedMbMessage(t, db, id)
	}
	// 51's archive Exec claims for another delegator, so its stored reward
	// row is not reproduced and nothing is planned for it.
	raws[51], _ = mbTx(t, waLoadWithdraw)
	other, _ := mbTx(t, waValidator)
	for _, q := range []string{
		"UPDATE txes SET hash = '" + mbHash(raws[51]) + "' WHERE id = 51",
		"UPDATE txes SET hash = '" + mbHash(other) + "' WHERE id = 52",
		"UPDATE messages SET message_index = 0 WHERE id = 53",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	ids := []uint{50, 51, 52, 53}
	_, counts, err := planWithdrawAddressBatch(db, codec, ids, nil)
	if err != nil || counts[waNoBody] != 4 {
		t.Fatalf("without bodies %v %v", counts, err)
	}
	bodies, err := readWaBodies(writeMbPlan(t, raws))
	if err != nil || len(bodies) != 4 {
		t.Fatalf("read bodies %d %v", len(bodies), err)
	}
	plans, counts, err := planWithdrawAddressBatch(db, codec, ids, bodies)
	if err != nil {
		t.Fatal(err)
	}
	if counts[waExact] != 1 || counts[waRecoveredBody+"_"+waExact] != 1 || counts[waNoBody] != 2 ||
		counts[waRecoveredBody+"_refused_hash"] != 1 || counts[waRecoveredBody+"_refused_index"] != 1 ||
		len(plans) != 1 || plans[0].MessageID != 50 || plans[0].SubIndex != 1 || plans[0].FromAddr != waLoadDelegator || plans[0].ToAddr != waLoadWithdraw {
		t.Fatalf("with bodies %v %+v", counts, plans)
	}
	// A body is never taken from an id-to-bytes list: the old two-column
	// form, and an archive line whose tx bytes do not hash to its own hash
	// column, are refused when read.
	path := filepath.Join(t.TempDir(), "bodies.tsv")
	for _, bad := range []string{
		"50\t" + hex.EncodeToString(raws[50]) + "\n",
		"50\t1\t" + mbHash(other) + "\t" + hex.EncodeToString(raws[50]) + "\n",
	} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readWaBodies(path); err == nil {
			t.Fatalf("accepted %q", bad[:8])
		}
	}
}

// The planner names each refused message by id and reason, with no address,
// so the refusals can be listed for handling by hand.
func TestWithdrawAddressPlanNamesRefusedMessages(t *testing.T) {
	db := waLoadScratch(t)
	seedWaMessage(t, db, 95, waLoadWithdraw)
	seedWaMessage(t, db, 96, waLoadWithdraw)
	for _, q := range []string{
		"DELETE FROM message_event_attributes WHERE message_event_id IN (SELECT id FROM message_events WHERE message_id = 96)",
		"DELETE FROM message_events WHERE message_id = 96",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	seedWaEvents(t, db, 96, []txlog.LogMessageEvent{
		waEvent("message", "action", waClaimURL, "sender", waLoadDelegator, "module", "distribution"),
		waEvent("transfer", "recipient", waLoadDelegator, "sender", waDistribution, "amount", "300uatom"),
		waEvent("transfer", "recipient", waLoadWithdraw, "sender", waDistribution, "amount", "327uatom"),
		waEvent("withdraw_rewards", "amount", "627uatom", "validator", waValidator, "delegator", waLoadDelegator),
	})
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	_, counts, refused, err := planWaBatch(db, codec, []uint{95, 96}, nil)
	if err != nil || counts[waExact] != 1 || counts[waRefusedSelf] != 1 || len(refused) != 1 {
		t.Fatalf("%v %v %v", counts, refused, err)
	}
	if r := refused[96]; !strings.Contains(r, ","+waRefusedSelf) || strings.Contains(r, "cosmos1") {
		t.Fatalf("message 96 refused as %q", r)
	}
}
