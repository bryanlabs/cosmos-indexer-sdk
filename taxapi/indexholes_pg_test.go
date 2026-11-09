package taxapi

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A failed vote rejected by the ante handler at one height and included again
// at the next was recorded as a hole at the first. The recheck drops it and
// keeps a hole whose transaction really is missing.
func TestRecheckIndexHolesPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres index hole verification")
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

	dir := t.TempDir()
	vote, kept, lost := "tx-vote "+backTestSigner, "tx-kept "+backTestSigner, "tx-lost "+backTestGranter
	cacheTxs := func(height int64, txs []string, results []map[string]any) {
		h := strconv.FormatInt(height, 10)
		raw := make([]string, len(txs))
		for i, x := range txs {
			raw[i] = b64(x)
		}
		writeCached(t, dir, height, "block", map[string]any{"block": map[string]any{"header": map[string]any{"height": h}, "data": map[string]any{"txs": raw}}})
		writeCached(t, dir, height, "block_results", map[string]any{"height": h, "txs_results": results})
	}
	charged := []event{{Type: "coin_spent", Attributes: []attr{{"spender", backTestSigner}, {"amount", "900uatom"}}}}
	cacheTxs(100, []string{vote, kept}, []map[string]any{{"code": 11, "events": []event{}}, {"code": 0, "events": charged}})
	cacheTxs(101, []string{vote}, []map[string]any{{"code": 11, "events": charged}})
	cacheTxs(200, []string{lost}, []map[string]any{{"code": 0, "events": []event{{Type: "coin_spent", Attributes: []attr{{"spender", backTestGranter}, {"amount", "900uatom"}}}}}})

	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE chains (id bigint, chain_id text)",
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, tx_indexed boolean, block_events_indexed boolean)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint, hash text, code bigint)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint)",
		"CREATE TEMP TABLE tax_index_holes (chain text, height bigint, chain_txs bigint, addresses_known boolean NOT NULL DEFAULT false, PRIMARY KEY (chain, height))",
		"CREATE TEMP TABLE tax_index_hole_addresses (chain text, height bigint, address text, PRIMARY KEY (chain, height, address))",
		"INSERT INTO chains VALUES (1,'cosmoshub-4')",
		"INSERT INTO blocks VALUES (1,1,100,true,true),(2,1,101,true,true),(3,1,200,true,true)",
		// The vote's one row is at the height that charged its fee.
		"INSERT INTO txes VALUES (1,1,'" + txHash(kept) + "',0),(2,2,'" + txHash(vote) + "',11)",
		"INSERT INTO messages VALUES (1,1),(2,2)",
		"INSERT INTO tax_index_holes VALUES ('mainnet',100,2,true),('mainnet',200,1,false)",
		"INSERT INTO tax_index_hole_addresses VALUES ('mainnet',100,'" + backTestSigner + "')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}

	dropped, left, err := s.RecheckIndexHoles(apiMainnet, &ledgerNode{cache: dir}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	require.Equal(t, 1, left)
	var holes []IndexHole
	require.NoError(t, database.Order("height").Find(&holes).Error)
	require.Equal(t, []IndexHole{{Chain: apiMainnet, Height: 200, ChainTxs: 1, AddressesKnown: true}}, holes)
	var addrs []string
	require.NoError(t, database.Model(&IndexHoleAddress{}).Order("height, address").Pluck("address", &addrs).Error)
	require.Equal(t, []string{backTestGranter}, addrs, "the dropped hole's accounts go with it and the kept one names its own")
}
