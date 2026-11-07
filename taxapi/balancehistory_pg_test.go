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

// Connection-local temporary tables exercise the ledger's fee and index hole
// SQL without touching any persistent schema.
func TestBalanceLedgerFeesAndHolesPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres ledger verification")
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
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint, hash text, code bigint)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint)",
		"CREATE TEMP TABLE addresses (id bigint, address text)",
		"CREATE TEMP TABLE denoms (id bigint, base text)",
		"CREATE TEMP TABLE fees (tx_id bigint, denomination_id bigint, amount numeric, payer_address_id bigint)",
		"CREATE TEMP TABLE tax_fee_grants (tx_id bigint, granter text)",
		"CREATE TEMP TABLE tax_failed_tx_fees (tx_id bigint, charged text)",
		"CREATE TEMP TABLE tax_index_holes (chain text, height bigint, chain_txs bigint, addresses_known boolean NOT NULL DEFAULT false)",
		"CREATE TEMP TABLE tax_index_hole_addresses (chain text, height bigint, address text)",
		"INSERT INTO blocks VALUES (1,1,10),(2,1,20),(3,1,30)",
		"INSERT INTO addresses VALUES (1,'cosmos1grantee'),(2,'cosmos1granter')",
		"INSERT INTO denoms VALUES (1,'uatom')",
		// tx 1: the grantee's own fee; tx 2: paid by a grant but indexed with the signer as payer.
		"INSERT INTO txes VALUES (1,1,'A',0),(2,2,'B',0),(3,3,'C',0)",
		// tx 1 was indexed without its messages.
		"INSERT INTO messages VALUES (2,2),(3,3)",
		"INSERT INTO fees VALUES (1,1,5,1),(2,1,7,1),(3,1,9,2)",
		"INSERT INTO tax_fee_grants VALUES (2,'cosmos1granter')",
		// The hole at 20 names only the granter; the one at 30 has no address list.
		"INSERT INTO tax_index_holes VALUES ('mainnet',10,1,true),('mainnet',20,1,true),('mainnet',30,2,false)",
		"INSERT INTO tax_index_hole_addresses VALUES ('mainnet',20,'cosmos1granter')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}
	ids := []int64{1}

	grantee, unknown, err := s.ledgerFees(ids, "cosmos1grantee", 0, 100, nil)
	require.NoError(t, err)
	require.Zero(t, unknown)
	require.Len(t, grantee, 1, "a fee a grant paid is not the signer's")
	require.Equal(t, int64(10), grantee[0].Height)

	granter, _, err := s.ledgerFees(ids, "cosmos1granter", 0, 100, nil)
	require.NoError(t, err)
	require.Len(t, granter, 2, "the granter pays the granted fee and its own")
	require.Equal(t, "7", granter[0].Coins.AmountOf("uatom").String())

	n, first, err := s.indexHoles("mainnet", "cosmos1granter", 10, 100)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	require.Equal(t, int64(20), first)
	n, first, err = s.indexHoles("mainnet", "cosmos1grantee", 10, 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the hole at 20 does not name the grantee")
	require.Equal(t, int64(30), first)
	n, _, err = s.indexHoles("mainnet", "cosmos1granter", 30, 100)
	require.NoError(t, err)
	require.Zero(t, n, "the window starts after the date's own block")

	cleared, err := s.clearIndexHoles("mainnet", ids)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared, "block 20 now holds its one transaction, block 30 is still short, block 10 still lacks messages")
	require.NoError(t, database.Exec("INSERT INTO messages VALUES (1,1)").Error)
	cleared, err = s.clearIndexHoles("mainnet", ids)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared, "block 10 now has its messages")
	n, first, err = s.indexHoles("mainnet", "cosmos1granter", 0, 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Equal(t, int64(30), first)
	var addrs int64
	require.NoError(t, database.Raw("SELECT count(*) FROM tax_index_hole_addresses").Scan(&addrs).Error)
	require.Zero(t, addrs, "a cleared hole drops its address list")
}
