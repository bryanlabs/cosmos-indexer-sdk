package db

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// A bulk Job's sessions carry report_evidence.bulk=on; others do not.
func TestReportEvidenceBulkSettingPostgres(t *testing.T) {
	if os.Getenv("COSMOS_TAX_TEST_PG_HOST") == "" {
		t.Skip("set COSMOS_TAX_TEST_PG_HOST")
	}
	show := func() string {
		db, err := PostgresDbConnect(os.Getenv("COSMOS_TAX_TEST_PG_HOST"), os.Getenv("COSMOS_TAX_TEST_PG_PORT"), "test", "test", "test", "silent")
		require.NoError(t, err)
		var v string
		require.NoError(t, db.Raw("SELECT coalesce(current_setting('report_evidence.bulk', true), '')").Row().Scan(&v))
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		return v
	}
	t.Setenv("REPORT_EVIDENCE_BULK", "")
	require.Equal(t, "", show())
	t.Setenv("REPORT_EVIDENCE_BULK", "on")
	require.Equal(t, "on", show())
}
