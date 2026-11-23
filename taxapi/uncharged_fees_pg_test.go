package taxapi

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A failed transaction the ante handler rejected was charged nothing, so no
// report path may book its declared fee. One that passed the ante handler and
// failed in a message paid its fee, and every path still books it. One with
// no recorded outcome is shown with its declared fee and counted as nothing
// (OPS-810). The fixture lives in a throwaway schema dropped at the end.
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
		// failed in its message; 5 failed with no recorded outcome yet. 6 is
		// a rejected copy whose same bytes ran in another block, stored on
		// the rejected copy's row with the decoded message of the copy that
		// ran (OPS-821), so its fee was charged. 7 was rejected and keeps
		// only an opaque message, which proves nothing ran.
		"INSERT INTO txes(id,block_id,hash,code) VALUES (1,1,'OKTX',0),(2,1,'SEQTX',32),(3,1,'MSGFAILTX',5),(4,1,'ANTEFUNDSTX',5),(5,1,'UNRECORDEDTX',11),(6,1,'RANELSEWHERETX',32),(7,1,'OPAQUESEQTX',32)",
		"INSERT INTO fees(tx_id,amount,denomination_id,payer_address_id) VALUES (1,100,1,1),(2,200,1,1),(3,300,1,1),(4,400,1,1),(5,500,1,1),(6,600,1,1),(7,700,1,1)",
		"INSERT INTO tax_failed_tx_fees(tx_id,charged) VALUES (2,''),(3,'300uatom'),(4,''),(6,''),(7,'')",
		"INSERT INTO message_types(id,message_type) VALUES (1,'/ibc.core.channel.v1.MsgRecvPacket'),(2,'/unknown.v1.MsgOpaque')",
		"INSERT INTO messages(tx_id,message_index,message_type_id,opaque) VALUES (6,0,1,false),(7,0,2,true)",
	} {
		require.NoError(t, database.Exec(q).Error, q)
	}

	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		if r.URL.Path == statementPGDenomsPath {
			_, _ = w.Write([]byte(`{"denoms":{"uatom":{"symbol":"ATOM","decimals":6}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usd":2,"found":true}`))
	}))
	t.Cleanup(oracle.Close)
	s := NewServer(database, NewOracle(oracle.URL, ""), DefaultNativeAsset)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	want := []string{"OKTX", "MSGFAILTX", "UNRECORDEDTX", "RANELSEWHERETX"}
	sort.Strings(want)

	// The database read every report, export, saved report and commission
	// flow starts from.
	input, err := s.readReportInput(apiMainnet, []string{wallet}, start, end, false)
	require.NoError(t, err)
	var hashes []string
	for _, f := range input.Wallets[0].Fees {
		hashes = append(hashes, f.Hash)
		require.Equal(t, f.Hash == "UNRECORDEDTX", f.OutcomeUnknown, f.Hash)
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
		if r.TxHash == "UNRECORDEDTX" {
			require.True(t, r.FeeOutcomeUnknown)
			require.True(t, r.Amount.IsZero())
			require.True(t, r.ValueUSD.IsZero())
			require.Equal(t, "0.0005", r.DeclaredFee)
		} else {
			require.False(t, r.FeeOutcomeUnknown, r.TxHash)
			require.False(t, r.Amount.IsZero(), r.TxHash)
		}
	}
	sort.Strings(hashes)
	require.Equal(t, want, hashes)
	require.Equal(t, 1, readinessForRows(rows).FeeOutcomeUnknown)

	// Tax-software imports, Form 8949, positions and the journal leave the
	// unknown fee out; the generic CSV keeps it with its declared amount.
	var koinly strings.Builder
	require.NoError(t, WriteCSV(&koinly, apiKoinly, rows))
	require.NotContains(t, koinly.String(), "UNRECORDEDTX")
	require.Contains(t, koinly.String(), "MSGFAILTX")
	var generic strings.Builder
	require.NoError(t, WriteCSV(&generic, apiGeneric, rows))
	records, err := csv.NewReader(strings.NewReader(generic.String())).ReadAll()
	require.NoError(t, err)
	unknownAt, declaredAt := -1, -1
	for i, h := range records[0] {
		switch h {
		case apiFeeOutcomeUnknown:
			unknownAt = i
		case "declared_fee":
			declaredAt = i
		}
	}
	require.Positive(t, unknownAt)
	require.Positive(t, declaredAt)
	for _, rec := range records[1:] {
		if rec[1] == "UNRECORDEDTX" {
			require.Equal(t, []string{"true", "0.0005"}, []string{rec[unknownAt], rec[declaredAt]})
		} else {
			require.Equal(t, []string{"false", ""}, []string{rec[unknownAt], rec[declaredAt]})
		}
	}
	var known []Row
	for _, r := range rows {
		if !r.FeeOutcomeUnknown {
			known = append(known, r)
		}
	}
	require.Equal(t, Build8949(known), Build8949(rows))
	price := func(string) (decimal.Decimal, bool) { return decimal.NewFromInt(2), true }
	require.Equal(t, BuildPositions(known, end, price), BuildPositions(rows, end, price))
	journal := BuildJournal(rows, nil)
	require.Len(t, journal, len(BuildJournal(known, nil)))
	for _, l := range journal {
		require.NotEqual(t, "UNRECORDEDTX", l.TxHash)
	}

	// The commission flow trace.
	flows, err := s.flowEvents(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	var total int64
	for _, e := range flows {
		require.Equal(t, apiFee, e.Category)
		total += e.Amount.IntPart()
	}
	require.Equal(t, int64(100+300+600), total)
}

// Before Gaia v6 the block results carry no fee events, so the backfill
// settles each failed transaction from where it failed: the message failure
// paid its fee, the wrong-sequence rejection paid nothing, and the
// out-of-gas panic the gas limit cannot place stays unrecorded, its fee
// shown as of unknown outcome.
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
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}, &FeeGrantUse{}))

	const height = 6000000
	okTx, msgTx, seqTx, gasTx := testTxBytes(t, "ok", 1), testTxBytes(t, "message", 1), testTxBytes(t, "sequence", 1), testTxBytes(t, "gas", 1)
	dir := t.TempDir()
	writeCached(t, dir, height, cachedBlock, map[string]any{
		cachedBlock: map[string]any{
			rpcHeaderKey: map[string]any{rpcHeightKey: "6000000"},
			rpcDataKey:   map[string]any{rpcTxsKey: []string{b64(okTx), b64(msgTx), b64(seqTx), b64(gasTx)}},
		},
	})
	writeCached(t, dir, height, "block_results", map[string]any{
		rpcHeightKey: "6000000",
		rpcTxsResultsKey: []map[string]any{
			{rpcCodeKey: 0, rpcGasWantedKey: "200000"},
			{rpcCodeKey: 13, rpcCodespaceKey: channelCodespace, rpcGasWantedKey: "200000", rpcLogKey: "failed to execute message; message index: 0: packet sequence already received: redundant relay"},
			{rpcCodeKey: 32, rpcCodespaceKey: sdkCodespace, rpcGasWantedKey: "200000", rpcLogKey: sequenceMismatchLog},
			{rpcCodeKey: 11, rpcCodespaceKey: sdkCodespace, rpcGasWantedKey: "50000", rpcLogKey: "out of gas in location: WriteFlat; gasWanted: 50000, gasUsed: 51000: out of gas"},
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
	require.Equal(t, []FailedTxFee{{TxID: 2, Charged: testFee2500}, {TxID: 3, Charged: ""}}, got)

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

// A fee outcome changes what a wallet's report books, but the evidence journal
// does not see tax_failed_tx_fees. Its writers mark stale the ready reports of
// every wallet that paid or granted a fee they record, and only those, and
// the review fingerprint of the payer changes while other wallets' stay.
func TestFeeOutcomeWritersMarkReportsStalePostgres(t *testing.T) {
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
	schema := fmt.Sprintf("fee_outcome_stale_%d", time.Now().UnixNano())
	require.NoError(t, database.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		_ = database.WithContext(context.Background()).Exec("DROP SCHEMA " + schema + " CASCADE").Error
	})
	require.NoError(t, database.Exec("SET search_path = "+schema).Error)
	require.NoError(t, dbpkg.MigrateModels(database))
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}, &ReportJob{}, &FailedTxFee{}, &FeeGrantUse{}))

	payer, granter, other := "cosmos1feeoutcomepayerwallet", "cosmos1feeoutcomegranterwallet", "cosmos1feeoutcomeotherwallet"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES (1,'" + payer + "'),(2,'" + granter + "'),(3,'" + other + "')",
		"INSERT INTO chains(id,chain_id) VALUES (1,'cosmoshub-4')",
		"INSERT INTO denoms(id,base) VALUES (1,'uatom')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES (1,1,1,6000000,'2021-05-01T12:00:00Z',true)",
		// 1 is the payer's own failed transaction, 2 one a grant paid for
		// the payer, 3 the other wallet's successful one, 4 its failed one.
		"INSERT INTO txes(id,block_id,hash,code) VALUES (1,1,'OWNTX',32),(2,1,'GRANTEDTX',5),(3,1,'OTHEROKTX',0),(4,1,'OTHERFAILTX',32)",
		"INSERT INTO fees(tx_id,amount,denomination_id,payer_address_id) VALUES (1,100,1,1),(2,200,1,1),(3,300,1,3),(4,400,1,3)",
		"INSERT INTO tax_fee_grants(tx_id,granter) VALUES (2,'" + granter + "')",
	} {
		require.NoError(t, database.Exec(q).Error, q)
	}
	now := time.Now().UTC().Truncate(time.Second)
	jobs := map[string]string{"payer": payer, "granter": granter, "other": other, "pair": granter + "," + other}
	for key, addresses := range jobs {
		require.NoError(t, database.Create(&ReportJob{Key: key, Chain: apiMainnet, Addresses: addresses, Status: ReportJobReady, ComputedThrough: now}).Error)
	}
	require.NoError(t, database.Create(&ReportJob{Key: "pending", Chain: apiMainnet, Addresses: payer, Status: ReportJobPending, ComputedThrough: now}).Error)
	stale := func() map[string]bool {
		var got []ReportJob
		require.NoError(t, database.Find(&got).Error)
		out := map[string]bool{}
		for _, j := range got {
			out[j.Key] = j.ComputedThrough.Before(now)
		}
		return out
	}
	s := NewServer(database, nil, DefaultNativeAsset)
	fingerprint := func(addr string) string {
		fp, err := s.reviewFingerprintInSnapshot(apiMainnet, []string{addr})
		require.NoError(t, err)
		return fp
	}
	payerBefore, otherBefore := fingerprint(payer), fingerprint(other)

	require.NoError(t, database.Transaction(func(tx *gorm.DB) error {
		return recordFailedTxFees(tx, []FailedTxFee{{TxID: 1, Charged: ""}}, 500)
	}))
	require.Equal(t, map[string]bool{"payer": true, "granter": false, "other": false, "pair": false, "pending": false}, stale())
	require.NotEqual(t, payerBefore, fingerprint(payer))
	require.Equal(t, otherBefore, fingerprint(other))

	// A granted fee marks the granter's reports, the pair among them.
	require.NoError(t, database.Model(&ReportJob{}).Where("1 = 1").Update("computed_through", now).Error)
	require.NoError(t, database.Transaction(func(tx *gorm.DB) error {
		return recordFailedTxFees(tx, []FailedTxFee{{TxID: 2, Charged: "200uatom"}}, 500)
	}))
	require.Equal(t, map[string]bool{"payer": true, "granter": true, "other": false, "pair": true, "pending": false}, stale())

	// A row already recorded changes nothing and marks nothing.
	require.NoError(t, database.Model(&ReportJob{}).Where("1 = 1").Update("computed_through", now).Error)
	require.NoError(t, database.Transaction(func(tx *gorm.DB) error {
		return recordFailedTxFees(tx, []FailedTxFee{{TxID: 1, Charged: "100uatom"}, {TxID: 2, Charged: ""}}, 1)
	}))
	require.Equal(t, map[string]bool{"payer": false, "granter": false, "other": false, "pair": false, "pending": false}, stale())
	var kept []FailedTxFee
	require.NoError(t, database.Order("tx_id").Find(&kept).Error)
	require.Equal(t, []FailedTxFee{{TxID: 1, Charged: ""}, {TxID: 2, Charged: "200uatom"}}, kept)
}
