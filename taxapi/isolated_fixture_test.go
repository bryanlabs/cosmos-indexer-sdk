package taxapi

import (
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/ory/dockertest/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Explicit loopback opt-in runs the same real-Postgres fixtures without Docker.
// Bind the throwaway schema in every connection, including MVCC test readers.
func isolatedAPIFixtureDB(t *testing.T) *gorm.DB {
	t.Helper()
	if dsn := os.Getenv("COSMOS_TAX_TEST_ISOLATED_DSN"); dsn != "" {
		u, err := url.Parse(dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" && u.Hostname() != "localhost") {
			t.Fatal("isolated fixture requires an explicit loopback PostgreSQL URL")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		conn, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("commission_isolated_fixture_%d", time.Now().UnixNano())
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		db, err = gorm.Open(postgres.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		conn, err = db.DB()
		if err != nil {
			t.Fatal(err)
		}
		conn.SetMaxOpenConns(4)
		conn.SetMaxIdleConns(4)
		t.Cleanup(func() {
			if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
				t.Error(err)
			}
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		})
		return db
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := pool.Run("postgres", "15-alpine", []string{"POSTGRES_USER=test", "POSTGRES_PASSWORD=test", "POSTGRES_DB=test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Purge(resource); err != nil {
			t.Error(err)
		}
	})
	var db *gorm.DB
	if err := pool.Retry(func() error {
		var err error
		db, err = dbpkg.PostgresDbConnect(resource.GetBoundIP(apiPostgresPort), resource.GetPort(apiPostgresPort), apiTest, apiTest, apiTest, "silent")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db
}
