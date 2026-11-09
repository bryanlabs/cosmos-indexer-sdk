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
