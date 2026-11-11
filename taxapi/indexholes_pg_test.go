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

const (
	cachedBlock     = "block"
	coinSpent       = "coin_spent"
	spender         = "spender"
	tempSearchPath  = "SET search_path = pg_temp"
	tempChainsTable = "CREATE TEMP TABLE chains (id bigint, chain_id text)"
	tempTxesTable   = "CREATE TEMP TABLE txes (id bigint, block_id bigint, hash text, code bigint)"
	tempMsgsTable   = "CREATE TEMP TABLE messages (id bigint, tx_id bigint)"
)

type cachedTxResult struct {
	Code   uint32  `json:"code"`
	Events []event `json:"events"`
}

type cachedBlockBody struct {
	Block struct {
		Header struct {
			Height string `json:"height"`
		} `json:"header"`
		Data struct {
			Txs []string `json:"txs"`
		} `json:"data"`
	} `json:"block"`
}

type cachedBlockResults struct {
	Height  string           `json:"height"`
	Results []cachedTxResult `json:"txs_results"`
}

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
	cacheTxs := func(height int64, txs []string, results []cachedTxResult) {
		h := strconv.FormatInt(height, 10)
		raw := make([]string, len(txs))
		for i, x := range txs {
			raw[i] = b64(x)
		}
		var blk cachedBlockBody
		blk.Block.Header.Height, blk.Block.Data.Txs = h, raw
		writeCached(t, dir, height, cachedBlock, blk)
		writeCached(t, dir, height, "block_results", cachedBlockResults{Height: h, Results: results})
	}
	fee := func(payer string) []event {
		return []event{{Type: coinSpent, Attributes: []attr{{spender, payer}, {apiAmount, "900uatom"}}}}
	}
	cacheTxs(100, []string{vote, kept}, []cachedTxResult{{Code: 11, Events: []event{}}, {Events: fee(backTestSigner)}})
	cacheTxs(101, []string{vote}, []cachedTxResult{{Code: 11, Events: fee(backTestSigner)}})
	cacheTxs(200, []string{lost}, []cachedTxResult{{Events: fee(backTestGranter)}})

	for _, sql := range []string{
		tempSearchPath,
		tempChainsTable,
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, tx_indexed boolean, block_events_indexed boolean)",
		tempTxesTable,
		tempMsgsTable,
		"CREATE TEMP TABLE tax_index_holes (chain text, height bigint, chain_txs bigint, addresses_known boolean NOT NULL DEFAULT false, PRIMARY KEY (chain, height))",
		"CREATE TEMP TABLE tax_index_hole_addresses (chain text, height bigint, address text, PRIMARY KEY (chain, height, address))",
		"INSERT INTO chains VALUES (1,'cosmoshub-4')",
		"INSERT INTO blocks VALUES (1,1,100,true,true),(2,1,101,true,true),(3,1,200,true,true),(4,1,201,true,true)",
		// The vote's one row is at the height that charged its fee.
		"INSERT INTO txes VALUES (1,1,'" + txHash(kept) + "',0),(2,2,'" + txHash(vote) + "',11),(3,4,'" + txHash(lost) + "',0),(4,3,'FILLER-A',0),(5,3,'FILLER-B',0)",
		"INSERT INTO messages VALUES (1,1),(2,2),(3,3),(4,4),(5,5)",
		"INSERT INTO tax_index_holes VALUES ('mainnet',100,2,true),('mainnet',200,1,false)",
		"INSERT INTO tax_index_hole_addresses VALUES ('mainnet',100,'" + backTestSigner + "')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}

	// At 200 the count exceeds the expected count and all stored successful
	// transactions have messages, but the successful source hash is at 201.
	for _, count := range []int{1, 2} {
		require.NoError(t, database.Exec("UPDATE tax_index_holes SET chain_txs = ? WHERE height = 200", count).Error)
		cleared, err := s.clearIndexHoles(apiMainnet, []int64{1})
		require.NoError(t, err)
		require.Zero(t, cleared, "neither equal nor excess counts prove membership")
		var holes int64
		require.NoError(t, database.Model(&IndexHole{}).Count(&holes).Error)
		require.Equal(t, int64(2), holes)
		var addresses int64
		require.NoError(t, database.Model(&IndexHoleAddress{}).Count(&addresses).Error)
		require.Equal(t, int64(1), addresses)
	}

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
	n, first, err := s.indexHoles(apiMainnet, backTestGranter, 0, 300)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Equal(t, int64(200), first)

	// Repair only this synthetic fixture. Membership is now correct, but a
	// sweep still cannot attest it. Only the explicit recheck removes it.
	require.NoError(t, database.Exec("UPDATE txes SET block_id = 3 WHERE id = 3").Error)
	cleared, err := s.clearIndexHoles(apiMainnet, []int64{1})
	require.NoError(t, err)
	require.Zero(t, cleared)
	dropped, left, err = s.RecheckIndexHoles(apiMainnet, &ledgerNode{cache: dir}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	require.Zero(t, left)
	require.NoError(t, database.Find(&holes).Error)
	require.Empty(t, holes)
	require.NoError(t, database.Model(&IndexHoleAddress{}).Pluck("address", &addrs).Error)
	require.Empty(t, addrs)
}
