package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"gorm.io/gorm"
)

// mbTx is an archive transaction whose message 1 is an authz Exec of the
// delegator's claim, after a bank send at message 0.
func mbTx(t *testing.T, delegator string) (raw []byte, exec []byte) {
	t.Helper()
	claim, err := codectypes.NewAnyWithValue(&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: delegator, ValidatorAddress: waValidator})
	if err != nil {
		t.Fatal(err)
	}
	execMsg := &authz.MsgExec{Grantee: waLoadWithdraw, Msgs: []*codectypes.Any{claim}}
	execAny, err := codectypes.NewAnyWithValue(execMsg)
	if err != nil {
		t.Fatal(err)
	}
	send, err := codectypes.NewAnyWithValue(&banktypes.MsgSend{FromAddress: waLoadWithdraw, ToAddress: delegator, Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 1))})
	if err != nil {
		t.Fatal(err)
	}
	body, err := (&sdktx.TxBody{Messages: []*codectypes.Any{send, execAny}, Memo: delegator[:12]}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = (&sdktx.TxRaw{BodyBytes: body, Signatures: [][]byte{{1, 2, 3}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw, execAny.Value
}

func mbHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// seedMbMessage stores a claim inside an authz Exec at message index 1, with
// no bytes, under the archive transaction's real hash.
func seedMbMessage(t *testing.T, db *gorm.DB, id uint) (raw, exec []byte) {
	t.Helper()
	seedWaMessage(t, db, id, waLoadWithdraw)
	raw, exec = mbTx(t, waLoadDelegator)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO message_types VALUES(2, ?) ON CONFLICT DO NOTHING", []any{typeURLAuthzExec}},
		{"UPDATE messages SET message_type_id = 2, message_index = 1 WHERE id = ?", []any{id}},
		{"UPDATE txes SET hash = ? WHERE id = ?", []any{mbHash(raw), id}},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	return raw, exec
}

func mbScratch(t *testing.T) *gorm.DB {
	t.Helper()
	db := waLoadScratch(t)
	if err := db.Exec("ALTER TABLE messages ADD COLUMN opaque boolean NOT NULL DEFAULT false").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func writeMbPlan(t *testing.T, lines map[uint][]byte) string {
	t.Helper()
	var b strings.Builder
	for id, raw := range lines {
		fmt.Fprintf(&b, "%d\t1\t%s\t%s\n", id, mbHash(raw), hex.EncodeToString(raw))
	}
	path := filepath.Join(t.TempDir(), "bodies-plan.tsv")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mbStoredBytes(t *testing.T, db *gorm.DB, id uint) []byte {
	t.Helper()
	var b []byte
	if err := db.Raw("SELECT message_bytes FROM messages WHERE id = ?", id).Row().Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The read-only check writes nothing, a dry run writes and rolls back, an
// apply records each row it writes as id and NULL first, and a rerun is a
// no-op. Afterwards the replay and the withdraw-address planner parse the
// message from its stored bytes, with no recovered bodies given.
func TestMessageBodiesLoad(t *testing.T) {
	db := mbScratch(t)
	raw60, exec60 := seedMbMessage(t, db, 60)
	raw61, exec61 := seedMbMessage(t, db, 61)
	sum, parts, err := readMessageBodiesPlan(writeMbPlan(t, map[uint][]byte{60: raw60, 61: raw61}), 1)
	if err != nil || len(parts) != 2 || parts[0].First != 60 || parts[1].First != 61 || len(sum) != 64 || parts[0].Sum == parts[1].Sum {
		t.Fatalf("parts %+v %v", parts, err)
	}
	if !bytes.Equal(parts[0].Rows[0].Body, exec60) || parts[0].Rows[0].TypeURL != typeURLAuthzExec {
		t.Fatal("body is not the transaction's message 1")
	}

	if _, err := checkMbParts(db, parts); err == nil || !strings.Contains(err.Error(), "not read only") {
		t.Fatalf("check ran on a writable connection: %v", err)
	}
	errDone := errors.New("rolled back")
	err = db.Transaction(func(conn *gorm.DB) error {
		if err := conn.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
			return err
		}
		if failed, err := checkMbParts(conn, parts); err != nil || failed != 0 {
			t.Fatalf("read-only check: %d failed %v", failed, err)
		}
		for _, p := range parts {
			v, todo, err := checkMbPart(conn, p)
			if err != nil || v.failures() != 0 || v.Todo != 1 || v.Written != 0 || len(todo) != 1 {
				t.Fatalf("check %+v %v", v, err)
			}
		}
		if err := conn.Exec("UPDATE messages SET message_bytes = '\\x00' WHERE id = 60").Error; err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("the check connection accepted a write: %v", err)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatal(err)
	}

	for _, p := range parts {
		if _, n, err := loadMbPart(db, p, false, nil); err != nil || n != 1 {
			t.Fatalf("dry run part %d: %d %v", p.N, n, err)
		}
	}
	if mbStoredBytes(t, db, 60) != nil || mbStoredBytes(t, db, 61) != nil {
		t.Fatal("dry run wrote bodies")
	}

	backupPath := filepath.Join(t.TempDir(), "backup.tsv")
	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	for _, p := range parts {
		if _, n, err := loadMbPart(db, p, true, backup); err != nil || n != 1 {
			t.Fatalf("apply part %d: %d %v", p.N, n, err)
		}
	}
	if !bytes.Equal(mbStoredBytes(t, db, 60), exec60) || !bytes.Equal(mbStoredBytes(t, db, 61), exec61) {
		t.Fatal("apply did not write the bodies")
	}
	for _, p := range parts {
		v, n, err := loadMbPart(db, p, true, backup)
		if err != nil || n != 0 || v.Written != 1 || v.Todo != 0 {
			t.Fatalf("rerun part %d: %d %+v %v", p.N, n, v, err)
		}
	}
	got, err := os.ReadFile(backupPath) //nolint:gosec // the test's own temp file
	if err != nil || string(got) != "60\tNULL\n61\tNULL\n" {
		t.Fatalf("backup %q %v", got, err)
	}

	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	planned, counts, err := planWithdrawAddressBatch(db, codec, []uint{60, 61}, nil)
	if err != nil || counts[waNoBody] != 0 || counts[waExact] != 2 || len(planned) != 2 {
		t.Fatalf("plan without bodies after the write: %v %d %v", counts, len(planned), err)
	}
	rows, err := load(db, 0, 100, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	plans, _, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	for _, p := range plans {
		if p.MessageID == 60 || p.MessageID == 61 {
			replayed++
			if n := len(p.Expected); n != 2 || p.Expected[1].RewardTrigger != tax.TriggerWithdrawAddress || p.Expected[1].ToAddr != waLoadWithdraw {
				t.Fatalf("message %d replays to %+v", p.MessageID, p.Expected)
			}
		}
	}
	if replayed != 2 {
		t.Fatalf("the replay planned %d of the 2 written messages", replayed)
	}
}

// A body is written only when its archive transaction hashes to the stored
// tx hash and its message matches the stored index and type, the stored
// message is not opaque and holds no other bytes. Any refusal refuses the
// whole part and writes nothing.
func TestMessageBodiesLoadRefusals(t *testing.T) {
	db := mbScratch(t)
	raws := map[uint][]byte{}
	for _, id := range []uint{70, 71, 72, 73, 74, 75} {
		raws[id], _ = seedMbMessage(t, db, id)
	}
	other, _ := mbTx(t, waLoadWithdraw)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE txes SET hash = ? WHERE id = 71", []any{mbHash(other)}},
		{"UPDATE messages SET message_index = 0 WHERE id = 72", nil},
		{"UPDATE messages SET message_type_id = 1 WHERE id = 73", nil},
		{"UPDATE messages SET opaque = true WHERE id = 74", nil},
		{"UPDATE messages SET message_bytes = '\\x0a00' WHERE id = 75", nil},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, parts, err := readMessageBodiesPlan(writeMbPlan(t, raws), 1)
	if err != nil || len(parts) != 6 {
		t.Fatalf("parts %d %v", len(parts), err)
	}
	want := []mbVerdict{
		{Rows: 1, Todo: 1},
		{Rows: 1, Hash: 1},
		{Rows: 1, Index: 1},
		{Rows: 1, Type: 1},
		{Rows: 1, Opaque: 1},
		{Rows: 1, Conflict: 1},
	}
	for i, p := range parts[1:] {
		v, _, err := checkMbPart(db, p)
		if err != nil || v != want[i+1] {
			t.Fatalf("message %d: %+v, want %+v (%v)", p.First, v, want[i+1], err)
		}
		if _, n, err := loadMbPart(db, p, true, nil); err == nil || n != 0 || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("message %d loaded: %d %v", p.First, n, err)
		}
	}
	// A part holding one good and one mismatched row writes neither.
	_, mixed, err := readMessageBodiesPlan(writeMbPlan(t, map[uint][]byte{70: raws[70], 71: raws[71]}), 10)
	if err != nil || len(mixed) != 1 {
		t.Fatal(err)
	}
	if _, _, err := loadMbPart(db, mixed[0], true, nil); err == nil {
		t.Fatal("a part with a hash mismatch loaded")
	}
	if mbStoredBytes(t, db, 70) != nil || mbStoredBytes(t, db, 71) != nil {
		t.Fatal("a refused part wrote bodies")
	}
	var n int
	if err := db.Raw("SELECT count(*) FROM messages WHERE message_bytes IS NOT NULL AND id <> 75").Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("%d bodies written by refused parts %v", n, err)
	}

	// Each part commits on its own: with one message per part, 70 commits
	// although the next part, 71, is refused.
	_, single, err := readMessageBodiesPlan(writeMbPlan(t, map[uint][]byte{70: raws[70], 71: raws[71]}), 1)
	if err != nil || len(single) != 2 {
		t.Fatal(err)
	}
	if _, n, err := loadMbPart(db, single[0], true, nil); err == nil || n != 0 {
		t.Fatalf("apply without a backup file: %d %v", n, err)
	}
	backup, err := os.OpenFile(filepath.Join(t.TempDir(), "backup.tsv"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if _, n, err := loadMbPart(db, single[0], true, backup); err != nil || n != 1 {
		t.Fatalf("part 1: %d %v", n, err)
	}
	if _, _, err := loadMbPart(db, single[1], true, backup); err == nil {
		t.Fatal("part 2 loaded")
	}
	if mbStoredBytes(t, db, 70) == nil || mbStoredBytes(t, db, 71) != nil {
		t.Fatal("parts did not commit on their own")
	}

	// A plan line whose transaction does not hash to its own hash column, a
	// message index past the transaction's messages, and a repeated message
	// are refused when the plan is read.
	dir := t.TempDir()
	good := fmt.Sprintf("70\t1\t%s\t%s\n", mbHash(raws[70]), hex.EncodeToString(raws[70]))
	for name, text := range map[string]string{
		"hash":   fmt.Sprintf("70\t1\t%s\t%s\n", mbHash(other), hex.EncodeToString(raws[70])),
		"index":  fmt.Sprintf("70\t2\t%s\t%s\n", mbHash(raws[70]), hex.EncodeToString(raws[70])),
		"repeat": good + good,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readMessageBodiesPlan(path, 10); err == nil {
			t.Fatalf("plan with a bad %s read", name)
		}
	}
	// An edited plan file has another sha, which --apply refuses.
	path := filepath.Join(dir, "plan")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	sum1, _, err := readMessageBodiesPlan(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(good, "70\t", "076\t", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if sum2, _, _ := readMessageBodiesPlan(path, 10); sum2 == sum1 {
		t.Fatal("an edited plan kept its sha")
	}
}

// The loader waits on another loader's advisory lock and on messages rows a
// replay holds FOR UPDATE, and gives up after the lock timeout without
// writing.
func TestMessageBodiesLoadWaitsOnLocks(t *testing.T) {
	db := mbScratch(t)
	raw, _ := seedMbMessage(t, db, 80)
	_, parts, err := readMessageBodiesPlan(writeMbPlan(t, map[uint][]byte{80: raw}), 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, hold := range map[string]func(*gorm.DB) error{
		"advisory": func(tx *gorm.DB) error {
			return tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", waLoadLockKey).Error
		},
		"rows": func(tx *gorm.DB) error {
			var locked []uint
			return tx.Raw("SELECT id FROM messages WHERE id IN ? ORDER BY id FOR UPDATE", []uint{80}).Scan(&locked).Error
		},
	} {
		held := db.Begin()
		if err := hold(held); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, _, err := loadMbPart(db, parts[0], true, nil)
		held.Rollback()
		if err == nil || !strings.Contains(err.Error(), "lock timeout") || time.Since(start) < 9*time.Second {
			t.Fatalf("%s lock: %v after %s", name, err, time.Since(start))
		}
		if mbStoredBytes(t, db, 80) != nil {
			t.Fatalf("written under a held %s lock", name)
		}
	}
}
