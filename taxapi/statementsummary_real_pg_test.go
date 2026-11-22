package taxapi

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const statementRealCopyOptIn = "stmt2-testpg"

func requireRealProofDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	var database, user string
	require.NoError(t, db.Raw("SELECT current_database(), session_user").Row().Scan(&database, &user))
	require.True(t, database == apiTest && user == apiTest, "real-copy proofs require the isolated test database and test role")
}

func requireRealStatementEqual(t *testing.T, want, got statementAggregates) {
	t.Helper()
	require.True(t, reflect.DeepEqual(statementDayKeys(want.Days), statementDayKeys(got.Days)), "daily amounts or counts differ; values withheld")
	require.Equal(t, want.DelegatorsByMonth, got.DelegatorsByMonth)
	require.Equal(t, want.Delegators, got.Delegators)
}

// This opt-in proof reads the preserved public copy only. All summary objects
// and writes belong to a new, retained schema. Never point it at production.
func TestStatementSummaryRealCopyPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("COSMOS_TAX_TEST_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	requireRealProofDatabase(t, db)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = pool.Close() })
	schema := os.Getenv("COSMOS_TAX_REAL_PROOF_SCHEMA")
	resume := schema != ""
	if resume {
		require.Regexp(t, regexp.MustCompile(`^statement_real_proof_[0-9]+$`), schema)
	} else {
		schema = fmt.Sprintf("statement_real_proof_%d", time.Now().UnixNano())
		require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
	}
	t.Logf("retained real proof schema=%s resume=%t", schema, resume)
	require.NoError(t, db.Exec("SET search_path="+schema).Error)
	require.NoError(t, db.Exec("SET TIME ZONE 'UTC'").Error)
	if resume && os.Getenv("COSMOS_TAX_REAL_PROOF_ARCHIVE_PREVIOUS") == "1" {
		// Retain every earlier summary row and counter before installing a
		// revised implementation. The immutable taxable_events copy stays put.
		archive := fmt.Sprintf("statement_proof_evidence_%d", time.Now().UnixNano())
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("CREATE SCHEMA " + archive).Error; err != nil {
				return err
			}
			for _, table := range []string{statementDaysTable, statementDelegatorsTable, statementDirtyTable, statementStateTable} {
				if err := tx.Exec("ALTER TABLE " + table + " SET SCHEMA " + archive).Error; err != nil {
					return err
				}
			}
			if err := tx.Exec("ALTER SEQUENCE statement_log_failures SET SCHEMA " + archive).Error; err != nil {
				return err
			}
			return InstallStatementSummary(context.Background(), tx, 0, 1, nil)
		}))
		t.Logf("previous summary objects retained intact in %s; base untouched", archive)
	}
	if !resume {
		began := time.Now()
		require.NoError(t, db.Exec("CREATE TABLE taxable_events AS TABLE public.taxable_events").Error)
		t.Logf("copied real rows in %s", time.Since(began))
		var indexes []string
		require.NoError(t, db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='taxable_events' ORDER BY indexname").Scan(&indexes).Error)
		for _, ddl := range indexes {
			require.NoError(t, db.Exec(strings.ReplaceAll(ddl, "public.taxable_events", schema+".taxable_events")).Error)
		}
		require.NoError(t, db.Exec("ANALYZE taxable_events").Error)
		require.NoError(t, InstallStatementSummary(context.Background(), db, 0, 1, nil))
	}
	require.Equal(t, int64(33_995_171), summaryCount(t, db, "SELECT count(*) FROM taxable_events"))
	ctx := context.Background()
	s := &Server{db: db}
	vals, err := summaryValidators(db)
	require.NoError(t, err)
	require.Len(t, vals, 3)
	windows := []struct {
		name   string
		lo, hi time.Time
	}{
		{"all", time.Time{}, summaryDay(2026, 11, 14)},
		{"closed", summaryDay(2025, 1, 1), summaryDay(2026, 1, 1)},
		{"mixed", summaryDay(2025, 3, 17), summaryDay(2026, 11, 10)},
		{"open", summaryDay(2026, 11, 1), summaryDay(2026, 11, 14)},
	}
	// This base fixture is immutable throughout both watermark builds. Reuse
	// each independently computed raw reference, never a summary result.
	references := map[string]statementAggregates{}
	compare := func(phase string) {
		for i, val := range vals {
			for _, w := range windows {
				began := time.Now()
				// These are the three original raw-table queries, not the planning-only
				// readStatement mode. They have no request row cap on this isolated copy.
				key := val + "/" + w.name
				want, cached := references[key]
				if !cached {
					want = legacyStatementAggregates(t, s, val, w.lo, w.hi)
					references[key] = want
				}
				liveTime := time.Since(began)
				if w.name == "all" {
					require.NotEmpty(t, want.Days)
					require.Positive(t, want.Delegators)
				}
				s.SetStatementSummary(true)
				began = time.Now()
				got, err := s.readStatement(ctx, val, w.lo, w.hi, true)
				require.NoError(t, err)
				summaryTime := time.Since(began)
				requireRealStatementEqual(t, want, got.Agg)
				require.Equal(t, w.name != "open", got.Plan.Summary)
				t.Logf("%s validator=%d window=%s exact=true unique_union=true raw_reference=%s reference_cached=%t summary=%s used=%t", phase, i+1, w.name, liveTime, cached, summaryTime, got.Plan.Summary)
			}
		}
	}
	trust := mustTrust(t, db)
	if trust.BuiltBefore == nil {
		began := time.Now()
		_, err = FillStatementSummary(ctx, db, false, summaryOpts(summaryDay(2025, 7, 10)))
		require.NoError(t, err)
		t.Logf("fill historical watermark duration=%s", time.Since(began))
	} else {
		require.Equal(t, summaryDay(2025, 7, 1), trust.BuiltBefore.UTC(), "resume only the retained historical-watermark proof, do not silently alter evidence")
	}
	compare("historical watermark")
	check, err := CheckStatementSummary(ctx, db, 0, 1, StatementSummaryOptions{})
	require.NoError(t, err)
	require.Empty(t, check.Mismatches)
	require.Equal(t, 159, check.Checked)
	t.Logf("historical all-cell check cells=%d mismatches=0", check.Checked)
	began := time.Now()
	advance, err := RefreshStatementSummary(ctx, db, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.Empty(t, advance.Waiting)
	require.True(t, advance.Advanced)
	require.True(t, advance.BuiltBefore.UTC().Equal(summaryDay(2026, 11, 1)))
	t.Logf("advance duration=%s", time.Since(began))
	compare("current watermark")
	check, err = CheckStatementSummary(ctx, db, 0, 1, StatementSummaryOptions{})
	require.NoError(t, err)
	require.Empty(t, check.Mismatches)
	require.Equal(t, 207, check.Checked)
	require.Equal(t, int64(33_995_171), summaryCount(t, db, "SELECT count(*) FROM taxable_events"))
	status, err := ReadStatementSummaryStatus(ctx, db)
	require.NoError(t, err)
	require.True(t, status.Trusted)
	t.Logf("current all-cell check cells=%d mismatches=0 summary_bytes=%d trusted=%t", check.Checked, status.SummaryBytes, status.Trusted)
}
