package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Optional password input is not an authentication-policy change. The server
// remains authoritative. An absent DB_PASS omits URL password material and
// cannot silently pick up a different password from ambient env or passfiles.
func databaseConfig(host, port, database, user, password string, legacy bool) (*pgx.ConnConfig, error) {
	if password == "" && !legacy {
		return nil, fmt.Errorf("password required unless explicit legacy operator mode")
	}
	u := url.URL{Scheme: "postgresql", Host: net.JoinHostPort(host, port), Path: "/" + database, User: url.User(user)}
	if password != "" {
		u.User = url.UserPassword(user, password)
	}
	u.RawQuery = url.Values{"sslmode": {"disable"}, "passfile": {"/dev/null"}, "default_transaction_read_only": {"on"}, "statement_timeout": {"60000"}, "max_parallel_workers_per_gather": {"0"}}.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	cfg.Password = password
	cfg.ConnectTimeout = 5 * time.Second
	return cfg, nil
}

type databaseOpener func(string, string, string, string, string, bool) (*gorm.DB, error)

// This is the normal startup path, also exercised directly by startup tests.
func initializeDatabaseFromEnvironment(c config, open databaseOpener) (*gorm.DB, error) {
	for _, key := range requiredDBEnvironment(c.LegacyTrustPasswordOptional) {
		if os.Getenv(key) == "" {
			return nil, fmt.Errorf("required database environment missing")
		}
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	return open(os.Getenv("DB_HOST"), port, os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASS"), c.LegacyTrustPasswordOptional)
}
func connectDatabase(host, port, database, user, password string, legacy bool) (*gorm.DB, error) {
	cfg, err := databaseConfig(host, port, database, user, password, legacy)
	if err != nil {
		return nil, err
	}
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	return db, nil
}
