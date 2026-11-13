// Keep the exact artifact commands and fixture SQL visible beside the proof.
//
//nolint:goconst
package taxapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Rebuild with the binary extracted from the final pinned image, not an
// earlier test binary. Compare every derived row and identity with retained
// raw-validated references, then recheck every current cell against raw data.
func TestStatementSummaryFinalBuildRealCopyPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	schema := os.Getenv("COSMOS_TAX_REAL_PROOF_SCHEMA")
	require.Regexp(t, regexp.MustCompile(`^statement_real_proof_[0-9]+$`), schema)
	binary := os.Getenv("COSMOS_TAX_FINAL_BINARY")
	require.NotEmpty(t, binary, "extract taxstatement from the final digest-pinned candidate first")
	require.Regexp(t, regexp.MustCompile(`^/Users/danb/\.cache/stmt2-artifact-[0-9a-f]{40}/taxstatement$`), binary)
	logDir := filepath.Join(filepath.Dir(binary), "proof-logs")
	// #nosec G703 -- Explicit opt-in artifact path is constrained above; logs stay beside that local artifact.
	require.NoError(t, os.MkdirAll(logDir, 0o700))
	cfg, err := pgx.ParseConfig(os.Getenv("COSMOS_TAX_TEST_DSN"))
	require.True(t, err == nil, "invalid isolated connection, details withheld")
	require.True(t, cfg.Host == "127.0.0.1" && cfg.Port == 25433 && cfg.Database == "test" && cfg.User == "test", "artifact proof requires the dedicated test-pod endpoint")
	ctx, cancel := context.WithTimeout(context.Background(), 19*time.Minute)
	defer cancel()
	runArtifact := func(args ...string) []byte {
		t.Helper()
		// #nosec G702 -- Executing the explicitly selected local, digest-verified test artifact is this proof's purpose.
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "DB_HOST="+cfg.Host, "DB_PORT="+strconv.Itoa(int(cfg.Port)), "DB_NAME="+cfg.Database, "DB_USER="+cfg.User, "DB_PASS="+cfg.Password, "DB_LOG_LEVEL=silent", "PGOPTIONS=-c search_path="+schema)
		out, runErr := cmd.CombinedOutput()
		path := filepath.Join(logDir, fmt.Sprintf("action_%d.log", time.Now().UnixNano()))
		// #nosec G703 -- Path is derived from the constrained artifact directory and a generated numeric filename.
		file, fileErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		require.NoError(t, fileErr)
		_, fileErr = file.Write(out)
		require.NoError(t, fileErr)
		require.NoError(t, file.Close())
		require.NoError(t, runErr, "artifact action failed; output withheld and retained privately")
		t.Logf("artifact action=%s passed output_sha256=%x", args[0], sha256.Sum256(out))
		return out
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("COSMOS_TAX_TEST_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	requireRealProofDatabase(t, db)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(2)
	defer func() { _ = pool.Close() }()
	summaryExec(t, db, "SET search_path="+schema)
	summaryExec(t, db, "SET TIME ZONE 'UTC'")
	trust := mustTrust(t, db)
	// A preceding protocol's reference is evidence, not permission to serve it
	// with this binary. A missing new guard must not prevent preserving that
	// reference before installation. The full raw recheck below is mandatory.
	require.True(t, trust.Installed && trust.SameTable && trust.PlainTable)
	require.Equal(t, trust.Failures, trust.Acked)
	require.False(t, trust.AllDirty)
	require.NotNil(t, trust.BuiltBefore)
	require.True(t, trust.BuiltBefore.UTC().Equal(summaryDay(2026, 11, 1)), "finish the preceding raw-reference proof first")
	archive := fmt.Sprintf("statement_final_reference_%d", time.Now().UnixNano())
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE SCHEMA " + archive).Error; err != nil {
			return err
		}
		for _, table := range []string{statementDaysTable, statementDelegatorsTable, statementDirtyTable, statementStateTable} {
			if err := tx.Exec("ALTER TABLE " + table + " SET SCHEMA " + archive).Error; err != nil {
				return err
			}
		}
		return tx.Exec("ALTER SEQUENCE statement_log_failures SET SCHEMA " + archive).Error
	}))
	t.Logf("raw-validated reference retained intact in %s; base untouched", archive)
	assertImmutable := func() {
		var unchanged bool
		// Original CTAS tuple versions must predate the reference publication.
		// Later insertion/update changes xmin; deletion changes the row count.
		require.NoError(t, db.Raw("SELECT count(*)=33995171 AND min(age(e.xmin))=max(age(e.xmin)) AND min(age(e.xmin))>(SELECT age(s.xmin) FROM "+archive+".statement_state s) FROM ONLY taxable_events e").Row().Scan(&unchanged))
		require.True(t, unchanged, "immutable source/reference binding changed; values withheld")
	}
	assertImmutable()
	for _, table := range []string{statementDaysTable, statementDelegatorsTable} {
		var original bool
		require.NoError(t, db.Raw("SELECT min(age(e.xmin))>(SELECT age(s.xmin) FROM "+archive+".statement_state s) FROM "+archive+"."+table+" e").Row().Scan(&original))
		require.True(t, original, "reference rows changed after publication; values withheld")
	}
	runArtifact("-install", "-attempts=1")
	began := time.Now()
	runArtifact("-fill", "-as-of=2026-11-13", "-require-trusted")
	t.Logf("final artifact full-source fill duration=%s", time.Since(began))
	for _, table := range []string{statementDaysTable, statementDelegatorsTable} {
		var differences int64
		query := "WITH differences AS ((TABLE " + archive + "." + table + " EXCEPT TABLE " + schema + "." + table + ") UNION ALL (TABLE " + schema + "." + table + " EXCEPT TABLE " + archive + "." + table + ")) SELECT count(*) FROM differences"
		require.NoError(t, db.Raw(query).Row().Scan(&differences))
		require.Zero(t, differences, "full reference row-set comparison failed; values withheld")
		require.Positive(t, summaryCount(t, db, "SELECT count(*) FROM "+table))
		t.Logf("final table=%s all_rows=true differences=0", table)
	}
	out := runArtifact("-check=0", "-seed=1", "-require-trusted")
	match := regexp.MustCompile(`statement-summary check: ([0-9]+) cells equal taxable_events`).FindSubmatch(out)
	require.Len(t, match, 2, "artifact did not report a completed exact check; output withheld")
	checked, err := strconv.Atoi(string(match[1]))
	require.NoError(t, err)
	require.Equal(t, 207, checked)
	assertImmutable()
	status, err := ReadStatementSummaryStatus(ctx, db)
	require.NoError(t, err)
	require.True(t, status.Trusted)
	t.Logf("final artifact all-cell raw check cells=%d mismatches=0 immutable_source=true trusted=true", checked)
}
