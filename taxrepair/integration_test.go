package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	sdk "github.com/cosmos/cosmos-sdk/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/ory/dockertest/v3"
	"gorm.io/gorm"
)

func scratch(t *testing.T) *gorm.DB {
	t.Helper()
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := pool.Run("postgres", "15-alpine", []string{"POSTGRES_USER=test", "POSTGRES_PASSWORD=test", "POSTGRES_DB=test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Purge(resource); err != nil {
			t.Error(err)
		}
	})
	var db *gorm.DB
	err = pool.Retry(func() error {
		var e error
		db, e = dbpkg.PostgresDbConnect(resource.GetBoundIP("5432/tcp"), resource.GetPort("5432/tcp"), "test", "test", "test", "silent")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	sql := `CREATE TABLE blocks(id bigint primary key,height bigint,time_stamp timestamptz);
 CREATE TABLE txes(id bigint primary key,block_id bigint,hash text,code bigint);
 CREATE TABLE message_types(id bigint primary key,message_type text);
 CREATE TABLE messages(id bigint primary key,tx_id bigint,message_type_id bigint,message_index bigint,message_bytes bytea);
 CREATE TABLE message_event_types(id bigserial primary key,type text);
 CREATE TABLE message_event_attribute_keys(id bigserial primary key,key text);
 CREATE TABLE message_events(id bigserial primary key,message_id bigint,index bigint,message_event_type_id bigint);
 CREATE TABLE message_event_attributes(id bigserial primary key,message_event_id bigint,index bigint,message_event_attribute_key_id bigint,value text);
 CREATE TABLE taxable_events(id bigserial primary key,message_id bigint REFERENCES messages(id),sub_index bigint,category text,amount text,denom text,asset text,from_addr text,to_addr text,block_height bigint,timestamp timestamptz,tx_hash text,validator_address text,reward_trigger text,UNIQUE(message_id,sub_index));`
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	return db
}
func seedMessage(t *testing.T, db *gorm.DB, id uint, typ string, wire []byte, fixture string) {
	t.Helper()
	stamp := time.Date(2026, 4, 15, 9, 40, 52, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO blocks VALUES(?,?,?)", []any{id, 30672570 + id, stamp}},
		{"INSERT INTO txes VALUES(?,?,?,0)", []any{id, id, fixture}},
		{"INSERT INTO message_types VALUES(?,?)", []any{id, typ}},
		{"INSERT INTO messages VALUES(?,?,?,0,?)", []any{id, id, id, wire}},
		{"INSERT INTO taxable_events(message_id,sub_index,category,amount,denom,to_addr,block_height,timestamp,tx_hash) VALUES(?,0,'reward','94405000000','uatom','wallet',?,?,?)", []any{id, 30672570 + id, stamp, fixture}},
	} {
		if err := db.Exec(q.sql, q.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	var f struct {
		Events []txlog.LogMessageEvent `json:"events"`
	}
	raw, err := os.ReadFile("../tax/testdata/" + fixture + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for i, e := range f.Events {
		scoped := false
		for _, a := range e.Attributes {
			if a.Key == "msg_index" && a.Value == "0" {
				scoped = true
			}
		}
		if !scoped {
			continue
		}
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
}
func TestRepairIntegrationZeroSplitStampsAndIdempotence(t *testing.T) {
	db := scratch(t)
	delegate := &staking.MsgDelegate{DelegatorAddress: "cosmos1ts4vwmfccwjv0vehlzd4x5rw5xzljjnhgm7gnz", ValidatorAddress: "val", Amount: sdk.NewInt64Coin("uatom", 94405000000)}
	wire, err := delegate.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	seedMessage(t, db, 1, urls[0], wire, "delegate-principal")
	redelegate := &staking.MsgBeginRedelegate{DelegatorAddress: "cosmos1f3vdsge09avpxsym5233xgskwv2q5s3cg57dcs", ValidatorSrcAddress: "src", ValidatorDstAddress: "dst", Amount: sdk.NewInt64Coin("uatom", 18885870000)}
	wire, err = redelegate.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	seedMessage(t, db, 2, urls[2], wire, "redelegate-two-validators")
	codec, err := repairCodec()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := load(db, 0, 2, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("load got %d", len(rows))
	}
	plans, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || len(plans[0].Expected) != 0 || len(plans[1].Expected) != 2 {
		t.Fatalf("wrong plan: %+v", plans)
	}
	// Production does not retain message bytes. The ordered action/sender and
	// withdrawal events must produce exactly the same plan without them.
	if err := db.Exec("UPDATE messages SET message_bytes=NULL").Error; err != nil {
		t.Fatal(err)
	}
	rows, err = load(db, 0, 2, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutBytes, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutBytes) != len(plans) {
		t.Fatal("event-only plan lost messages")
	}
	for i := range plans {
		if !same(withoutBytes[i].Expected, plans[i].Expected) {
			t.Fatal("event-only plan differs from body decode")
		}
	}
	var noBackup *backupWriter
	if err := noBackup.persist(plans); err != nil {
		t.Fatal(err)
	}
	before, err := oldRows(db, []uint{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(before[1]) != 1 || len(before[2]) != 1 {
		t.Fatal("dryrun mutated rows")
	}
	path := filepath.Join(t.TempDir(), "backup.jsonl")
	b, err := newBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.file.Close()
	if err = b.persist(plans); err != nil {
		t.Fatal(err)
	}
	if sum, err := b.verify(); err != nil || len(sum) != 64 {
		t.Fatalf("bad backup: %s %v", sum, err)
	}
	if _, err := newBackup(path); err == nil {
		t.Fatal("backup overwrite allowed")
	}
	if err := applyPlans(db, plans); err != nil {
		t.Fatal(err)
	}
	after, err := oldRows(db, []uint{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(after[1]) != 0 || !same(after[2], plans[1].Expected) {
		t.Fatalf("bad replacement: %+v", after)
	}
	if after[2][0].Amount != "35629" || after[2][1].Amount != "47507213" || after[2][0].BlockHeight != 30672572 {
		t.Fatal(after[2])
	}
	again, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatal("repair not idempotent")
	}
	// The production indexer also removes excess sub-indices and empty results.
	msg := models.Message{ID: 2, Tx: models.Tx{Hash: "redelegate-two-validators", Block: models.Block{Height: 30672572, TimeStamp: rows[1].Stamp}}}
	onlyOne := any(plans[1].Expected[:1])
	if err := (&tax.Parser{}).IndexMessage(&onlyOne, db, msg, nil, config.IndexConfig{}); err != nil {
		t.Fatal(err)
	}
	current, _ := oldRows(db, []uint{2})
	if len(current[2]) != 1 {
		t.Fatal("indexer left stale sub-index")
	}
	empty := any([]tax.TaxableEvent{})
	if err := (&tax.Parser{}).IndexMessage(&empty, db, msg, nil, config.IndexConfig{}); err != nil {
		t.Fatal(err)
	}
	current, _ = oldRows(db, []uint{2})
	if len(current[2]) != 0 {
		t.Fatal("indexer left stale zero-output rows")
	}
}

func TestRepairRejectsConcurrentChanges(t *testing.T) {
	db := scratch(t)
	msg := &staking.MsgDelegate{DelegatorAddress: "wallet", ValidatorAddress: "val", Amount: sdk.NewInt64Coin("uatom", 94405000000)}
	wire, _ := msg.Marshal()
	seedMessage(t, db, 1, urls[0], wire, "delegate-principal")
	codec, _ := repairCodec()
	rows, err := load(db, 0, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := planBatch(db, codec, rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE taxable_events SET amount='123'").Error; err != nil {
		t.Fatal(err)
	}
	if err := applyPlans(db, plans); err == nil {
		t.Fatal("concurrent change overwritten")
	}
	got, _ := oldRows(db, []uint{1})
	if got[1][0].Amount != "123" {
		t.Fatal("rollback failed")
	}
}
