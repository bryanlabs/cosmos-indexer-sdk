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

// Verify the already completed fill from the actual pinned-image executable.
// The CLI uses its normal public schema, just as production does. PGOPTIONS
// is not supported by its pgx connection configuration. References remain in
// their retained raw-validated schema. A test-driver repair does not require
// repeating a successful fill when the final executable is byte-identical.
func TestStatementSummaryFinalBuildRealCopyPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	require.Equal(t, "public", os.Getenv("COSMOS_TAX_REAL_PROOF_SCHEMA"))
	archive := os.Getenv("COSMOS_TAX_REAL_REFERENCE_SCHEMA")
	require.Regexp(t, regexp.MustCompile(`^statement_final_reference_[0-9]+$`), archive)
	clone := os.Getenv("COSMOS_TAX_REAL_IMMUTABLE_CLONE_SCHEMA")
	require.Regexp(t, regexp.MustCompile(`^statement_real_proof_[0-9]+$`), clone)
	binary := os.Getenv("COSMOS_TAX_FINAL_BINARY")
	require.Regexp(t, regexp.MustCompile(`^/Users/danb/\.cache/stmt2-artifact-[0-9a-f]{40}/taxstatement$`), binary)
	logDir := filepath.Join(filepath.Dir(binary), "proof-logs")
	// #nosec G703 -- Opt-in local artifact path is constrained above; private logs stay beside it.
	require.NoError(t, os.MkdirAll(logDir, 0o700))
	cfg, err := pgx.ParseConfig(os.Getenv("COSMOS_TAX_TEST_DSN"))
	require.True(t, err == nil, "invalid isolated connection, details withheld")
	require.True(t, cfg.Host == "127.0.0.1" && cfg.Port == 25433 && cfg.Database == apiTest && cfg.User == apiTest, "artifact proof requires the dedicated test-pod endpoint")
	ctx, cancel := context.WithTimeout(context.Background(), 19*time.Minute)
	defer cancel()
	runArtifact := func(args ...string) []byte {
		t.Helper()
		// #nosec G702 -- Execute the explicitly selected, digest-verified local test artifact.
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "DB_HOST="+cfg.Host, "DB_PORT="+strconv.Itoa(int(cfg.Port)), "DB_NAME="+cfg.Database, "DB_USER="+cfg.User, "DB_PASS="+cfg.Password, "DB_LOG_LEVEL=silent")
		out, runErr := cmd.CombinedOutput()
		path := filepath.Join(logDir, fmt.Sprintf("action_%d.log", time.Now().UnixNano()))
		// #nosec G703 -- Constrained artifact directory and generated numeric filename.
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
	summaryExec(t, db, "SET search_path=public")
	summaryExec(t, db, "SET TIME ZONE 'UTC'")
	trust := mustTrust(t, db)
	require.True(t, trust.MetadataSame && trust.Reason() == "", "actual artifact build must be trusted before acceptance")
	require.NotNil(t, trust.BuiltBefore)
	require.True(t, trust.BuiltBefore.UTC().Equal(summaryDay(2026, 11, 1)))
	runArtifact("-status", "-require-trusted")
	assertImmutable := func() {
		var unchanged bool
		require.NoError(t, db.Raw("WITH p AS MATERIALIZED (SELECT count(*) n,min(age(xmin)) a FROM ONLY public.taxable_events),c AS MATERIALIZED (SELECT count(*) n,min(age(xmin)) a,max(age(xmin)) b FROM ONLY "+clone+".taxable_events),r AS (SELECT age(xmin) a FROM "+archive+".statement_state) SELECT p.n=33995171 AND c.n=33995171 AND c.a=c.b AND p.a>c.a AND c.a>r.a FROM p,c,r").Row().Scan(&unchanged))
		require.True(t, unchanged, "immutable source/reference binding changed; values withheld")
	}
	assertImmutable()
	for _, table := range []string{statementDaysTable, statementDelegatorsTable} {
		var original bool
		require.NoError(t, db.Raw("SELECT min(age(e.xmin))>=(SELECT age(s.xmin) FROM "+archive+".statement_state s) FROM "+archive+"."+table+" e").Row().Scan(&original))
		require.True(t, original, "reference rows changed after publication; values withheld")
		var differences int64
		query := "WITH differences AS ((TABLE " + archive + "." + table + " EXCEPT TABLE public." + table + ") UNION ALL (TABLE public." + table + " EXCEPT TABLE " + archive + "." + table + ")) SELECT count(*) FROM differences"
		require.NoError(t, db.Raw(query).Row().Scan(&differences))
		require.Zero(t, differences, "full reference row-set comparison failed; values withheld")
		require.Positive(t, summaryCount(t, db, "SELECT count(*) FROM "+table))
		t.Logf("final artifact table=%s all_rows=true differences=0", table)
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
