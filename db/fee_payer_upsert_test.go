package db

import (
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestFeeUpsertRefreshesPayer(t *testing.T) {
	database, err := gorm.Open(postgres.New(postgres.Config{DSN: dryRunPostgresDSN}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	require.NoError(t, err)
	statement := database.Omit(clause.Associations).Create(&models.Fee{TxID: 7, DenominationID: 11, PayerAddressID: 2}).Statement
	require.NoError(t, statement.Error)
	require.True(t, strings.Contains(statement.SQL.String(), `"payer_address_id"="excluded"."payer_address_id"`), "replay must refresh payer ownership: %s", statement.SQL.String())
}
