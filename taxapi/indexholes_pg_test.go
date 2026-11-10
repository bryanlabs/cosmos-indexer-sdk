package taxapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	var original []IndexHole
	require.NoError(t, database.Order("height").Find(&original).Error)
	var originalAddresses []IndexHoleAddress
	require.NoError(t, database.Order("height, address").Find(&originalAddresses).Error)
	unchanged := func() {
		t.Helper()
		var got []IndexHole
		require.NoError(t, database.Order("height").Find(&got).Error)
		require.Equal(t, original, got)
		var addresses []IndexHoleAddress
		require.NoError(t, database.Order("height, address").Find(&addresses).Error)
		require.Equal(t, originalAddresses, addresses)
	}
	node := &ledgerNode{cache: dir}
	planPath := filepath.Join(t.TempDir(), "dry-run.jsonl")
	dropped, left, err := s.RecheckIndexHoles(apiMainnet, node, RecheckIndexHolesOptions{BackupPath: planPath}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	require.Equal(t, 1, left)
	unchanged()

	_, _, err = s.RecheckIndexHoles(apiMainnet, node, RecheckIndexHolesOptions{Apply: true}, t.Logf)
	require.ErrorContains(t, err, "requires --backup")
	unchanged()
	_, _, err = s.RecheckIndexHoles(apiMainnet, node, RecheckIndexHolesOptions{Apply: true, BackupPath: planPath}, t.Logf)
	require.ErrorContains(t, err, "file exists")
	unchanged()
	_, _, err = s.RecheckIndexHoles(apiMainnet, node, RecheckIndexHolesOptions{Apply: true, BackupPath: filepath.Join(t.TempDir(), "absent", "backup.jsonl")}, t.Logf)
	require.Error(t, err)
	unchanged()

	backupPath := filepath.Join(t.TempDir(), "apply.jsonl")
	dropped, left, err = s.RecheckIndexHoles(apiMainnet, node, RecheckIndexHolesOptions{Apply: true, BackupPath: backupPath}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	require.Equal(t, 1, left)
	f, err := os.Open(backupPath)
	require.NoError(t, err)
	defer f.Close()
	stat, err := f.Stat()
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
	dec := json.NewDecoder(f)
	var first, second indexHoleChange
	require.NoError(t, dec.Decode(&first))
	require.NoError(t, dec.Decode(&second))
	require.Equal(t, original[0], first.Before)
	require.Equal(t, originalAddresses, first.BeforeAddresses)
	require.Nil(t, first.After)
	require.Empty(t, first.AfterAddresses)
	require.Equal(t, original[1], second.Before)
	require.Empty(t, second.BeforeAddresses)
	require.True(t, first.Apply)
	require.Equal(t, 1, first.Version)
	var holes []IndexHole
	require.NoError(t, database.Order("height").Find(&holes).Error)
	require.Equal(t, []IndexHole{{Chain: apiMainnet, Height: 200, ChainTxs: 1, AddressesKnown: true}}, holes)
	require.Equal(t, holes[0], *second.After)
	require.Equal(t, []IndexHoleAddress{{Chain: apiMainnet, Height: 200, Address: backTestGranter}}, second.AfterAddresses)
	var addrs []string
	require.NoError(t, database.Model(&IndexHoleAddress{}).Order("height, address").Pluck("address", &addrs).Error)
	require.Equal(t, []string{backTestGranter}, addrs, "the dropped hole's accounts go with it and the kept one names its own")
}
