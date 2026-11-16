package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestActualRestartPayoutWiring runs the real add path on real sealed
// components with the canonical height lowered to the end of the second
// component, in its own throwaway private database. It proves the wiring
// only: that the export at the canonical height becomes payout rows on that
// height exactly once, and that a second run adds nothing. The database it
// fills is a test fixture and is never staging for a load.
func TestActualRestartPayoutWiring(t *testing.T) {
	if os.Getenv("HUB2_STAGING_PAYOUT_TEST") != "yes" {
		t.Skip("private fixture only")
	}
	p, e := strconv.Atoi(os.Getenv("HUB2_STAGING_TEST_PORT"))
	name := os.Getenv("HUB2_STAGING_TEST_DATABASE")
	if e != nil || guardTarget(name, p) != nil {
		t.Fatal("private test target refused")
	}
	database, port = name, p
	var m Manifest
	if readJSON(os.Getenv("HUB2_STAGING_TEST_MANIFEST"), &m) != nil || len(m.Components) != 2 {
		t.Fatal("two-component sealed manifest required")
	}
	canonicalHeight = m.Last
	defer func() { canonicalHeight = 2902000 }()
	executable, _ := os.Executable()
	m.Version, m.BinarySHA = 2, fileSHA(executable)
	if validateLayout(m) == nil {
		t.Fatal("canonical end admitted without its payout")
	}
	x := &CanonicalExport{Records: os.Getenv("HUB2_STAGING_TEST_EXPORT_RECORDS"), Rows: 1}
	if readJSON(os.Getenv("HUB2_STAGING_TEST_EXPORT_PROOF"), &x.Proof) != nil {
		t.Fatal("export proof")
	}
	m.CanonicalExport = x
	if _, e := loadExport(x); e == nil {
		t.Fatal("wrong sealed payout count admitted")
	}
	x.Rows, e = strconv.ParseInt(os.Getenv("HUB2_STAGING_TEST_EXPORT_ROWS"), 10, 64)
	if e != nil {
		t.Fatal("expected payout rows")
	}
	if _, e := loadExport(x); e != nil {
		t.Fatal("export records refused")
	}
	dir := t.TempDir()
	if e := runAdd(m, filepath.Join(dir, "first"), true, 1); e != nil {
		t.Fatal("first run refused")
	}
	count := func() (payout, elsewhere, historical int64) {
		db, e := owner()
		if e != nil {
			t.Fatal("owner")
		}
		pool, _ := db.DB()
		defer pool.Close()
		if pool.QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE phase=4 AND height=$1 AND amount_source='export_payout'), count(*) FILTER (WHERE (phase=4 OR amount_source='export_payout' OR reward_trigger='zero_height_export') AND height<>$1), count(*) FILTER (WHERE phase<>4) FROM legacy.taxable_events`, canonicalHeight).Scan(&payout, &elsewhere, &historical) != nil {
			t.Fatal("count")
		}
		return
	}
	payout, elsewhere, historical := count()
	if payout != x.Rows || elsewhere != 0 || historical != m.Components[0].HistoricalRows+m.Components[1].HistoricalRows {
		t.Fatal("payout not exactly once on the canonical height", payout, elsewhere, historical)
	}
	if e := runAdd(m, filepath.Join(dir, "second"), true, 1); e != nil {
		t.Fatal("second run refused")
	}
	var result struct {
		Read []componentResult `json:"components_read_this_run"`
		Kept []int64           `json:"components_already_stored_and_rechecked"`
	}
	if loose(filepath.Join(dir, "second", "import-result.json"), &result) != nil || len(result.Read) != 0 || len(result.Kept) != 2 {
		t.Fatal("second run read a stored component again")
	}
	again, elsewhere2, historical2 := count()
	if again != payout || elsewhere2 != 0 || historical2 != historical {
		t.Fatal("second run changed the payout")
	}
	// The same database without its payout listed is refused, and so is one
	// whose sealed payout count differs.
	short := m
	short.CanonicalExport = &CanonicalExport{Records: x.Records, Proof: x.Proof, Rows: x.Rows + 1}
	d, e := openStagingDestination(context.Background(), dsn(importerRole), database, true)
	if e != nil {
		t.Fatal("destination")
	}
	defer d.close()
	if _, e := verifyStaged(context.Background(), d.conn, short); e == nil {
		t.Fatal("wrong payout count accepted")
	}
	if _, e := verifyStaged(context.Background(), d.conn, m); e != nil {
		t.Fatal("exact staged database refused")
	}
	t.Logf("payout rows %d on height %d, historical rows %d", payout, canonicalHeight, historical)
}
