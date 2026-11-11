// Package nativepg is isolated validation support, never imported by runtime code.
package nativepg

import (
	"fmt"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"sync/atomic"
)

var sequence atomic.Uint64

// Open replaces only disposable Docker fixture provisioning. Each caller gets
// a separate real database and normal multi-connection MVCC/constraint semantics.
func Open() (func(), *gorm.DB, error) {
	dsn := os.Getenv("COSMOS_DB_SAVE_TEST_DSN")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, err
	}
	if dsn == "" || cfg.Host != "127.0.0.1" || cfg.Port != 16439 || cfg.User != "dbsave" || cfg.Database != "postgres" {
		return nil, nil, fmt.Errorf("native fixture requires isolated 127.0.0.1:16439 dbsave/postgres")
	}
	admin, err := gorm.Open(postgres.Open("host=127.0.0.1 port=16439 user=dbsave dbname=postgres sslmode=disable"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, nil, err
	}
	adminSQL, err := admin.DB()
	if err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("dbsave_native_%d_%d", os.Getpid(), sequence.Add(1))
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		adminSQL.Close()
		return nil, nil, err
	}
	childDSN := "host=127.0.0.1 port=16439 user=dbsave dbname=" + name + " sslmode=disable"
	if os.Getenv("COSMOS_RETAINED_SQL_LOG") == "1" {
		childDSN += " options='-clog_statement=all'"
	}
	database, err := gorm.Open(postgres.Open(childDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		admin.Exec("DROP DATABASE " + name)
		adminSQL.Close()
		return nil, nil, err
	}
	clean := func() {
		conn, e := database.DB()
		if e != nil {
			panic(e)
		}
		if e := conn.Close(); e != nil {
			panic(e)
		}
		if e := admin.Exec("DROP DATABASE " + name).Error; e != nil {
			panic(e)
		}
		if e := adminSQL.Close(); e != nil {
			panic(e)
		}
	}
	return clean, database, nil
}
