//go:build ops818new06stop && ops818stopshared

package taxapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOPS818New06DedicatedStopSupplier(t *testing.T) {
	dir := os.Getenv("OPS818_NEW06_STOP_ROOT")
	root := os.Getenv("OPS818_NEW06_STOP_REQUEST_ROOT")
	if dir == "" {
		t.Skip("private actual06 only")
	}
	syscall.Umask(0077)
	const exactRoot = "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/validator-06"
	if root != exactRoot {
		t.Fatal("dedicated06 ownership only,07 and GATA excluded")
	}
	if e := scpVerifyProduction(); e != nil {
		t.Fatal("source production mismatch")
	}
	rec := &scpRecorder{Phase: "connect"}
	cfg, e := pgx.ParseConfig("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-dedicated06-stop default_transaction_read_only=on")
	if e != nil {
		t.Fatal("source config failed")
	}
	cfg.Tracer = &scpTracer{rec}
	pool := sql.OpenDB(&scpConnector{stdlib.GetConnector(*cfg), rec})
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("source connection failed")
	}
	snapshot := 1
	s, e := scsNewSession(db, rec, dir, snapshot)
	if e != nil {
		t.Fatal("actual label setup failed")
	}
	defer func() { s.tx.Rollback() }()
	seen := map[string]bool{}
	n := 0
	for {
		var disk syscall.Statfs_t
		if syscall.Statfs(dir, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
			t.Fatal("owned4GiBreserve stop")
		}
		if time.Since(s.c.TransactionAt) > 10*time.Minute || s.requests >= 1000 {
			s.tx.Rollback()
			snapshot++
			s, e = scsNewSession(db, rec, dir, snapshot)
			if e != nil {
				t.Fatal("actual snapshot refresh failed")
			}
		}
		b, e := os.ReadFile(filepath.Join(root, "extra-bank-request.private.jsonl"))
		if e != nil {
			t.Fatal("actual owned request missing")
		}
		lines := bytes.Split(b, []byte("\n"))
		for _, line := range lines[:len(lines)-1] {
			var r struct {
				Kind, Source string
				Request      struct{ Address string }
			}
			if json.Unmarshal(line, &r) != nil {
				t.Fatal("actual request malformed")
			}
			if r.Kind != "source-bound-stop" {
				continue
			}
			if r.Source != scpSource || r.Request.Address == "" {
				t.Fatal("actual request source rejected")
			}
			if seen[r.Request.Address] {
				continue
			}
			lock, e := os.OpenFile(filepath.Join(root, "opening06-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if e != nil {
				t.Fatal("shared cumulative lock open failed")
			}
			e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
			if e != nil {
				lock.Close()
				t.Fatal("shared cumulative lock failed")
			}
			n++
			e = scsSupply(s, root, r.Request.Address, json.RawMessage(line), dir, n)
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			if e != nil {
				_ = scpWrite(filepath.Join(dir, "failure.private.json"), map[string]any{"phase": rec.Phase, "error": e.Error(), "count": n, "queries": rec.Queries, "wire": rec.Wire, "financially_accepted": false})
				t.Fatal("actual06 stop failed explicitly")
			}
			seen[r.Request.Address] = true
		}
		time.Sleep(time.Second)
	}
}
