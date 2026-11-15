package main

import (
	"context"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestActualLabExclusiveLockAndProtectedReadRefusal(t *testing.T) {
	if os.Getenv("HUB2_PRIVATE_LAB_TEST") != "yes" {
		t.Skip("private same-Pod fixture only")
	}
	ctx := context.Background()
	d, e := openRehearsalDestination(ctx, dsn(importerRole), database)
	if e != nil {
		t.Fatal("first lab destination refused")
	}
	defer d.close()
	other, e := openRehearsalDestination(ctx, dsn(importerRole), database)
	if other != nil {
		other.close()
	}
	if e == nil {
		t.Fatal("second writer admitted")
	}
	var n int64
	if d.conn.QueryRowContext(ctx, "SELECT count(*) FROM public.protected_fixture").Scan(&n) == nil {
		t.Fatal("importer read protected public records")
	}
	var pid1, pid2 int
	if d.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid1) != nil || d.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid2) != nil || pid1 != pid2 {
		t.Fatal("pinned session changed")
	}
}
func TestActualLabFailedBatchHasNoPartialCommit(t *testing.T) {
	if os.Getenv("HUB2_PRIVATE_LAB_TEST") != "yes" {
		t.Skip("private same-Pod fixture only")
	}
	db := pgtest.OpenRetained(t)
	pool, e := db.Importer.DB()
	if e != nil {
		t.Fatal("fixture connection")
	}
	b := ledger.Block{Chain: "cosmoshub-2", Height: 1, Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Hash: strings.Repeat("AB", 32), Proposer: "synthetic"}
	bad := b
	bad.Height = 2
	bad.Events = []ledger.Event{{Phase: 1, Index: 0, Type: "synthetic_negative", Attributes: [][2]string{{"test", "\x00"}}}}
	if _, e = ledger.Write(context.Background(), pool, []ledger.Block{b, bad}); e == nil {
		t.Fatal("invalid late SQL value admitted")
	}
	var n int64
	if db.Owner.Raw("SELECT count(*) FROM legacy.blocks").Scan(&n).Error != nil || n != 0 {
		t.Fatal("failed transaction left partial headers")
	}
	if db.Owner.Raw("SELECT count(*) FROM legacy.block_events").Scan(&n).Error != nil || n != 0 {
		t.Fatal("failed transaction left partial events")
	}
}
