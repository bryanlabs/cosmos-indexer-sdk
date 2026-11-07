package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

func TestFeePayerCorrectionPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres fee replay verification")
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
	require.NoError(t, database.Exec(`CREATE TEMP TABLE fees (
 id bigserial PRIMARY KEY, tx_id bigint NOT NULL, denomination_id bigint NOT NULL,
 amount numeric(78,0), payer_address_id bigint NOT NULL,
 UNIQUE (tx_id, denomination_id))`).Error)
	original := models.Fee{TxID: 7, DenominationID: 11, PayerAddressID: 1, Amount: decimal.NewFromInt(17)}
	require.NoError(t, database.Omit(clause.Associations).Create(&original).Error)
	corrected := models.Fee{TxID: 7, DenominationID: 11, PayerAddressID: 2, Amount: decimal.NewFromInt(17)}
	require.NoError(t, database.Omit(clause.Associations).Create(&corrected).Error)
	var fees []models.Fee
	require.NoError(t, database.Find(&fees).Error)
	require.Len(t, fees, 1, "replay must not duplicate a fee")
	require.Equal(t, uint(2), fees[0].PayerAddressID, "the actual fee granter must replace prior signer attribution")
	require.True(t, fees[0].Amount.Equal(original.Amount))
	var count int64
	require.NoError(t, database.Model(&models.Fee{}).Where("payer_address_id = ?", 1).Count(&count).Error)
	require.Zero(t, count, "the authorizing signer no longer bears the expense")
	require.NoError(t, database.Model(&models.Fee{}).Where("payer_address_id = ?", 2).Count(&count).Error)
	require.Equal(t, int64(1), count)
}
