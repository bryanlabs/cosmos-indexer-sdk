package taxapi

import (
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

func TestWalletFewTxScopeMessageFencePostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(postgres.Open(dsn), config)
	require.NoError(t, err)
	schema := fmt.Sprintf("wallet_scope_proof_%d", time.Now().UnixNano())
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
		"CREATE TABLE txes(id bigint PRIMARY KEY)",
		"CREATE TABLE addresses(id bigint PRIMARY KEY,address text UNIQUE)",
		"CREATE TABLE tx_signer_addresses(tx_id bigint,address_id bigint)",
		"CREATE TABLE fees(tx_id bigint,payer_address_id bigint)",
		"CREATE TABLE messages(id bigint PRIMARY KEY,tx_id bigint)",
		"CREATE INDEX message_tx ON messages(tx_id)",
		"CREATE TABLE taxable_events(message_id bigint,from_addr text,to_addr text,validator_address text)",
		"CREATE INDEX event_message ON taxable_events(message_id)",
		"CREATE TABLE message_events(id bigint PRIMARY KEY,message_id bigint)",
		"CREATE TABLE message_event_attributes(message_event_id bigint,value text)",
		"INSERT INTO txes SELECT generate_series(1,12)",
		"INSERT INTO addresses VALUES(1,'target'),(2,'other')",
		"INSERT INTO tx_signer_addresses VALUES(1,1)",
		"INSERT INTO fees VALUES(2,1),(9,1)",
		"INSERT INTO messages SELECT n,n FROM generate_series(1,12)n",
		"INSERT INTO taxable_events VALUES(3,'target',NULL,NULL),(4,NULL,'target',NULL),(5,NULL,NULL,'target'),(9,'target','target','target'),(10,'other','other','other'),(11,NULL,NULL,NULL),(12,'target','target','target')",
		"INSERT INTO message_events VALUES(6,6),(7,7),(8,8),(9,9)",
		"INSERT INTO message_event_attributes VALUES(6,'target'),(7,'target-extra'),(8,'prefix-target-suffix'),(9,'target')",
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	t.Logf("preserved proof schema: %s", schema)
	const legacy = `(
 EXISTS (SELECT 1 FROM tx_signer_addresses tsa JOIN addresses a ON a.id=tsa.address_id WHERE tsa.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM fees f JOIN addresses a ON a.id=f.payer_address_id WHERE f.tx_id=t.id AND a.address IN ?)
 OR EXISTS (SELECT 1 FROM messages wm JOIN taxable_events te ON te.message_id=wm.id WHERE wm.tx_id=t.id AND (te.from_addr IN ? OR te.to_addr IN ? OR te.validator_address IN ?))
 OR EXISTS (SELECT 1 FROM messages wm JOIN message_events me ON me.message_id=wm.id JOIN message_event_attributes mea ON mea.message_event_id=me.id WHERE wm.tx_id=t.id AND mea.value IN ? OFFSET 0)
 )`
	all := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	for _, c := range []struct {
		name                 string
		wallets              []string
		candidates, expected []int64
	}{
		{"all_paths", []string{"target"}, all, []int64{1, 2, 3, 4, 5, 6, 9, 12}},
		{"other", []string{"other"}, all, []int64{10}},
		{"multi_wallet", []string{"target", "other"}, all, []int64{1, 2, 3, 4, 5, 6, 9, 10, 12}},
		{"duplicate_wallet", []string{"target", "target"}, all, []int64{1, 2, 3, 4, 5, 6, 9, 12}},
		{"sparse_candidates", []string{"target"}, []int64{3, 6, 7}, []int64{3, 6}},
		{"no_substring_match", []string{"target"}, []int64{7, 8, 10, 11}, nil},
		{"missing_wallet", []string{"missing"}, all, nil},
		{"empty_wallets", nil, all, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
			query := func() *gorm.DB { return tx.Table("(SELECT id FROM txes WHERE id IN ? OFFSET 0) t", c.candidates) }
			var old, got []int64
			require.NoError(t, query().Where(legacy, c.wallets, c.wallets, c.wallets, c.wallets, c.wallets, c.wallets).Order("t.id").Pluck("t.id", &old).Error)
			require.NoError(t, walletFewTxScope(query(), c.wallets).Order("t.id").Pluck("t.id", &got).Error)
			require.Equal(t, len(c.expected), len(got))
			if len(got) > 0 {
				require.Equal(t, c.expected, got)
				require.Equal(t, old, got)
			} else {
				require.Empty(t, old)
			}
			var unassigned bool
			require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error)
			require.True(t, unassigned)
		})
	}
}
