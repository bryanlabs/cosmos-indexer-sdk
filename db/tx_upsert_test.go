package db

import (
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestTxUpsertKeepsSuccessfulCopy(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dryRunPostgresDSN}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	stmt := db.Clauses(txUpsertConflict()).Create(&models.Tx{Hash: "AB", Code: 32, BlockID: 9}).Statement
	if stmt.Error != nil {
		t.Fatal(stmt.Error)
	}
	sql := stmt.SQL.String()
	for _, want := range []string{
		`ON CONFLICT ("hash") DO UPDATE SET`,
		`"code"=CASE WHEN txes.code = 0 AND EXCLUDED.code <> 0 THEN txes.code ELSE EXCLUDED.code END`,
		`"block_id"=CASE WHEN txes.code = 0 AND EXCLUDED.code <> 0 THEN txes.block_id ELSE EXCLUDED.block_id END`,
		`RETURNING "id"`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %q in %s", want, sql)
		}
	}
}
