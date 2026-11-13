// Package pgtest gives the legacy schema tests a throwaway database that
// holds the serving schema with rows in it, the legacy schema, and a
// connection as the importer role.
package pgtest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	indexerdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Database is one throwaway database. Owner created every table. Importer
// is logged in as the importer role.
type Database struct {
	Owner, Importer *gorm.DB
	Role            string
}

// Open needs LEGACY_STORE_TEST_ADMIN_DSN to name a loopback server. It
// creates its own database and role and drops both afterwards. Port 15432
// is refused because it is the customary forward to the serving database.
func Open(t *testing.T) Database {
	return open(t, false)
}

// OpenRetained uses the same private confined-role fixture but never drops a
// database or role. Callers under a no-deletes instruction use this variant.
func OpenRetained(t *testing.T) Database {
	return open(t, true)
}

func open(t *testing.T, retained bool) Database {
	t.Helper()
	admin := os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN")
	if admin == "" {
		t.Skip("private Postgres not supplied")
	}
	config, parseErr := pgx.ParseConfig(admin)
	require.NoError(t, parseErr)
	safe := func(host string, port uint16) bool {
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback() && port != 15432 && port != 0
	}
	if !safe(config.Host, config.Port) || config.Database == "" {
		t.Fatal("refusing a DSN that is not an explicit loopback throwaway server")
	}
	for _, fallback := range config.Fallbacks {
		if !safe(fallback.Host, fallback.Port) {
			t.Fatal("refusing a non-private fixture fallback")
		}
	}
	quiet := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	root, err := gorm.Open(postgres.Open(admin), quiet)
	require.NoError(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "legacy_schema_test_" + hex.EncodeToString(suffix)
	if retained {
		name = "legacy_replay_rehearsal_" + hex.EncodeToString(suffix)
	}
	role := "legacy_importer_" + hex.EncodeToString(suffix)
	require.NoError(t, root.Exec("CREATE DATABASE "+name).Error)
	connect := func(user, password string) (*gorm.DB, error) {
		c := config.Copy()
		c.Database = name
		c.User = user
		c.Password = password
		// The typed config drives connections; the canonical URL is retained
		// for command-path tests that intentionally reconnect via the CLI.
		u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), User: url.UserPassword(c.User, c.Password), Path: "/" + c.Database, RawQuery: "sslmode=disable"}
		return gorm.Open(postgres.New(postgres.Config{Conn: stdlib.OpenDB(*c), DSN: u.String()}), quiet)
	}
	owner, err := connect(config.User, config.Password)
	require.NoError(t, err)
	var destination string
	require.NoError(t, owner.Raw("SELECT current_database()").Scan(&destination).Error)
	require.Equal(t, name, destination, "fixture connection must match its newly created database before migration")
	var importer *gorm.DB
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{importer, owner} {
			if db != nil {
				if sql, err := db.DB(); err == nil {
					_ = sql.Close()
				}
			}
		}
		if !retained {
			_ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
			_ = root.Exec("DROP ROLE IF EXISTS " + role).Error
		}
		if sql, err := root.DB(); err == nil {
			_ = sql.Close()
		}
	})
	// The serving schema exactly as the indexer creates it, with a
	// cosmoshub-4 transaction in it, before anything legacy exists.
	require.NoError(t, indexerdb.MigrateModels(owner))
	chain := models.Chain{ChainID: "cosmoshub-4", Name: "cosmoshub-4"}
	require.NoError(t, owner.Create(&chain).Error)
	proposer := models.Address{Address: "synthetic-proposer"}
	require.NoError(t, owner.Create(&proposer).Error)
	block := models.Block{Height: 5200791, ChainID: chain.ID, TimeStamp: time.Date(2021, 2, 18, 6, 0, 0, 0, time.UTC), ProposerConsAddressID: proposer.ID, TxIndexed: true, BlockEventsIndexed: true}
	require.NoError(t, owner.Omit(clause.Associations).Create(&block).Error)
	require.NoError(t, owner.Omit(clause.Associations).Create(&models.Tx{Hash: strings.Repeat("AB", 32), BlockID: block.ID, Memo: "synthetic memo"}).Error)

	require.NoError(t, schema.Migrate(owner, role))
	require.NoError(t, owner.Exec("ALTER ROLE "+role+" PASSWORD 'importer-test'").Error)
	importer, err = connect(role, "importer-test")
	require.NoError(t, err)
	return Database{Owner: owner, Importer: importer, Role: role}
}

// Snapshot records every row of every table in a schema with its physical
// location and the transaction that wrote it. An unchanged location and
// writer mean the stored row was never rewritten.
func Snapshot(t *testing.T, db *gorm.DB, schemaName string) map[string]string {
	t.Helper()
	var tables []string
	require.NoError(t, db.Raw(`SELECT tablename FROM pg_tables WHERE schemaname = ? ORDER BY 1`, schemaName).Scan(&tables).Error)
	rows := map[string]string{}
	for _, table := range tables {
		var lines []string
		require.NoError(t, db.Raw(fmt.Sprintf(`SELECT ctid::text || ' ' || xmin::text || ' ' || r::text FROM %q.%q r`, schemaName, table)).Scan(&lines).Error)
		for _, line := range lines {
			location := strings.SplitN(line, " ", 2)
			rows[table+" "+location[0]] = location[1]
		}
	}
	return rows
}

// RequireUntouched fails when a row of the earlier snapshot is gone or
// differs, or when a row was added.
func RequireUntouched(t *testing.T, before, after map[string]string) {
	t.Helper()
	require.NotEmpty(t, before)
	var keys []string
	for key := range before {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		got, ok := after[key]
		require.True(t, ok, "a row that existed before is gone: %s", strings.Fields(key)[0])
		require.Equal(t, before[key], got, "a row that existed before changed: %s", strings.Fields(key)[0])
	}
	require.Len(t, after, len(before), "rows were added")
}
