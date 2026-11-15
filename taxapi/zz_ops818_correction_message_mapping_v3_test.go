//go:build ops818correctionbridgev3

package taxapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOPS818CorrectionMessageMappingV3(t *testing.T) {
	root := os.Getenv("OPS818_CORRECTION_MAPPING_ROOT")
	input := os.Getenv("OPS818_CORRECTION_MAPPING_REQUEST")
	if root == "" || input == "" {
		t.Skip("explicit private mapping only")
	}
	raw, e := os.ReadFile(input)
	if e != nil {
		t.Fatal("mapping request unavailable")
	}
	var req struct {
		Unresolved []struct {
			ID     uint   `json:"message_id"`
			Index  int    `json:"expected_native_msg_index"`
			Height int64  `json:"expected_height"`
			Hash   string `json:"expected_tx_hash"`
		} `json:"unresolved_message_mappings"`
	}
	if json.Unmarshal(raw, &req) != nil || len(req.Unresolved) != 6 {
		t.Fatal("six reward membership mappings required")
	}
	ids := []uint{}
	seen := map[uint]bool{}
	for _, r := range req.Unresolved {
		if !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatal("two exact message IDs required")
	}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-correction-message-mapping default_transaction_read_only=on"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("readonly transport unavailable")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	tx := db.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if tx.Error != nil {
		t.Fatal("readonly snapshot unavailable")
	}
	defer tx.Rollback()
	for _, q := range []string{"SET LOCAL statement_timeout='30s'", "SET LOCAL lock_timeout='2s'"} {
		if tx.Exec(q).Error != nil {
			t.Fatal("source controls failed")
		}
	}
	type controls struct {
		Readonly string `json:"readonly"`
		Snapshot string `json:"snapshot"`
		Assigned *int64 `json:"assigned_xid"`
		Database string `json:"database"`
	}
	var before, after controls
	control := func(c *controls) error {
		return tx.Raw("SELECT current_setting('transaction_read_only') AS readonly, txid_current_snapshot()::text AS snapshot, txid_current_if_assigned() AS assigned, current_database() AS database").Scan(c).Error
	}
	if control(&before) != nil || before.Readonly != "on" || before.Assigned != nil || before.Database != "cosmostax" {
		t.Fatal("source read controls rejected")
	}
	var messages []models.Message
	if tx.Preload("Tx.Block").Where("id IN ?", ids).Find(&messages).Error != nil || len(messages) != 2 {
		t.Fatal("exact source mapping failed")
	}
	rows := []map[string]any{}
	for _, original := range messages {
		var want int = -1
		for _, r := range req.Unresolved {
			if r.ID == original.ID {
				if original.Tx.Block.Height != r.Height || original.Tx.Hash != r.Hash {
					t.Fatal("original source transaction mismatch")
				}
				if want != -1 && want != r.Index {
					t.Fatal("ambiguous native message index")
				}
				want = r.Index
			}
		}
		if want < 0 {
			t.Fatal("unscoped original source message")
		}
		var actual models.Message
		if tx.Preload("Tx.Block").Preload("MessageType").Where("tx_id = ? AND message_index = ?", original.TxID, want).First(&actual).Error != nil {
			t.Fatal("actual native-index source message unavailable")
		}
		if actual.TxID != original.TxID || actual.Tx.Hash != original.Tx.Hash || actual.Tx.Block.Height != original.Tx.Block.Height || actual.MessageIndex != want {
			t.Fatal("actual native-index transaction mismatch")
		}
		rows = append(rows, map[string]any{"original_message_id": original.ID, "original_stored_message_index": original.MessageIndex, "message_id": actual.ID, "native_msg_index": actual.MessageIndex, "height": actual.Tx.Block.Height, "tx_hash": actual.Tx.Hash, "message_type": actual.MessageType.MessageType, "message_bytes_sha256": fmt.Sprintf("%x", sha256.Sum256(actual.MessageBytes)), "original_source_message_unchanged": true})
	}
	if control(&after) != nil || before != after {
		t.Fatal("snapshot controls changed")
	}
	f, e := os.OpenFile(filepath.Join(root, "mapping.private.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal("immutable mapping output failed")
	}
	defer f.Close()
	if json.NewEncoder(f).Encode(map[string]any{"rows": rows, "controls": after, "request_sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "financially_accepted": false, "native_cursor_mapping_matches": true}) != nil || f.Sync() != nil {
		t.Fatal("mapping output failed")
	}

	fmt.Printf("phase actual_message_mapping_complete count %d\n", len(rows))
}
