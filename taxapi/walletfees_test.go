package taxapi

import (
	"errors"
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

func TestFeeBoundaryUnavailable(t *testing.T) {
	for _, err := range []error{errBalanceBeforeIndex, errBalanceNotReached, balanceIncomplete{"missing boundary"}} {
		require.True(t, feeBoundaryUnavailable(fmt.Errorf("wrapped: %w", err)))
	}
	require.False(t, feeBoundaryUnavailable(errors.New("database failure")))
}

// The uniquely named schema and every fixture row remain available for audit.
func TestWalletFeesExactWindowPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	schema := fmt.Sprintf("wallet_fees_proof_%d", time.Now().UnixNano())
	require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
	bootstrap, err := db.DB()
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
	db, err = gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
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
		"INSERT INTO chains VALUES(1,'cosmoshub-4'),(2,'foreign')",
		"INSERT INTO addresses VALUES(1,'payer'),(2,'other')",
		"INSERT INTO denoms VALUES(1,'uatom'),(2,'voucher')",
		"INSERT INTO blocks VALUES(21,1,1,'2026-01-01 23:59Z'),(4,2,1,'2026-01-02 00:00Z'),(900,3,1,'2026-01-02 12:00Z'),(6,4,1,'2026-01-02 23:59Z'),(8,5,1,'2026-01-03 00:00Z'),(5,7,1,'2026-01-04 00:00Z'),(7,3,2,'2026-01-02 12:00Z')",
		"INSERT INTO txes VALUES(9,21,'before',0),(2,4,'start',0),(8,900,'failed',1),(3,6,'last',0),(7,8,'end',0),(4,7,'foreign',0),(6,900,'other',0)",
		"INSERT INTO fees VALUES(1,1,1,9,1),(2,1,1,2,2),(3,1,1,8,3),(4,1,1,3,4),(5,1,2,3,5),(6,1,1,7,6),(7,1,1,4,7),(8,2,1,6,8)",
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	t.Logf("preserved proof schema: %s", schema)
	parse := func(s string) time.Time { v, err := time.Parse(time.RFC3339, s); require.NoError(t, err); return v }
	cases := []struct{ name, payer, start, end string }{
		{"whole_day", "payer", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"},
		{"intraday", "payer", "2026-01-02T11:00:00Z", "2026-01-02T13:00:00Z"},
		{"before_index", "payer", "2025-12-30T00:00:00Z", "2026-01-03T00:00:00Z"},
		{"future_end", "payer", "2026-01-02T00:00:00Z", "2026-01-10T00:00:00Z"},
		{"boundary_hole", "payer", "2026-01-02T00:00:00Z", "2026-01-04T00:00:00Z"},
		{"unknown_payer", "absent", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"},
		{"empty_window", "payer", "2026-01-03T00:00:00Z", "2026-01-03T00:00:00Z"},
	}
	compare := func(t *testing.T, chain, payer string, start, end time.Time) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
		var old []feeRec
		require.NoError(t, tx.Table("fees").
			Select("fees.amount::text AS amount, denoms.base AS denom, blocks.time_stamp AS ts, txes.hash AS hash").
			Joins("JOIN addresses ON addresses.id=fees.payer_address_id").
			Joins("JOIN denoms ON denoms.id=fees.denomination_id").
			Joins("JOIN txes ON txes.id=fees.tx_id").
			Joins("JOIN blocks ON blocks.id=txes.block_id").
			Joins("JOIN chains ON chains.id=blocks.chain_id").
			Where("chains.chain_id IN ?", chainIDs(chain)).
			Where("addresses.address=? AND blocks.time_stamp>=? AND blocks.time_stamp<?", payer, start, end).
			Order("blocks.time_stamp asc,fees.id asc").Scan(&old).Error)
		got, err := (&Server{db: tx}).walletFees(chain, payer, start, end)
		require.NoError(t, err)
		require.Equal(t, len(old), len(got))
		if len(old) > 0 {
			require.Equal(t, old, got)
		}
		if chain == apiMainnet && start.Equal(parse("2026-01-02T00:00:00Z")) && end.Equal(parse("2026-01-03T00:00:00Z")) && payer == "payer" {
			require.GreaterOrEqual(t, len(got), 4)
			require.Equal(t, "failed", got[1].Hash)
		}
		var unassigned bool
		require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error)
		require.True(t, unassigned)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { compare(t, apiMainnet, c.payer, parse(c.start), parse(c.end)) })
	}
	t.Run("unindexed_chain", func(t *testing.T) {
		compare(t, apiTestnet, "payer", parse("2026-01-02T00:00:00Z"), parse("2026-01-03T00:00:00Z"))
	})
	require.NoError(t, db.Exec("INSERT INTO chains VALUES(3,'mainnet')").Error)
	require.NoError(t, db.Exec("INSERT INTO blocks VALUES(11,3,3,'2026-01-02 13:00Z')").Error)
	require.NoError(t, db.Exec("INSERT INTO txes VALUES(11,11,'alias',0)").Error)
	require.NoError(t, db.Exec("INSERT INTO fees VALUES(11,1,1,11,11)").Error)
	t.Run("multiple_chain_aliases", func(t *testing.T) {
		compare(t, apiMainnet, "payer", parse("2026-01-02T00:00:00Z"), parse("2026-01-03T00:00:00Z"))
	})
}
