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
	"gorm.io/gorm/logger"
)

func TestIndexAddressesPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres address verification")
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
	require.NoError(t, database.Exec(`CREATE TEMP TABLE addresses (id bigserial PRIMARY KEY, address text UNIQUE)`).Error)
	require.NoError(t, database.Exec(`INSERT INTO addresses (address) VALUES ('cosmos1b'), ('cosmos1c')`).Error)
	var existing []models.Address
	require.NoError(t, database.Order("address").Find(&existing).Error)

	committed := newIDCache()
	committed.put("cosmos1c", existing[1].ID)
	ids := newPendingIDCache(committed)
	block := []models.Address{{Address: "cosmos1a"}, {Address: "cosmos1b"}, {Address: "cosmos1c"}}
	require.NoError(t, indexAddresses(database, block, ids))

	var rows []models.Address
	require.NoError(t, database.Order("address").Find(&rows).Error)
	require.Len(t, rows, 3)
	byAddress := map[string]uint{}
	for _, row := range rows {
		byAddress[row.Address] = row.ID
	}
	for _, address := range block {
		require.NotZero(t, address.ID)
		require.Equal(t, byAddress[address.Address], address.ID, address.Address)
	}

	_, ok := committed.get("cosmos1a")
	require.False(t, ok, "an uncommitted ID must not reach other blocks before publish")
	ids.publish()
	id, ok := committed.get("cosmos1a")
	require.True(t, ok)
	require.Equal(t, byAddress["cosmos1a"], id)

	again := []models.Address{{Address: "cosmos1a"}, {Address: "cosmos1b"}}
	require.NoError(t, indexAddresses(database, again, newPendingIDCache(committed)))
	require.Equal(t, byAddress["cosmos1a"], again[0].ID)
	require.Equal(t, byAddress["cosmos1b"], again[1].ID)
}
