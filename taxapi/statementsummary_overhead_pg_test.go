// Write-shape literals are kept next to the scenario they measure.
//
//nolint:goconst
package taxapi

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// All measured writes, including DELETE, are rolled back on a new retained
// fixture made from the preserved real copy. No base rows are ever rewritten.
func TestStatementSummaryWriteOverheadPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	ctx := context.Background()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	requireRealProofDatabase(t, db)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	defer func() { _ = pool.Close() }()
	schema := fmt.Sprintf("statement_write_proof_%d", time.Now().UnixNano())
	t.Logf("retained write fixture schema=%s", schema)
	for _, q := range []string{"CREATE SCHEMA " + schema, "SET search_path=" + schema, "CREATE TABLE taxable_events AS SELECT * FROM public.taxable_events LIMIT 10000"} {
		require.NoError(t, db.Exec(q).Error)
	}
	var indexes []string
	require.NoError(t, db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='taxable_events' ORDER BY indexname").Scan(&indexes).Error)
	for _, ddl := range indexes {
		require.NoError(t, db.Exec(strings.ReplaceAll(ddl, "public.taxable_events", schema+".taxable_events")).Error)
	}
	require.NoError(t, InstallStatementSummary(ctx, db, 0, 1, nil))
	require.NoError(t, db.Exec("UPDATE statement_state SET log_before='2026-11-01'").Error)
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	var val string
	require.NoError(t, conn.QueryRow(ctx, "SELECT validator_address FROM taxable_events WHERE validator_address<>'' LIMIT 1").Scan(&val))
	columns := []string{"id", "message_id", "sub_index", "category", "amount", "denom", "to_addr", "timestamp", "validator_address"}
	sample := make([][]any, 1000)
	for i := range sample {
		sample[i] = []any{int64(-100000 - i), int64(-100000 - i), int64(0), "reward", "7", "uatom", "synthetic-recipient", summaryDay(2025, 6, 15), val}
	}
	shapes := []string{"indexer-tip", "indexer-history", "repair-1000", "delete-10000", "insert-1000", "copy-1000"}
	results := map[string][]time.Duration{}
	for _, enabled := range []bool{false, true} {
		mode := "off"
		if enabled {
			mode = "on"
		}
		for _, name := range statementTriggerNames {
			action := "DISABLE"
			if enabled {
				action = "ENABLE ALWAYS"
			}
			require.NoError(t, db.Exec("ALTER TABLE taxable_events "+action+" TRIGGER "+name).Error)
		}
		for _, shape := range shapes {
			for repeat := 0; repeat < 4; repeat++ {
				tx, err := conn.Begin(ctx)
				require.NoError(t, err)
				run := func(q string, args ...any) { _, err := tx.Exec(ctx, q, args...); require.NoError(t, err) }
				// Preparation is outside the measured interval and remains rollback-only.
				if shape == "repair-1000" {
					run("CREATE TEMP TABLE repair_rows AS SELECT * FROM taxable_events LIMIT 1000")
				}
				began := time.Now()
				switch shape {
				case "indexer-tip", "indexer-history":
					day := summaryDay(2026, 11, 12)
					if shape == "indexer-history" {
						day = summaryDay(2025, 6, 15)
					}
					for message := 0; message < 100; message++ {
						run("SAVEPOINT message_write")
						for pass := 0; pass < 2; pass++ {
							for sub := 0; sub < 3; sub++ {
								run("INSERT INTO taxable_events(id,message_id,sub_index,category,amount,denom,to_addr,timestamp,validator_address) VALUES($1,$2,$3,'reward','7','uatom','synthetic-recipient',$4,$5) ON CONFLICT(message_id,sub_index) DO UPDATE SET category=EXCLUDED.category,amount=EXCLUDED.amount,denom=EXCLUDED.denom,to_addr=EXCLUDED.to_addr,timestamp=EXCLUDED.timestamp,validator_address=EXCLUDED.validator_address", -100000-message*3-sub, -100000-message, sub, day, val)
							}
						}
						run("DELETE FROM taxable_events WHERE message_id=$1 AND sub_index>=3", -100000-message)
						run("RELEASE SAVEPOINT message_write")
					}
				case "repair-1000":
					run("DELETE FROM taxable_events WHERE message_id IN (SELECT message_id FROM repair_rows)")
					run("INSERT INTO taxable_events SELECT * FROM repair_rows")
				case "delete-10000":
					run("DELETE FROM taxable_events")
				case "insert-1000":
					run("INSERT INTO taxable_events(id,message_id,sub_index,category,amount,denom,to_addr,timestamp,validator_address) SELECT -200000-g,-200000-g,0,'reward','7','uatom','synthetic-recipient','2025-06-15'::timestamptz,$1 FROM generate_series(1,1000) g", val)
				case "copy-1000":
					_, err := tx.CopyFrom(ctx, pgx.Identifier{"taxable_events"}, columns, pgx.CopyFromRows(sample))
					require.NoError(t, err)
				}
				elapsed := time.Since(began)
				require.NoError(t, tx.Rollback(ctx))
				if repeat > 0 {
					key := shape + "/" + mode
					results[key] = append(results[key], elapsed)
				}
			}
		}
	}
	for _, shape := range shapes {
		off, on := results[shape+"/off"], results[shape+"/on"]
		sort.Slice(off, func(i, j int) bool { return off[i] < off[j] })
		sort.Slice(on, func(i, j int) bool { return on[i] < on[j] })
		t.Logf("shape=%s baseline_median=%s trigger_median=%s delta=%s ratio=%.3f repeats=3 rollback=true", shape, off[1], on[1], on[1]-off[1], float64(on[1])/float64(off[1]))
	}
	require.Equal(t, int64(10000), summaryCount(t, db, "SELECT count(*) FROM taxable_events"))
	require.Zero(t, summaryCount(t, db, "SELECT COALESCE(pg_sequence_last_value('statement_log_failures'::regclass),0)"))
}
