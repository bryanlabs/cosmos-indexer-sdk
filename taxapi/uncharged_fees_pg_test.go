package taxapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A failed transaction the ante handler rejected was charged nothing, so no
// report path may book its declared fee. One that passed the ante handler and
// failed in a message paid its fee, and every path still books it. The
// fixture lives in a throwaway schema dropped at the end.
func TestReportFeesSkipUnchargedFailedTxsPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres report fee verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	schema := fmt.Sprintf("uncharged_fees_%d", time.Now().UnixNano())
	require.NoError(t, database.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		_ = database.WithContext(context.Background()).Exec("DROP SCHEMA " + schema + " CASCADE").Error
	})
	require.NoError(t, database.Exec("SET search_path = "+schema).Error)
	require.NoError(t, dbpkg.MigrateModels(database))
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}))

	wallet := "cosmos1unchargedfeefixturewallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES (1,'" + wallet + "')",
		"INSERT INTO chains(id,chain_id) VALUES (1,'cosmoshub-4')",
		"INSERT INTO denoms(id,base) VALUES (1,'uatom')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES (1,1,1,31802699,'2026-05-01T12:00:00Z',true)",
		// 1 succeeded; 2 and 4 the ante handler rejected (wrong sequence,
		// insufficient funds for the fee); 3 passed the ante handler and
		// failed in its message; 5 failed with no recorded outcome yet.
		"INSERT INTO txes(id,block_id,hash,code) VALUES (1,1,'OKTX',0),(2,1,'SEQTX',32),(3,1,'MSGFAILTX',5),(4,1,'ANTEFUNDSTX',5),(5,1,'UNRECORDEDTX',11)",
		"INSERT INTO fees(tx_id,amount,denomination_id,payer_address_id) VALUES (1,100,1,1),(2,200,1,1),(3,300,1,1),(4,400,1,1),(5,500,1,1)",
		"INSERT INTO tax_failed_tx_fees(tx_id,charged) VALUES (2,''),(3,'300uatom'),(4,'')",
	} {
		require.NoError(t, database.Exec(q).Error, q)
	}

	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == "/denoms" {
			_, _ = w.Write([]byte(`{"denoms":{"uatom":{"symbol":"ATOM","decimals":6}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	t.Cleanup(oracle.Close)
	s := NewServer(database, NewOracle(oracle.URL, ""), DefaultNativeAsset)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	want := []string{"OKTX", "MSGFAILTX", "UNRECORDEDTX"}
	sort.Strings(want)

	// The database read every report, export, saved report and commission
	// flow starts from.
	input, err := s.readReportInput(apiMainnet, []string{wallet}, start, end, false)
	require.NoError(t, err)
	var hashes []string
	for _, f := range input.Wallets[0].Fees {
		hashes = append(hashes, f.Hash)
	}
	sort.Strings(hashes)
	require.Equal(t, want, hashes)

	// The priced report rows (preview, CSV and PDF exports, report jobs).
	rows, err := s.rowsFor(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	hashes = hashes[:0]
	for _, r := range rows {
		require.Equal(t, apiFee, r.Category)
		hashes = append(hashes, r.TxHash)
	}
	sort.Strings(hashes)
	require.Equal(t, want, hashes)

	// The commission flow trace.
	flows, err := s.flowEvents(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	var total int64
	for _, e := range flows {
		require.Equal(t, apiFee, e.Category)
		total += e.Amount.IntPart()
	}
	require.Equal(t, int64(100+300+500), total)
}

// Before Gaia v6 the block results carry no fee events, so the backfill
// settles each failed transaction from where it failed: the message failure
// paid its fee, the wrong-sequence rejection paid nothing, and the
// out-of-gas panic the gas limit cannot place stays unrecorded and keeps
// its fee in the report.
func TestRecordFailedTxFeesBeforeCoinEventsPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres report fee verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	schema := fmt.Sprintf("failed_fee_backfill_%d", time.Now().UnixNano())
	require.NoError(t, database.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		_ = database.WithContext(context.Background()).Exec("DROP SCHEMA " + schema + " CASCADE").Error
	})
	require.NoError(t, database.Exec("SET search_path = "+schema).Error)
	require.NoError(t, dbpkg.MigrateModels(database))
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}))

	const height = 6000000
	okTx, msgTx, seqTx, gasTx := testTxBytes(t, "ok", 1), testTxBytes(t, "message", 1), testTxBytes(t, "sequence", 1), testTxBytes(t, "gas", 1)
	dir := t.TempDir()
	writeCached(t, dir, height, "block", map[string]any{
		"block": map[string]any{
			"header": map[string]any{"height": "6000000"},
			"data":   map[string]any{"txs": []string{b64(okTx), b64(msgTx), b64(seqTx), b64(gasTx)}},
		},
	})
	writeCached(t, dir, height, "block_results", map[string]any{
		"height": "6000000",
		"txs_results": []map[string]any{
			{"code": 0, "gas_wanted": "200000"},
			{"code": 13, "codespace": "channel", "gas_wanted": "200000", "log": "failed to execute message; message index: 0: packet sequence already received: redundant relay"},
			{"code": 32, "codespace": "sdk", "gas_wanted": "200000", "log": "account sequence mismatch, expected 7, got 6: incorrect account sequence"},
			{"code": 11, "codespace": "sdk", "gas_wanted": "50000", "log": "out of gas in location: WriteFlat; gasWanted: 50000, gasUsed: 51000: out of gas"},
		},
	})

	wallet := "cosmos1failedfeebackfillfixturewallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES (1,'" + wallet + "')",
		"INSERT INTO chains(id,chain_id) VALUES (1,'cosmoshub-4')",
		"INSERT INTO denoms(id,base) VALUES (1,'uatom')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES (1,1,1,6000000,'2021-05-01T12:00:00Z',true)",
		fmt.Sprintf("INSERT INTO txes(id,block_id,hash,code) VALUES (1,1,'%s',0),(2,1,'%s',13),(3,1,'%s',32),(4,1,'%s',11)", txHash(okTx), txHash(msgTx), txHash(seqTx), txHash(gasTx)),
		"INSERT INTO fees(tx_id,amount,denomination_id,payer_address_id) VALUES (1,2500,1,1),(2,2500,1,1),(3,2500,1,1),(4,2500,1,1)",
	} {
		require.NoError(t, database.Exec(q).Error, q)
	}

	s := NewServer(database, nil, DefaultNativeAsset)
	node := &ledgerNode{cache: dir}
	// A dry run counts without writing.
	n, err := s.RecordFailedTxFees(apiMainnet, node, 5000000, 7000000, 500000, 2, false, t.Logf)
	require.NoError(t, err)
	require.Equal(t, FailedFeeCounts{Charged: 1, Uncharged: 1, Unsettled: 1}, n)
	var count int64
	require.NoError(t, database.Model(&FailedTxFee{}).Count(&count).Error)
	require.Zero(t, count)

	n, err = s.RecordFailedTxFees(apiMainnet, node, 5000000, 7000000, 500000, 2, true, t.Logf)
	require.NoError(t, err)
	require.Equal(t, FailedFeeCounts{Charged: 1, Uncharged: 1, Unsettled: 1}, n)
	var got []FailedTxFee
	require.NoError(t, database.Order("tx_id").Find(&got).Error)
	require.Equal(t, []FailedTxFee{{TxID: 2, Charged: "2500uatom"}, {TxID: 3, Charged: ""}}, got)

	// A rerun records nothing new and leaves the unsettled one unsettled.
	n, err = s.RecordFailedTxFees(apiMainnet, node, 5000000, 7000000, 500000, 2, true, t.Logf)
	require.NoError(t, err)
	require.Equal(t, FailedFeeCounts{Unsettled: 1}, n)

	input, err := s.readReportInput(apiMainnet, []string{wallet}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	var hashes []string
	for _, f := range input.Wallets[0].Fees {
		hashes = append(hashes, f.Hash)
	}
	want := []string{txHash(okTx), txHash(msgTx), txHash(gasTx)}
	sort.Strings(hashes)
	sort.Strings(want)
	require.Equal(t, want, hashes)
}
