package db

import (
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMessageEvidenceUpsertPreservesBytes(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dryRunPostgresDSN}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	stmt := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tx_id"}, {Name: "message_index"}}, DoUpdates: messageEvidenceUpdates()}).Create(&models.Message{TxID: 7, MessageIndex: 2}).Statement
	if stmt.Error != nil {
		t.Fatal(stmt.Error)
	}
	sql := stmt.SQL.String()
	if !strings.Contains(sql, `"message_bytes"=COALESCE(EXCLUDED.message_bytes, messages.message_bytes)`) {
		t.Fatalf("raw evidence can be erased: %s", sql)
	}
	if !strings.Contains(sql, `ON CONFLICT ("tx_id","message_index")`) {
		t.Fatalf("message identity changed: %s", sql)
	}
}
