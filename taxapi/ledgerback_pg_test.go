package taxapi

import (
	"context"
	"encoding/base64"
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
	pgHeightKey  = "height"
	pgSenderKey  = "sender"
	pgResultsKey = "txs_results"
)

type pgTxResult struct {
	Code      uint32  `json:"code"`
	Codespace string  `json:"codespace,omitempty"`
	Events    []event `json:"events"`
}

// A dry run of the backward scan over Gaia v6 blocks plans every chunk and
// writes nothing; an apply needs a new backup, records what the plan said,
// keeps recorded rows, and refuses a chunk that disagrees with one.
func TestExtendLedgerScanBackPreEventsPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres ledger scan verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database = database.WithContext(ctx)

	fc := feeCollectorAddress()
	dir := t.TempDir()
	cache := func(height int64, txs [][]byte, results []pgTxResult, begin []event) {
		h := strconv.FormatInt(height, 10)
		raw := make([]string, len(txs))
		for i, x := range txs {
			raw[i] = base64.StdEncoding.EncodeToString(x)
		}
		var blk cachedBlockBody
		blk.Block.Header.Height, blk.Block.Data.Txs = h, raw
		writeCached(t, dir, height, cachedBlock, blk)
		writeCached(t, dir, height, "block_results", map[string]any{pgHeightKey: h, pgResultsKey: results, "begin_block_events": begin})
	}
	collected := func(fees string) []event {
		return []event{
			{Type: eventCoinReceived, Attributes: []attr{{attrReceiver, fc}, {apiAmount, testMintAmount}}},
			{Type: eventCoinSpent, Attributes: []attr{{attrSpender, fc}, {apiAmount, fees}}},
		}
	}
	okA := protoTx(t, "a", "100uatom", "")
	failB := protoTx(t, "b", "50uatom", backTestGranter)
	seqC := protoTx(t, "c", "30uatom", "")
	failD := protoTx(t, "d", "40uatom", "")
	failE := protoTx(t, "e", "40uatom", "")
	okF := protoTx(t, "f", "10uatom", "")
	lostG := protoTx(t, "g", "20uatom", "")
	ok := pgTxResult{Events: []event{{Type: "message", Attributes: []attr{{pgSenderKey, backTestSigner}}}}}
	// Each block's BeginBlock collects the fees of the block before it.
	// 9000002: B's staking message failed after its fee was charged, C had a
	// wrong sequence; 9000003 collected 100 + 50.
	cache(9000002, [][]byte{okA, failB, seqC}, []pgTxResult{ok, {Code: 19, Codespace: apiStaking}, {Code: 32, Codespace: codespaceSDK}}, collected("1040uatom"))
	cache(9000003, nil, nil, collected("1150uatom"))
	// 9000001: one of two equal failed fees was charged, so neither is known.
	cache(9000001, [][]byte{failD, failE}, []pgTxResult{{Code: 5, Codespace: codespaceSDK}, {Code: 5, Codespace: codespaceSDK}}, collected("1030uatom"))
	// 9000000: G ran out of gas after paying 20 and is missing from the
	// index.
	cache(9000000, [][]byte{okF, lostG}, []pgTxResult{ok, {Code: 11, Codespace: codespaceSDK}}, nil)

	for _, sql := range []string{
		tempSearchPath,
		tempChainsTable,
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, tx_indexed boolean, block_events_indexed boolean)",
		tempTxesTable,
		tempMsgsTable,
		tempHolesKeyed,
		tempHoleAddrsKeyed,
		tempInclusionsTable,
		tempClearancesTable,
		"CREATE TEMP TABLE tax_failed_tx_fees (tx_id bigint PRIMARY KEY, charged text)",
		"CREATE TEMP TABLE tax_fee_grants (tx_id bigint PRIMARY KEY, granter text)",
		"CREATE TEMP TABLE tax_ledger_scans (chain text PRIMARY KEY, fee_grants_from bigint, fee_grants_to bigint)",
		tempHubChainRow,
		"INSERT INTO blocks VALUES (1,1,9000000,true,true),(2,1,9000001,true,true),(3,1,9000002,true,true),(4,1,9000003,true,true)",
		"INSERT INTO txes VALUES (1,3,'" + txHash(string(okA)) + "',0),(2,3,'" + txHash(string(failB)) + "',19),(3,3,'" + txHash(string(seqC)) + "',32)," +
			"(4,2,'" + txHash(string(failD)) + "',5),(5,2,'" + txHash(string(failE)) + "',5),(6,1,'" + txHash(string(okF)) + "',0)",
		"INSERT INTO messages VALUES (1,1),(2,6)",
		"INSERT INTO tax_ledger_scans VALUES ('mainnet',9000003,9000003)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}
	node := &ledgerNode{cache: dir}
	count := func(table string) int64 {
		var n int64
		require.NoError(t, database.Table(table).Count(&n).Error)
		return n
	}
	span := func() int64 {
		scan, err := s.ledgerScan(apiMainnet)
		require.NoError(t, err)
		return scan.FeeGrantsFrom
	}

	_, _, err = s.ExtendLedgerScanBack(apiMainnet, node, hubGenesisHeight-1, 2, 2, LedgerBackOptions{}, t.Logf)
	require.ErrorContains(t, err, "first block of cosmoshub-4")

	planPath := filepath.Join(t.TempDir(), "dry-run.jsonl")
	from, sum, err := s.ExtendLedgerScanBack(apiMainnet, node, 9000000, 2, 2, LedgerBackOptions{BackupPath: planPath}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, int64(9000000), from)
	require.Equal(t, int64(3), sum.Blocks)
	require.Equal(t, 2, sum.Fees, "B charged, C rejected; D and E unknown")
	require.Equal(t, 1, sum.FeesCharged)
	require.Equal(t, 1, sum.Grants)
	require.Equal(t, 1, sum.Holes)
	require.Equal(t, 1, sum.PreEvents.UnresolvedBlocks)
	require.Equal(t, 2, sum.PreEvents.FailedUnknown)
	require.Equal(t, int64(9000003), span(), "a dry run moves nothing")
	for _, table := range []string{"tax_failed_tx_fees", "tax_fee_grants", "tax_index_holes", "tax_index_hole_addresses"} {
		require.Zero(t, count(table), table)
	}

	_, _, err = s.ExtendLedgerScanBack(apiMainnet, node, 9000000, 2, 2, LedgerBackOptions{Apply: true}, t.Logf)
	require.ErrorContains(t, err, "requires --backup")
	_, _, err = s.ExtendLedgerScanBack(apiMainnet, node, 9000000, 2, 2, LedgerBackOptions{Apply: true, BackupPath: planPath}, t.Logf)
	require.ErrorContains(t, err, "file exists")

	// A recorded row that disagrees stops the apply before it writes.
	require.NoError(t, database.Exec("INSERT INTO tax_failed_tx_fees VALUES (2,'')").Error)
	_, _, err = s.ExtendLedgerScanBack(apiMainnet, node, 9000000, 2, 2, LedgerBackOptions{Apply: true, BackupPath: filepath.Join(t.TempDir(), "refused.jsonl")}, t.Logf)
	require.ErrorContains(t, err, "disagrees")
	require.Equal(t, int64(9000003), span())
	require.Equal(t, int64(1), count("tax_failed_tx_fees"))
	require.NoError(t, database.Exec("UPDATE tax_failed_tx_fees SET charged = '50uatom' WHERE tx_id = 2").Error)

	backupPath := filepath.Join(t.TempDir(), "apply.jsonl")
	from, sum, err = s.ExtendLedgerScanBack(apiMainnet, node, 9000000, 2, 2, LedgerBackOptions{Apply: true, BackupPath: backupPath}, t.Logf)
	require.NoError(t, err)
	require.Equal(t, int64(9000000), from)
	require.Equal(t, 1, sum.ExistingSame)
	require.Equal(t, int64(9000000), span())
	var fees []FailedTxFee
	require.NoError(t, database.Order("tx_id").Find(&fees).Error)
	require.Equal(t, []FailedTxFee{{TxID: 2, Charged: "50uatom"}, {TxID: 3, Charged: ""}}, fees)
	var grants []FeeGrantUse
	require.NoError(t, database.Find(&grants).Error)
	require.Equal(t, []FeeGrantUse{{TxID: 2, Granter: backTestGranter}}, grants)
	var holes []IndexHole
	require.NoError(t, database.Find(&holes).Error)
	require.Equal(t, []IndexHole{{Chain: apiMainnet, Height: 9000000, ChainTxs: 2, AddressesKnown: true}}, holes)

	f, err := os.Open(backupPath)
	require.NoError(t, err)
	defer f.Close()
	stat, err := f.Stat()
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
	dec := json.NewDecoder(f)
	var first, second ledgerBackChunk
	require.NoError(t, dec.Decode(&first))
	require.NoError(t, dec.Decode(&second))
	require.True(t, first.Apply)
	require.Equal(t, int64(9000003), first.FromBefore)
	require.Equal(t, int64(9000001), first.FromAfter)
	require.Equal(t, int64(9000000), second.FromAfter)
	require.Equal(t, []ledgerBackHole{{Height: 9000000, ChainTxs: 2, Addresses: []string{backTestSigner}}}, second.Holes)
}
