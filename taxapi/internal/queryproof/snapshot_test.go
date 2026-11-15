package queryproof

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func scratch(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("PROOF_SCRATCH_DSN")
	if dsn == "" {
		t.Skip("explicit disposable loopback named scratch database required")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || (cfg.Host != "127.0.0.1" && cfg.Host != "::1" && cfg.Host != "localhost") || !strings.HasPrefix(cfg.Database, "query_proof_scratch_") {
		t.Fatal("unsafe scratch configuration refused before connection")
	}
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal("scratch open failed")
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return dsn, db
}
func TestExportedSnapshotImportReadOnlyIdentityRollbackPostgres(t *testing.T) {
	dsn, writer := scratch(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("proof_values_%d", time.Now().UnixNano())
	if _, e := writer.ExecContext(ctx, "CREATE TABLE "+name+"(n bigint)"); e != nil {
		t.Fatal("scratch setup failed")
	}
	if _, e := writer.ExecContext(ctx, "INSERT INTO "+name+" VALUES(7)"); e != nil {
		t.Fatal("scratch fixture insert failed")
	}
	exported, e := Export(ctx, dsn, "scratch", nil)
	if e != nil {
		t.Fatal("scratch exporter failed")
	}
	defer exported.Close()
	if _, e = writer.ExecContext(ctx, "INSERT INTO "+name+" VALUES(9)"); e != nil {
		t.Fatal("scratch writer update failed")
	}
	var snapshots []string
	for i := 0; i < 2; i++ {
		identity := fixture(t).Identity()
		capture, e := NewCapture(exported.Source, identity)
		if e != nil {
			t.Fatal("capture initialization failed")
		}
		db, e := OpenSQL(capture, dsn)
		if e != nil {
			t.Fatal("capture SQL open failed")
		}
		tx, e := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if e != nil {
			t.Fatal("imported RR transaction failed")
		}
		if _, e = tx.ExecContext(ctx, "SET LOCAL plan_cache_mode = force_custom_plan"); e != nil {
			t.Fatal("read planner setting failed")
		}
		var total int64
		var snapshot string
		if e = tx.QueryRowContext(ctx, "SELECT sum(n) FROM "+name).Scan(&total); e != nil || total != 7 {
			t.Fatal("exported snapshot was not preserved")
		}
		if e = tx.QueryRowContext(ctx, "SELECT pg_current_snapshot()::text").Scan(&snapshot); e != nil {
			t.Fatal("imported snapshot read failed")
		}
		snapshots = append(snapshots, snapshot)
		if e = tx.Commit(); e != nil {
			t.Fatal("virtual reader commit failed")
		}
		_ = db.Close()
		replay := frozen(t, capture)
		offline, e := OpenSQL(replay, "MUST NOT PARSE THIS DSN")
		if e != nil {
			t.Fatal(e)
		}
		rt, e := offline.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if e != nil {
			t.Fatal("offline transaction failed")
		}
		_, e = rt.ExecContext(ctx, "SET LOCAL plan_cache_mode = force_custom_plan")
		if e != nil {
			t.Fatal("offline planner read failed")
		}
		if e = rt.QueryRowContext(ctx, "SELECT sum(n) FROM "+name).Scan(&total); e != nil || total != 7 {
			t.Fatal("offline SQL result changed")
		}
		if e = rt.QueryRowContext(ctx, "SELECT pg_current_snapshot()::text").Scan(&snapshot); e != nil {
			t.Fatal("offline snapshot read failed")
		}
		_ = rt.Commit()
		_ = offline.Close()
		if e = replay.Finish(false); e != nil {
			t.Fatal("offline SQL inputs unaccounted")
		}
	}
	if snapshots[0] != snapshots[1] || snapshots[0] != exported.Source.PGSnapshot {
		t.Fatal("reader snapshots differ")
	}
	t.Run("wrong-source-identity", func(t *testing.T) {
		source := exported.Source
		source.DatabaseIdentitySHA256 = Digest([]byte("different database"))
		capture, e := NewCapture(source, fixture(t).Identity())
		if e != nil {
			t.Fatal(e)
		}
		db, _ := OpenSQL(capture, dsn)
		defer db.Close()
		var n int
		if e = db.QueryRowContext(ctx, "SELECT 1").Scan(&n); e == nil {
			t.Fatal("different source identity accepted")
		}
	})
	if e = exported.Close(); e != nil {
		t.Fatal("exporter rollback failed")
	}
	t.Run("expired-export", func(t *testing.T) {
		capture, e := NewCapture(exported.Source, fixture(t).Identity())
		if e != nil {
			t.Fatal(e)
		}
		db, _ := OpenSQL(capture, dsn)
		defer db.Close()
		var n int
		if e = db.QueryRowContext(ctx, "SELECT 1").Scan(&n); e == nil {
			t.Fatal("expired snapshot accepted")
		}
	})
	var actual int
	if e = writer.QueryRowContext(ctx, "SELECT sum(n) FROM "+name).Scan(&actual); e != nil || actual != 16 {
		t.Fatal("scratch writer content changed")
	}
	t.Run("server-read-only-enforced", func(t *testing.T) {
		e, err := Export(ctx, dsn, "scratch", nil)
		if err != nil {
			t.Fatal("fresh exporter failed")
		}
		defer e.Close()
		if _, err = e.tx.ExecContext(ctx, "INSERT INTO "+name+" VALUES(100)"); err == nil {
			t.Fatal("read-only exporter allowed write")
		}
	})
}
