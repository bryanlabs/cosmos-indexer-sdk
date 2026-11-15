//go:build ops818correctionbridgev1

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

func TestOPS818CorrectionMessageMappingV1(t *testing.T) {
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
	for _, m := range messages {
		for _, r := range req.Unresolved {
			if r.ID == m.ID && (m.MessageIndex != r.Index || m.Tx.Block.Height != r.Height || m.Tx.Hash != r.Hash) {
				t.Fatal("native/source mapping mismatch")
			}
		}
		rows = append(rows, map[string]any{"message_id": m.ID, "native_msg_index": m.MessageIndex, "height": m.Tx.Block.Height, "tx_hash": m.Tx.Hash, "message_bytes_sha256": fmt.Sprintf("%x", sha256.Sum256(m.MessageBytes))})
	}
	if control(&after) != nil || before != after {
		t.Fatal("snapshot controls changed")
	}
	f, e := os.OpenFile(filepath.Join(root, "mapping.private.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal("immutable mapping output failed")
	}
	defer f.Close()
	if json.NewEncoder(f).Encode(map[string]any{"rows": rows, "controls": after, "request_sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "financially_accepted": false}) != nil || f.Sync() != nil {
		t.Fatal("mapping output failed")
	}
	fmt.Printf("phase actual_message_mapping_complete count %d\n", len(rows))
}
