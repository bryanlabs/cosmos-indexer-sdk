package main

import (
	"context"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"path/filepath"
)

// A refusal receipt is issued only after independently observing the complete
// populated target, an available lock, a healthy session and confinement.
func provePopulatedRefusal(out string) error {
	db, e := owner()
	if e != nil {
		return refused
	}
	pool, e := db.DB()
	if e != nil {
		return refused
	}
	defer pool.Close()
	var name string
	var count, first, last int64
	if db.Raw("SELECT current_database()").Scan(&name).Error != nil || name != database {
		return refused
	}
	if db.Raw("SELECT count(*) FROM legacy.blocks").Scan(&count).Error != nil || count != 1400000 {
		return refused
	}
	if db.Raw("SELECT min(height) FROM legacy.blocks WHERE chain_id='cosmoshub-2'").Scan(&first).Error != nil || first != 1 {
		return refused
	}
	if db.Raw("SELECT max(height) FROM legacy.blocks WHERE chain_id='cosmoshub-2'").Scan(&last).Error != nil || last != 1400000 {
		return refused
	}
	p, e := gorm.Open(postgres.Open(dsn(importerRole)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return refused
	}
	ip, e := p.DB()
	if e != nil {
		return refused
	}
	defer ip.Close()
	findings, e := schema.Confined(p)
	if e != nil || len(findings) != 0 {
		return refused
	}
	conn, e := ip.Conn(context.Background())
	if e != nil {
		return refused
	}
	defer conn.Close()
	var locked bool
	if conn.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock(340310)").Scan(&locked) != nil || !locked {
		return refused
	}
	if _, e = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(340310)"); e != nil {
		return refused
	}
	var result map[string]any
	if readJSON(filepath.Join(filepath.Dir(out), "import-result.json"), &result) != nil || result["heights"] != float64(1400000) {
		return refused
	}
	return save(out, map[string]any{"populated_destination_refused": true, "actual_heights": count, "first": first, "last": last, "identity": name, "confined_findings": 0, "lock_available": true, "healthy_private_connection": true})
}
