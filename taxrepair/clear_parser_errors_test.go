package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"gorm.io/gorm"
)

const (
	execURL      = "/cosmos.authz.v1beta1.MsgExec"
	messageEvent = "message"
	testVal      = "val"
)

// seedExec stores an authz exec with the given events and no tax rows.
func seedExec(t *testing.T, db *gorm.DB, id uint, inner *codectypes.Any, events [][2]string) {
	t.Helper()
	exec := &authz.MsgExec{Grantee: "cosmos1grantee", Msgs: []*codectypes.Any{inner}}
	wire, err := exec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2025, 4, 15, 9, 40, 52, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO blocks VALUES(?,?,?)", []any{id, 25030000 + id, stamp}},
		{"INSERT INTO txes VALUES(?,?,?,0)", []any{id, id, "exec"}},
		{sqlSeedType, []any{id, execURL}},
		{sqlSeedMessage, []any{id, id, id, wire}},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, e := range events {
		var typeID, eventID, keyID uint
		if err := db.Raw("INSERT INTO message_event_types(type) VALUES(?) RETURNING id", e[0]).Scan(&typeID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Raw("INSERT INTO message_events(message_id,index,message_event_type_id) VALUES(?,?,?) RETURNING id", id, i, typeID).Scan(&eventID).Error; err != nil {
			t.Fatal(err)
		}
		if e[1] == "" {
			continue
		}
		if err := db.Raw("INSERT INTO message_event_attribute_keys(key) VALUES('amount') RETURNING id").Scan(&keyID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO message_event_attributes(message_event_id,index,message_event_attribute_key_id,value) VALUES(?,0,?,?)", eventID, keyID, e[1]).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func parserErrorIDs(t *testing.T, db *gorm.DB) []uint {
	t.Helper()
	var ids []uint
	if err := db.Raw("SELECT id FROM message_parser_errors ORDER BY id").Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	return ids
}

// A repair that now classifies a message from its stored body removes that
// message's tax parser error, as the indexer does on a clean parse, and keeps
// every other error: one from a different parser, and one whose message still
// does not classify.
func TestRepairClearsResolvedParserErrors(t *testing.T) {
	db := scratch(t)
	store, err := codectypes.NewAnyWithValue(&wasmtypes.MsgStoreCode{Sender: "cosmos1grantee", WASMByteCode: []byte("\x00asm")})
	if err != nil {
		t.Fatal(err)
	}
	seedExec(t, db, 1, store, [][2]string{{messageEvent, ""}, {"store_code", ""}})
	delegate := &staking.MsgDelegate{DelegatorAddress: fixtureWallet, ValidatorAddress: testVal, Amount: sdk.NewInt64Coin("uatom", 94405000000)}
	wire, err := delegate.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	seedMessage(t, db, 2, urls[0], wire, "delegate-principal")
	unknown := &codectypes.Any{TypeUrl: "/not.registered.Inner", Value: []byte{8, 1}}
	seedExec(t, db, 3, unknown, [][2]string{{messageEvent, ""}, {"coin_received", "5uatom"}})
	for _, q := range []string{
		"INSERT INTO message_parsers(id,identifier) VALUES(4,'tax:" + execURL + "'),(9,'tax:" + urls[0] + "'),(5,'other:" + execURL + "')",
		"INSERT INTO message_parser_errors(id,message_parser_id,message_id,error) VALUES(10,4,1,'authz 0: unsupported inner'),(11,5,1,'other parser'),(12,9,2,'old delegate error'),(13,4,3,'unknown inner')",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	plan := func() []backup {
		rows, err := load(db, 0, 3, 100, nil)
		if err != nil || len(rows) != 3 {
			t.Fatalf("load: %d %v", len(rows), err)
		}
		plans, skips, parsed, err := planBatchParsed(db, codec, rows)
		if err != nil {
			t.Fatal(err)
		}
		if len(skips) != 1 || skips[0].MessageID != 3 {
			t.Fatalf("skips = %+v", skips)
		}
		if len(parsed) != 2 {
			t.Fatalf("parsed = %v", parsed)
		}
		plans, err = withResolvedErrors(db, plans, parsed, rows)
		if err != nil {
			t.Fatal(err)
		}
		return plans
	}
	plans := plan()
	if len(plans) != 2 || plans[0].MessageID != 1 || plans[1].MessageID != 2 {
		t.Fatalf("plans = %+v", plans)
	}
	if !same(plans[0].Old, plans[0].Expected) || len(plans[0].Expected) != 0 || len(plans[0].ClearedParserErrors) != 1 || plans[0].ClearedParserErrors[0].ID != 10 {
		t.Fatalf("store code plan = %+v", plans[0])
	}
	if same(plans[1].Old, plans[1].Expected) || len(plans[1].ClearedParserErrors) != 1 || plans[1].ClearedParserErrors[0].ID != 12 {
		t.Fatalf("delegate plan = %+v", plans[1])
	}
	path := filepath.Join(t.TempDir(), "backup.jsonl")
	b, err := newBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.file.Close()
	if err := b.persist(plans); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var backed []parserErrorRow
	scan := bufio.NewScanner(f)
	scan.Buffer(nil, 1<<24)
	for scan.Scan() {
		var rec backup
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		backed = append(backed, rec.ClearedParserErrors...)
	}
	if len(backed) != 2 || backed[0].Error != "authz 0: unsupported inner" || backed[1].MessageParserID != 9 {
		t.Fatalf("backup holds %+v", backed)
	}

	// An error that changed after the backup rolls the whole batch back.
	if err := db.Exec("UPDATE message_parser_errors SET error='rewritten' WHERE id=12").Error; err != nil {
		t.Fatal(err)
	}
	if err := applyPlans(db, plans); err == nil {
		t.Fatal("changed parser error was deleted")
	}
	if got := parserErrorIDs(t, db); len(got) != 4 {
		t.Fatalf("rollback left %v", got)
	}
	before, _ := oldRows(db, []uint{2})
	if !same(before[2], plans[1].Old) {
		t.Fatal("rollback left rewritten tax rows")
	}
	if err := db.Exec("UPDATE message_parser_errors SET error='old delegate error' WHERE id=12").Error; err != nil {
		t.Fatal(err)
	}

	if err := applyPlans(db, plans); err != nil {
		t.Fatal(err)
	}
	if got := parserErrorIDs(t, db); len(got) != 2 || got[0] != 11 || got[1] != 13 {
		t.Fatalf("parser errors left = %v", got)
	}
	after, _ := oldRows(db, []uint{1, 2})
	if len(after[1]) != 0 || !same(after[2], plans[1].Expected) || after[2][0].Category != string(tax.CategoryDelegate) {
		t.Fatalf("tax rows after = %+v", after)
	}
	if again := plan(); len(again) != 0 {
		t.Fatalf("second run not idempotent: %+v", again)
	}
}
