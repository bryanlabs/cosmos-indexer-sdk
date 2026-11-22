package taxapi

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Keep the uniquely named schema and its evidence rows for audit. All fixture
// writes are confined to that schema; the fee readers run in READ ONLY snapshots.
func effectiveFeesProofDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	database, err := gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	schema := fmt.Sprintf("effective_fees_proof_%d", time.Now().UnixNano())
	require.NoError(t, database.Exec("CREATE SCHEMA "+schema).Error)
	bootstrap, err := database.DB()
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	database, err = gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	for _, sql := range []string{
		"SET statement_timeout='10s'",
		"SET TIME ZONE 'UTC'",
		"CREATE TABLE chains(id bigint PRIMARY KEY,chain_id text)",
		"CREATE TABLE addresses(id bigint PRIMARY KEY,address text UNIQUE)",
		"CREATE TABLE denoms(id bigint PRIMARY KEY,base text)",
		"CREATE TABLE blocks(id bigint PRIMARY KEY,height bigint,chain_id bigint,time_stamp timestamptz)",
		"CREATE UNIQUE INDEX chainheight ON blocks(height,chain_id)",
		"CREATE TABLE txes(id bigint PRIMARY KEY,block_id bigint,hash text,code int)",
		"CREATE INDEX idx_txes_block_id ON txes(block_id)",
		"CREATE TABLE fees(id bigint PRIMARY KEY,payer_address_id bigint,denomination_id bigint,tx_id bigint,amount numeric)",
		"CREATE INDEX idx_payer_addr ON fees(payer_address_id)",
		"CREATE UNIQUE INDEX tx_denom_fee ON fees(tx_id,denomination_id)",
		"CREATE TABLE tax_fee_grants(tx_id bigint PRIMARY KEY,granter text)",
		"CREATE INDEX idx_fee_granter ON tax_fee_grants(granter)",
		"CREATE TABLE tax_failed_tx_fees(tx_id bigint PRIMARY KEY,charged text)",
		"INSERT INTO chains VALUES(1,'cosmoshub-4'),(2,'mainnet'),(3,'foreign')",
		"INSERT INTO addresses VALUES(1,'proof-owner'),(2,'proof-grantee'),(3,'proof-other')",
		"INSERT INTO denoms VALUES(1,'uatom'),(2,'voucher')",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	t.Logf("preserved proof schema: %s", schema)
	return database
}

type effectiveFeesFixtureTrace struct {
	logger.Interface
	query string
}

func (r *effectiveFeesFixtureTrace) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	query, _ := fc()
	if strings.Contains(query, ") attributed ORDER BY") {
		r.query = query
	}
}

func TestEffectiveWalletFeesBoundariesPostgres(t *testing.T) {
	database := effectiveFeesProofDB(t)
	for _, sql := range []string{
		"INSERT INTO chains VALUES(4,'provider-1')",
		"INSERT INTO blocks VALUES(21,1,4,'2026-01-01 23:59Z'),(4,2,4,'2026-01-02 00:00Z'),(900,3,4,'2026-01-02 12:00Z'),(6,4,4,'2026-01-02 23:59Z'),(8,5,4,'2026-01-03 00:00Z'),(5,7,4,'2026-01-04 00:00Z')",
		"INSERT INTO txes VALUES(1,21,'before',0),(2,4,'start',0),(3,900,'grant',0),(4,6,'last',1),(5,8,'end',0)",
		"INSERT INTO fees VALUES(1,1,1,1,1),(2,1,1,2,2),(3,2,1,3,3),(4,1,1,4,400),(5,1,1,5,5)",
		"INSERT INTO tax_fee_grants VALUES(3,'proof-owner')",
		"INSERT INTO tax_failed_tx_fees VALUES(4,'4uatom,6voucher')",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	parse := func(value string) time.Time {
		ts, e := time.Parse(time.RFC3339, value)
		require.NoError(t, e)
		return ts
	}
	trace := &effectiveFeesFixtureTrace{Interface: logger.Default.LogMode(logger.Silent)}
	database = database.Session(&gorm.Session{Logger: trace})
	for _, c := range []struct {
		name, start, end string
		count            int
		bounds           bool
	}{
		{"proven_whole_day", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z", 4, true},
		{"utc_equivalent_offsets", "2026-01-02T01:00:00+01:00", "2026-01-02T19:00:00-05:00", 4, true},
		{"partial_day", "2026-01-02T11:00:00Z", "2026-01-02T13:00:00Z", 1, false},
		{"before_index", "2025-12-30T00:00:00Z", "2026-01-03T00:00:00Z", 5, false},
		{"future_end", "2026-01-02T00:00:00Z", "2026-01-10T00:00:00Z", 5, false},
		{"missing_boundary", "2026-01-02T00:00:00Z", "2026-01-04T00:00:00Z", 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := database.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
			got, e := (&Server{db: tx, readOnlyLedger: true}).walletFees(apiTestnet, "proof-owner", parse(c.start), parse(c.end))
			require.NoError(t, e)
			require.Len(t, got, c.count)
			require.NotEmpty(t, trace.query)
			require.Equal(t, c.bounds, strings.Contains(trace.query, "b.height > 1 AND b.height <= 4"))
			require.Equal(t, c.bounds, strings.Contains(trace.query, "b.height >"))
			if c.bounds {
				require.Equal(t, "start", got[0].Hash)
				require.Equal(t, "last", got[len(got)-1].Hash)
			}
			for _, r := range got {
				require.False(t, r.Ts.Before(parse(c.start)))
				require.True(t, r.Ts.Before(parse(c.end)))
			}
			var unassigned bool
			require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error)
			require.True(t, unassigned)
		})
	}
}

func TestEffectiveWalletFeesPostgres(t *testing.T) {
	database := effectiveFeesProofDB(t)
	parse := func(value string) time.Time {
		ts, err := time.Parse(time.RFC3339, value)
		require.NoError(t, err)
		return ts
	}
	// IDs are deliberately not height order. Transaction IDs and fee IDs within
	// one timestamp are increasing so expected ordering is unambiguous.
	for _, sql := range []string{
		"INSERT INTO blocks VALUES(90,1,1,'2026-01-01 23:59Z'),(4,2,1,'2026-01-02 00:00Z'),(80,3,1,'2026-01-02 12:00Z'),(2,4,1,'2026-01-02 23:59Z'),(70,5,1,'2026-01-03 00:00Z'),(6,6,1,'2026-01-04 12:00Z'),(60,7,1,'2026-01-05 12:00Z'),(8,8,1,'2026-01-06 12:00Z'),(9,3,2,'2026-01-02 12:00Z'),(10,3,3,'2026-01-02 12:00Z')",
		"INSERT INTO txes VALUES(1,90,'before',0),(2,4,'start',0),(3,80,'own-multidenom',0),(4,80,'grant-absent-id',0),(5,80,'grant-self',0),(6,80,'failed-multidenom',5),(7,80,'failed-nocharge',5),(8,2,'last',0),(9,70,'end',0),(10,9,'alias',0),(11,10,'foreign',0),(12,6,'good-before-unresolved',0),(13,6,'unresolved',5),(14,60,'good-before-malformed',0),(15,60,'malformed',5),(16,8,'granted-failed',5),(17,80,'grant-known-owner',0)",
		"INSERT INTO fees VALUES(1,1,1,1,1),(2,1,1,2,2),(3,1,1,3,3),(4,1,2,3,4),(5,2,1,4,5),(6,1,1,5,6),(7,1,1,6,700),(8,1,2,6,800),(9,1,1,7,900),(10,1,1,8,10),(11,1,1,9,11),(12,1,1,10,12),(13,1,1,11,13),(14,1,1,12,14),(15,1,1,13,15),(16,1,1,14,16),(17,1,1,15,17),(18,2,1,16,1800),(19,2,2,16,1900),(20,2,1,17,20)",
		"INSERT INTO tax_fee_grants VALUES(4,'proof-unregistered-granter'),(5,'proof-owner'),(16,'proof-unregistered-granter'),(17,'proof-owner')",
		"INSERT INTO tax_failed_tx_fees VALUES(6,'7uatom,11uosmo'),(7,''),(15,'not-a-coin'),(16,'19uatom,23uosmo')",
		"INSERT INTO blocks VALUES(11,9,1,'2026-01-07 12:00Z')",
		"INSERT INTO txes VALUES(18,11,'null-charged',5)",
		"INSERT INTO fees VALUES(21,1,1,18,2100)",
		"INSERT INTO tax_failed_tx_fees VALUES(18,NULL)",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	read := func(t *testing.T, chain, address string, start, end time.Time, effective, direct bool) ([]feeRec, error) {
		t.Helper()
		tx := database.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
		server := &Server{db: tx, native: DefaultNativeAsset, readOnlyLedger: effective}
		var rows []feeRec
		var err error
		if direct {
			rows, err = server.effectiveWalletFees(chain, address, start, end)
		} else {
			rows, err = server.walletFees(chain, address, start, end)
		}
		var unassigned bool
		require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error)
		require.True(t, unassigned, "fee reads must neither write nor assign a transaction ID")
		for i := range rows {
			rows[i].Ts = rows[i].Ts.UTC()
		}
		return rows, err
	}
	row := func(amount, denom, hash, timestamp string) feeRec {
		return feeRec{Amount: amount, Denom: denom, Hash: hash, Ts: parse(timestamp)}
	}
	start, noon, last := "2026-01-02T00:00:00Z", "2026-01-02T12:00:00Z", "2026-01-02T23:59:00Z"
	expected := []feeRec{
		row("2", "uatom", "start", start),
		row("3", "uatom", "own-multidenom", noon),
		row("4", "voucher", "own-multidenom", noon),
		row("6", "uatom", "grant-self", noon),
		row("7", "uatom", "failed-multidenom", noon),
		row("11", "uosmo", "failed-multidenom", noon),
		row("12", "uatom", "alias", noon),
		row("20", "uatom", "grant-known-owner", noon),
		row("10", "uatom", "last", last),
	}
	// The contract orders timestamps, then transaction IDs and denominations,
	// including transactions from different aliases of the same chain.
	assertRows := func(t *testing.T, want, got []feeRec) {
		t.Helper()
		require.Equal(t, want, got)
		for i := 1; i < len(got); i++ {
			require.False(t, got[i].Ts.Before(got[i-1].Ts), "fees must be chronological")
		}
	}
	t.Run("success_failed_grants_and_stable_order", func(t *testing.T) {
		for _, chain := range []string{"mainnet", "cosmoshub", "cosmoshub-4"} {
			var first []feeRec
			for i := 0; i < 5; i++ {
				got, err := read(t, chain, "proof-owner", parse(start), parse("2026-01-03T00:00:00Z"), true, false)
				require.NoError(t, err)
				assertRows(t, expected, got)
				if i == 0 {
					first = got
				} else {
					require.Equal(t, first, got)
				}
			}
		}
		got, err := read(t, apiMainnet, "proof-owner", parse(start), parse("2026-01-03T00:00:00Z"), true, true)
		require.NoError(t, err)
		assertRows(t, expected, got)
	})
	t.Run("granter_without_address_id_and_grantee_excluded", func(t *testing.T) {
		got, err := read(t, apiMainnet, "proof-unregistered-granter", parse(start), parse("2026-01-03T00:00:00Z"), true, false)
		require.NoError(t, err)
		require.Equal(t, []feeRec{row("5", "uatom", "grant-absent-id", noon)}, got)
		got, err = read(t, apiMainnet, "proof-grantee", parse(start), parse("2026-01-07T00:00:00Z"), true, false)
		require.NoError(t, err)
		require.Empty(t, got)
		got, err = read(t, apiMainnet, "proof-unregistered-granter", parse("2026-01-06T00:00:00Z"), parse("2026-01-07T00:00:00Z"), true, false)
		require.NoError(t, err)
		assertRows(t, []feeRec{row("19", "uatom", "granted-failed", "2026-01-06T12:00:00Z"), row("23", "uosmo", "granted-failed", "2026-01-06T12:00:00Z")}, got)
	})
	t.Run("utc_partial_day_half_open_window", func(t *testing.T) {
		got, err := read(t, "cosmoshub", "proof-owner", parse("2026-01-02T13:00:00+01:00"), parse("2026-01-02T19:00:00-05:00"), true, false)
		require.NoError(t, err)
		assertRows(t, expected[1:], got)
		got, err = read(t, apiMainnet, "proof-owner", parse("2026-01-02T11:00:00Z"), parse(noon), true, false)
		require.NoError(t, err)
		require.Empty(t, got, "end is exclusive even within one UTC day")
		got, err = read(t, apiMainnet, "proof-owner", parse(noon), parse(noon), true, false)
		require.NoError(t, err)
		require.Empty(t, got)
		got, err = read(t, apiMainnet, "proof-absent", parse(start), parse("2026-01-03T00:00:00Z"), true, false)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	for _, c := range []struct{ name, start, end string }{
		{"unresolved_no_partial_results", "2026-01-04T00:00:00Z", "2026-01-05T00:00:00Z"},
		{"malformed_charged_no_partial_results", "2026-01-05T00:00:00Z", "2026-01-06T00:00:00Z"},
		{"null_charge_no_partial_results", "2026-01-07T00:00:00Z", "2026-01-08T00:00:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, direct := range []bool{false, true} {
				got, err := read(t, apiMainnet, "proof-owner", parse(c.start), parse(c.end), true, direct)
				require.Error(t, err)
				require.Empty(t, got, "a known successful fee must not escape a failed fee read")
			}
		})
	}
	t.Run("strict_charged_coins_and_unbounded_integer_sum", func(t *testing.T) {
		cases := []struct {
			name, charged string
		}{
			{name: "decimal", charged: "1.5uatom"},
			{name: "bare_integer", charged: "123"},
			{name: "negative", charged: "-1uatom"},
			{name: "trailing_separator", charged: "1uatom,"},
			{name: "malformed_after_valid_coin", charged: "1uatom,broken"},
			{name: "whitespace", charged: " 1uatom"},
			{name: "invalid_denom", charged: "1u"},
			{name: "duplicate_denom_big_integer", charged: "9223372036854775808uatom,9uosmo,9223372036854775808uatom"},
		}
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				ts := parse("2026-01-10T12:00:00Z").AddDate(0, 0, i)
				blockID, goodID, failedID := 200+i, 400+2*i, 401+2*i
				require.NoError(t, database.Exec("INSERT INTO blocks VALUES(?,?,1,?)", blockID, 100+i, ts).Error)
				require.NoError(t, database.Exec("INSERT INTO txes VALUES(?,?,?,0),(?,?,?,5)", goodID, blockID, "strict-good", failedID, blockID, "strict-failed").Error)
				require.NoError(t, database.Exec("INSERT INTO fees VALUES(?,1,1,?,1),(?,1,1,?,700),(?,1,2,?,800)", 2000+goodID, goodID, 2000+failedID, failedID, 3000+failedID, failedID).Error)
				require.NoError(t, database.Exec("INSERT INTO tax_failed_tx_fees VALUES(?,?)", failedID, c.charged).Error)
				for _, direct := range []bool{false, true} {
					got, err := read(t, apiMainnet, "proof-owner", ts, ts.Add(time.Hour), true, direct)
					if c.name != "duplicate_denom_big_integer" {
						require.Error(t, err)
						require.Empty(t, got, "invalid charged evidence must discard even earlier valid rows")
						continue
					}
					require.NoError(t, err)
					require.Equal(t, []feeRec{
						{Amount: "1", Denom: "uatom", Hash: "strict-good", Ts: ts},
						{Amount: "18446744073709551616", Denom: "uatom", Hash: "strict-failed", Ts: ts},
						{Amount: "9", Denom: "uosmo", Hash: "strict-failed", Ts: ts},
					}, got, "combine charged denominations once, without int64 overflow or declared-fee multiplication")
				}
			})
		}
	})
	t.Run("legacy_public_declared_payer_fees_unchanged", func(t *testing.T) {
		// Partial-day bounds avoid exercising the independent legacy day-boundary
		// optimization. Compare against the exact pre-change public query.
		lo, hi := parse("2026-01-02T00:00:01Z"), parse("2026-01-06T23:59:59Z")
		for _, address := range []string{"proof-owner", "proof-grantee", "proof-unregistered-granter"} {
			var want []feeRec
			require.NoError(t, database.Table("fees").
				Select("fees.amount::text AS amount, denoms.base AS denom, blocks.time_stamp AS ts, txes.hash AS hash").
				Joins("JOIN addresses ON addresses.id=fees.payer_address_id").
				Joins("JOIN denoms ON denoms.id=fees.denomination_id").
				Joins("JOIN txes ON txes.id=fees.tx_id").
				Joins("JOIN blocks ON blocks.id=txes.block_id").
				Joins("JOIN chains ON chains.id=blocks.chain_id").
				Where("chains.chain_id IN ?", chainIDs(apiMainnet)).
				Where("addresses.address=? AND blocks.time_stamp>=? AND blocks.time_stamp<?", address, lo, hi).
				Order("blocks.time_stamp asc,fees.id asc").Scan(&want).Error)
			for i := range want {
				want[i].Ts = want[i].Ts.UTC()
			}
			got, err := read(t, apiMainnet, address, lo, hi, false, false)
			require.NoError(t, err, "legacy reads must not fail closed on unresolved or malformed outcomes")
			require.Len(t, got, len(want))
			if len(want) > 0 {
				require.Equal(t, want, got)
			}
		}
	})
}
