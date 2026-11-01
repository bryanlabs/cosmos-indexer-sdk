package taxapi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Connection-local temporary tables exercise the actual readiness SQL without
// reading, migrating or mutating any persistent application schema.
func TestReadinessRetainedEvidencePostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres readiness verification")
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
	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE chains (id bigint, chain_id text)",
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, time_stamp timestamptz)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint, hash text, code bigint)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint, opaque boolean)",
		"CREATE TEMP TABLE message_parser_errors (message_id bigint)",
		"CREATE TEMP TABLE failed_messages (tx_id bigint)",
		"CREATE TEMP TABLE failed_txes (block_id bigint, hash text)",
		"CREATE TEMP TABLE addresses (id bigint, address text)",
		"CREATE TEMP TABLE tx_signer_addresses (tx_id bigint, address_id bigint)",
		"CREATE TEMP TABLE fees (tx_id bigint, payer_address_id bigint)",
		"CREATE TEMP TABLE taxable_events (message_id bigint, from_addr text, to_addr text, validator_address text)",
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint)",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, value text)",
		"INSERT INTO pg_temp.chains VALUES (1,'cosmoshub-4')",
		"INSERT INTO pg_temp.blocks VALUES (1,1,'2025-06-01T00:00:00Z')",
		"INSERT INTO pg_temp.txes VALUES (1,1,'fixture-tx',0)",
		"INSERT INTO pg_temp.messages VALUES (1,1,false)",
		"INSERT INTO pg_temp.addresses VALUES (1,'cosmos1fixture-wallet')",
		"INSERT INTO pg_temp.tx_signer_addresses VALUES (1,1)",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	server := &Server{db: database}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	wallet := []string{"cosmos1fixture-wallet"}
	check := func(addresses []string, complete bool, parser, decode, opaque int64) {
		t.Helper()
		r, err := server.classificationReadiness(apiMainnet, addresses, start, end)
		require.NoError(t, err)
		require.Equal(t, complete, r.ParserDecodeComplete)
		require.Equal(t, complete, r.ExportReady)
		require.Equal(t, parser, r.ParserErrors)
		require.Equal(t, decode, r.DecodeErrors)
		require.Equal(t, opaque, r.OpaqueMessages)
	}
	check(wallet, true, 0, 0, 0)
	require.NoError(t, database.Exec("UPDATE pg_temp.messages SET opaque=true").Error)
	check(wallet, false, 0, 0, 1)
	check([]string{"cosmos1unrelated-wallet"}, true, 0, 0, 0)
	require.Error(t, server.requireClassificationReady(apiMainnet, wallet, start, end))
	require.NoError(t, database.Exec("UPDATE pg_temp.messages SET opaque=false").Error)
	require.NoError(t, database.Exec("INSERT INTO pg_temp.message_parser_errors VALUES (1)").Error)
	check(wallet, false, 1, 0, 0)
	require.NoError(t, database.Exec("DELETE FROM pg_temp.message_parser_errors").Error)
	require.NoError(t, database.Exec("INSERT INTO pg_temp.failed_messages VALUES (1)").Error)
	check(wallet, false, 0, 1, 0)
	require.NoError(t, database.Exec("INSERT INTO pg_temp.failed_txes VALUES (1,'fixture-tx')").Error)
	check(wallet, false, 0, 1, 0)
	require.NoError(t, database.Exec("DELETE FROM pg_temp.failed_messages").Error)
	require.NoError(t, database.Exec("DELETE FROM pg_temp.failed_txes").Error)
	check(wallet, true, 0, 0, 0)
	require.NoError(t, database.Exec("INSERT INTO pg_temp.failed_txes VALUES (1,'no-retained-transaction')").Error)
	check(nil, false, 0, 1, 0)
	check(wallet, true, 0, 0, 0)
}
