package main

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"gorm.io/gorm"
)

func omitPassword(t *testing.T) {
	t.Helper()
	t.Setenv("DB_PASS", "")
	_ = os.Unsetenv("DB_PASS")
	t.Setenv("PGPASSWORD", "${PRESERVATION_REDACTED}")
	t.Setenv("PGPASSFILE", "/dev/null")
}
func startupEnvironment(t *testing.T, host, port, database, user string) {
	t.Helper()
	t.Setenv("DB_HOST", host)
	t.Setenv("DB_PORT", port)
	t.Setenv("DB_NAME", database)
	t.Setenv("DB_USER", user)
	omitPassword(t)
}
func TestWrapperPasswordOmissionRequiresExplicitOperatorMode(t *testing.T) {
	if strings.Join(requiredDBEnvironment(false), ",") != "DB_HOST,DB_NAME,DB_USER,DB_PASS" {
		t.Fatal("default startup no longer requires password input")
	}
	if strings.Join(requiredDBEnvironment(true), ",") != "DB_HOST,DB_NAME,DB_USER" {
		t.Fatal("unexpected legacy operator contract")
	}
	omitPassword(t)
	if _, e := databaseConfig("127.0.0.1", "55473", "query_proof_scratch_fixture", "fixture-user", "", false); e == nil {
		t.Fatal("default config accepted missing password")
	}
	cfg, e := databaseConfig("127.0.0.1", "55473", "query_proof_scratch_fixture", "fixture-user", "", true)
	if e != nil || cfg.Password != "" {
		t.Fatal("legacy config invented or inherited password material")
	}
}
func TestWrapperNormalStartupDefaultRejectsMissingPasswordBeforeConnect(t *testing.T) {
	startupEnvironment(t, "127.0.0.1", "1", "query_proof_scratch_fixture", "fixture-user")
	called := false
	_, e := initializeDatabaseFromEnvironment(config{}, func(string, string, string, string, string, bool) (*gorm.DB, error) {
		called = true
		return nil, errors.New("unexpected connect")
	})
	if e == nil || called {
		t.Fatal("normal default startup attempted connection without required password")
	}
}
func TestWrapperNormalStartupExplicitLegacyInitializesActualDriver(t *testing.T) {
	dsn := os.Getenv("PROOF_SCRATCH_DSN")
	if dsn == "" {
		t.Skip("explicit disposable loopback scratch database required")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || cfg.Host != "127.0.0.1" || !strings.HasPrefix(cfg.Database, "query_proof_scratch_") {
		t.Fatal("unsafe scratch target refused")
	}
	startupEnvironment(t, cfg.Host, strconv.Itoa(int(cfg.Port)), cfg.Database, cfg.User)
	db, e := initializeDatabaseFromEnvironment(config{LegacyTrustPasswordOptional: true}, connectDatabase)
	if e != nil {
		t.Fatal("normal explicit-legacy startup could not initialize local driver")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("scratch pool unavailable")
	}
	defer pool.Close()
	if pool.Stats().MaxOpenConnections != 1 {
		t.Fatal("wrapper pool limit changed")
	}
	var readOnly string
	if e = db.Raw("SELECT current_setting('transaction_read_only')").Scan(&readOnly).Error; e != nil || readOnly != "on" {
		t.Fatal("actual normal startup did not retain read-only session")
	}
}
func TestWrapperNormalStartupLegacyFailsClosedWhenServerDemandsPassword(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("fixture listener failed")
	}
	defer listener.Close()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan bool, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			done <- false
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		backend := pgproto3.NewBackend(conn, conn)
		if _, e = backend.ReceiveStartupMessage(); e != nil {
			done <- false
			return
		}
		backend.Send(&pgproto3.AuthenticationCleartextPassword{})
		if e = backend.Flush(); e != nil {
			done <- false
			return
		}
		msg, e := backend.Receive()
		if e != nil {
			done <- false
			return
		}
		password, ok := msg.(*pgproto3.PasswordMessage)
		backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: "fixture authentication rejected"})
		_ = backend.Flush()
		done <- ok && password.Password =${PRESERVATION_REDACTED} ""
	}()
	addr := listener.Addr().(*net.TCPAddr)
	startupEnvironment(t, "127.0.0.1", strconv.Itoa(addr.Port), "query_proof_scratch_fake", "fixture-user")
	db, e := initializeDatabaseFromEnvironment(config{LegacyTrustPasswordOptional: true}, connectDatabase)
	if e == nil {
		if pool, x := db.DB(); x == nil {
			_ = pool.Close()
		}
		t.Fatal("server authentication challenge was bypassed")
	}
	select {
	case empty := <-done:
		if !empty {
			t.Fatal("client supplied ambient password or did not process authentication challenge")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("fixture authentication exchange did not complete")
	}
}
