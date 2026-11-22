package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Cosmos Hub block 8434944 holds a relayer transaction the ante handler
// rejected for its sequence; the same bytes ran two blocks later. Whichever
// copy is indexed last, the row must keep the copy that ran, and the insert
// must still return the row id.
func TestTxUpsertKeepsSuccessfulCopyPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres tx upsert verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	require.NoError(t, database.Exec("SET search_path = pg_temp").Error)
	require.NoError(t, database.Exec(`CREATE TEMP TABLE txes (
 id bigserial PRIMARY KEY, hash text NOT NULL UNIQUE, code bigint, block_id bigint,
 signer_address_id bigint, memo text, timeout_height bigint, extension_options text, non_critical_extension_options text)`).Error)

	upsert := func(hash string, code uint32, block uint) uint {
		tx := models.Tx{Hash: hash, Code: code, BlockID: block}
		require.NoError(t, database.Omit(clause.Associations).Clauses(txUpsertConflict()).Create(&tx).Error)
		require.NotZero(t, tx.ID, "the insert must return the row id")
		return tx.ID
	}
	stored := func(hash string) (code uint32, block uint) {
		var row struct {
			Code    uint32
			BlockID uint
		}
		require.NoError(t, database.Raw("SELECT code, block_id FROM txes WHERE hash = ?", hash).Scan(&row).Error)
		return row.Code, row.BlockID
	}

	// The successful copy first, the rejected copy indexed after it.
	id := upsert("RAN", 0, 102)
	require.Equal(t, id, upsert("RAN", 32, 100))
	code, block := stored("RAN")
	require.Equal(t, uint32(0), code)
	require.Equal(t, uint(102), block)

	// The rejected copy first, the successful copy after it.
	id = upsert("LATER", 32, 200)
	require.Equal(t, id, upsert("LATER", 0, 201))
	code, block = stored("LATER")
	require.Equal(t, uint32(0), code)
	require.Equal(t, uint(201), block)

	// Two failed copies: the newer write wins, as before.
	upsert("BOTH", 32, 300)
	upsert("BOTH", 5, 301)
	code, block = stored("BOTH")
	require.Equal(t, uint32(5), code)
	require.Equal(t, uint(301), block)
}
