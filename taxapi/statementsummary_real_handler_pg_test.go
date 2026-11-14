package taxapi

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Verify the actual JSON/CSV rendering over the real rows. Prices and readiness
// metadata are controlled fixtures shared by both paths, not production claims.
func TestStatementSummaryRealHandlerPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_REAL_COPY_PROOF") != statementRealCopyOptIn {
		t.Skip("explicit real-copy proof opt-in required")
	}
	schema := os.Getenv("COSMOS_TAX_REAL_PROOF_SCHEMA")
	require.Regexp(t, regexp.MustCompile(`^(public|statement_real_proof_[0-9]+)$`), schema)
	db, err := gorm.Open(postgres.Open(os.Getenv("COSMOS_TAX_TEST_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	requireRealProofDatabase(t, db)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(2)
	defer func() { _ = pool.Close() }()
	require.NoError(t, db.Exec("SET search_path="+schema).Error)
	require.NoError(t, db.Exec("SET TIME ZONE 'UTC'").Error)
	trust := mustTrust(t, db)
	require.True(t, trust.MetadataSame && trust.Reason() == "", "handler proof requires a genuinely trusted summary")
	require.NotNil(t, trust.BuiltBefore)
	require.True(t, trust.BuiltBefore.UTC().Equal(summaryDay(2026, 11, 1)), "complete the real-copy advance before handler proof")
	metadata := statementPGServer(t)
	var source string
	require.NoError(t, metadata.db.Raw("SELECT current_schema()").Row().Scan(&source))
	var tables []string
	require.NoError(t, db.Raw("SELECT tablename FROM pg_tables WHERE schemaname=? AND tablename<>'taxable_events'", source).Scan(&tables).Error)
	for _, name := range tables {
		require.True(t, regexp.MustCompile(`^[a-z_]+$`).MatchString(name))
		require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS "+name+" (LIKE "+source+"."+name+" INCLUDING ALL)").Error)
	}
	require.NoError(t, db.Exec("INSERT INTO chains SELECT 1,'cosmoshub-4' WHERE NOT EXISTS (SELECT 1 FROM chains)").Error)
	s := &Server{db: db, native: DefaultNativeAsset, oracle: metadata.oracle}
	vals, err := summaryValidators(db)
	require.NoError(t, err)
	for i, val := range vals {
		for _, format := range []string{"json", "csv"} {
			path := statementPGPath + val + "&end=2026-11-14"
			if format == "csv" {
				path += "&format=csv"
			}
			s.SetStatementSummary(true)
			s.SetStatementLimits(12_000_000, 90*time.Second)
			with := statementPGRequest(t, s, path)
			require.Equal(t, http.StatusOK, with.Code, "summary response failed, body withheld")
			// Only this isolated raw-reference request bypasses the public row cap;
			// the summary request above retains the deployed 12M/90s limits.
			s.SetStatementSummary(false)
			s.SetStatementLimits(40_000_000, 10*time.Minute)
			without := statementPGRequest(t, s, path)
			s.SetStatementLimits(12_000_000, 90*time.Second)
			require.Equal(t, http.StatusOK, without.Code, "reference response failed, body withheld")
			require.True(t, bytes.Equal(with.Body.Bytes(), without.Body.Bytes()), "JSON/CSV bytes differ; values withheld")
			sum := sha256.Sum256(with.Body.Bytes())
			t.Logf("real handler validator=%d format=%s exact_bytes=true sha256=%s", i+1, format, strings.ToLower(fmt.Sprintf("%x", sum)))
		}
	}
}
