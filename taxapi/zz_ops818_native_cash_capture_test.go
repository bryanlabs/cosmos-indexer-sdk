//go:build ops818harness

package taxapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"database/sql"
	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

type nativeCashAttribute struct {
	EventID, MessageID                 uint
	Height                             int64
	Timestamp                          time.Time
	MessageType, EventType, Key, Value string
	Index                              uint
}

const nativeCashMessages = `WITH named AS MATERIALIZED (
 SELECT DISTINCT a.message_event_id
 FROM message_event_attributes a
 JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id
 WHERE left(a.value,100)=left(?,100) AND a.value=? AND k.key IN ('receiver','spender')
)
SELECT me.id AS event_id,me.message_id,b.height,b.time_stamp AS timestamp,mt.message_type,et.type AS event_type,attrs.key,attrs.value,attrs.index
FROM named n
JOIN message_events me ON me.id=n.message_event_id
JOIN message_event_types et ON et.id=me.message_event_type_id AND et.type IN ('coin_received','coin_spent')
JOIN messages m ON m.id=me.message_id
JOIN message_types mt ON mt.id=m.message_type_id
JOIN txes t ON t.id=m.tx_id
JOIN blocks b ON b.id=t.block_id
JOIN chains c ON c.id=b.chain_id
CROSS JOIN LATERAL (
 SELECT k.key,a.value,a.index FROM message_event_attributes a
 JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id
 WHERE a.message_event_id=me.id ORDER BY a.index OFFSET 0
) attrs
WHERE c.chain_id IN ('cosmoshub-4','mainnet','cosmoshub') AND b.time_stamp>=? AND b.time_stamp<?
ORDER BY b.height,me.message_id,me.id,attrs.index`
const nativeCashBlocks = `WITH named AS MATERIALIZED (
 SELECT DISTINCT a.block_event_id
 FROM block_event_attributes a
 JOIN block_event_attribute_keys k ON k.id=a.block_event_attribute_key_id
 WHERE left(a.value,100)=left(?,100) AND a.value=? AND k.key IN ('receiver','spender')
)
SELECT be.id AS event_id,b.height,b.time_stamp AS timestamp,et.type AS event_type,attrs.key,attrs.value,attrs.index
FROM named n
JOIN block_events be ON be.id=n.block_event_id
JOIN block_event_types et ON et.id=be.block_event_type_id AND et.type IN ('coin_received','coin_spent')
JOIN blocks b ON b.id=be.block_id
JOIN chains c ON c.id=b.chain_id
CROSS JOIN LATERAL (
 SELECT k.key,a.value,a.index FROM block_event_attributes a
 JOIN block_event_attribute_keys k ON k.id=a.block_event_attribute_key_id
 WHERE a.block_event_id=be.id ORDER BY a.index OFFSET 0
) attrs
WHERE c.chain_id IN ('cosmoshub-4','mainnet','cosmoshub') AND b.time_stamp>=? AND b.time_stamp<?
ORDER BY b.height,be.id,attrs.index`

func TestOPS818NativeCashCapture(t *testing.T) {
	root := os.Getenv("H_CAPTURE_DIR")
	if root == "" {
		t.Fatal("private root required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-native-cash-01bbba7-readonly", "jit=off", "statement_timeout=30s", "lock_timeout=2s", "effective_io_concurrency=200")
	if err != nil {
		t.Fatal("native source connection failed")
	}
	s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	s.readOnlyLedger = true
	archive, err := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
	if err != nil {
		t.Fatal("archive config failed")
	}
	base := &dbFlowLoader{s: s, chain: apiMainnet, private: true, archive: archive, mu: &sync.Mutex{}}
	seen := map[string]bool{}
	var scope struct {
		Validators []string
		AsOf       string
	}
	data, err := os.ReadFile(filepath.Join(root, "scope.private.json"))
	if err != nil || json.Unmarshal(data, &scope) != nil {
		t.Fatal("scope unavailable")
	}
	end, err := time.Parse(time.DateOnly, scope.AsOf)
	if err != nil || len(scope.Validators) == 0 {
		t.Fatal("invalid source scope")
	}
	var owner struct{ PID int }
	state, err := os.ReadFile("/Users/danb/.cache/ops818-handoff/material-capture-state.json")
	if err != nil || json.Unmarshal(state, &owner) != nil || owner.PID <= 0 {
		t.Fatal("source owner unavailable")
	}
	for {
		dirs, _ := filepath.Glob(filepath.Join(root, "validator-*"))
		found := 0
		for _, dir := range dirs {
			data, err := os.ReadFile(filepath.Join(dir, "openings.private.jsonl"))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				if line == "" {
					continue
				}
				var row struct {
					Address string
					Day     time.Time
				}
				if json.Unmarshal([]byte(line), &row) != nil {
					continue
				}
				name := filepath.Join(dir, walletFile(row.Address))
				if _, err = os.Stat(name + ".events.private.json"); err != nil {
					continue
				}
				found++
				if seen[name] {
					continue
				}
				seen[name] = true
				start := row.Day.AddDate(0, 0, 1)
				began := time.Now()
				var msg, block []nativeCashAttribute
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					if x := tx.Exec("SET LOCAL statement_timeout='30s'").Error; x != nil {
						return x
					}
					{
						var plan []string
						if x := tx.Raw("EXPLAIN "+nativeCashMessages, row.Address, row.Address, start, end).Scan(&plan).Error; x != nil {
							return x
						}
						_ = privateJSON(name+".native-message-plan.private.json", plan)
						if strings.Contains(strings.Join(plan, "\n"), "Seq Scan on message_event_attributes") {
							return fmt.Errorf("native source access path would scan attribute history")
						}
						plan = nil
						if x := tx.Raw("EXPLAIN "+nativeCashBlocks, row.Address, row.Address, start, end).Scan(&plan).Error; x != nil {
							return x
						}
						_ = privateJSON(name+".native-block-plan.private.json", plan)
						if strings.Contains(strings.Join(plan, "\n"), "Seq Scan on block_event_attributes") {
							return fmt.Errorf("native source access path would scan block attribute history")
						}
					}
					if x := tx.Raw(nativeCashMessages, row.Address, row.Address, start, end).Scan(&msg).Error; x != nil {
						return x
					}
					return tx.Raw(nativeCashBlocks, row.Address, row.Address, start, end).Scan(&block).Error
				}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
				cancel()
				if err != nil && strings.Contains(err.Error(), "access path would scan") {
					t.Fatal("unsafe source plan rejected; no later wallets executed")
				}
				result := map[string]any{"address": row.Address, "since": start, "asof_exclusive": end, "message_attributes": msg, "block_attributes": block, "source_error": fmt.Sprint(err), "elapsed_ms": time.Since(began).Milliseconds()}
				if err == nil {
					h, k, e := base.holdingsAt(row.Address, end.AddDate(0, 0, -1))
					result["closing"] = h
					result["closing_known"] = k
					result["closing_error"] = fmt.Sprint(e)
					result["archive"] = archive.evidence(map[string]bool{row.Address: true})
				}
				if x := privateJSON(name+".native-cash.private.json", result); x != nil {
					t.Fatal("native receipt save failed")
				}
				_ = privateAppend(filepath.Join(root, "native-cash-progress.private.jsonl"), map[string]any{"ordinal": len(seen), "source_failed": err != nil, "at": time.Now().UTC()})
			}
		}
		count := 0
		if b, e := os.ReadFile(filepath.Join(root, "cohort-progress.private.jsonl")); e == nil {
			count = len(strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' }))
		}
		if count == len(scope.Validators) && found == len(seen) {
			break
		}
		if syscall.Kill(owner.PID, 0) != nil {
			t.Fatal("source capture ended before complete cohort; all receipts retained")
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("all captured wallet native source receipts retained", len(seen))
}
