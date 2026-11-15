package main

import (
	"net"
	"net/url"
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
func databaseConfig(host, port, database, user, password string) (*pgx.ConnConfig, error) {
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
func connectDatabase(host, port, database, user, password string) (*gorm.DB, error) {
	cfg, err := databaseConfig(host, port, database, user, password)
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
