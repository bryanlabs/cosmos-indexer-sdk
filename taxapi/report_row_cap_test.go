package taxapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestReportTooLargeAnswers422WithTheLimitOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTaxError(rec, reportTooLarge{Limit: DefaultReportRowCap})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "250,000")
	require.Contains(t, body, "shorter period")
	require.Less(t, len(strings.TrimSpace(body)), 300, "the website shows the first 300 characters")
}

func TestReportRowCapOffReadsNothing(t *testing.T) {
	s := &Server{}
	require.NoError(t, s.checkReportRowCap([]string{"a"}, time.Time{}, time.Now()))
	s.SetReportRowCap(-1)
	require.Zero(t, s.reportRowCap)
}

func TestReportRowCapPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for the native Postgres report row cap check")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)
	for _, sql := range []string{
		tempSearchPath,
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, from_addr text, to_addr text, timestamp timestamptz)",
		"CREATE INDEX ON taxable_events (from_addr)",
		"CREATE INDEX ON taxable_events (to_addr)",
		"CREATE INDEX ON taxable_events (timestamp)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	in := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	out := time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)
	rows := []struct {
		from, to string
		at       time.Time
	}{
		{"a", "x", in},
		{"x", "a", in},
		{"a", "a", in},
		{"a", "y", in},
		{"a", "x", out},
		{"x", "a", out},
		{"b", "x", in},
		{"x", "b", in},
	}
	for _, r := range rows {
		require.NoError(t, database.Exec("INSERT INTO taxable_events (from_addr, to_addr, timestamp) VALUES (?, ?, ?)", r.from, r.to, r.at).Error)
	}
	start, end := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{db: database}

	// Wallet a has four rows in the period: the self-transfer counts once and
	// the two 2021 rows are outside it.
	s.SetReportRowCap(4)
	require.NoError(t, s.checkReportRowCap([]string{"a"}, start, end))
	s.SetReportRowCap(3)
	var tooLarge reportTooLarge
	require.True(t, errors.As(s.checkReportRowCap([]string{"a"}, start, end), &tooLarge))
	require.Equal(t, int64(3), tooLarge.Limit)

	// The whole history counts both years.
	s.SetReportRowCap(6)
	require.NoError(t, s.checkReportRowCap([]string{"a"}, time.Time{}, end))
	s.SetReportRowCap(5)
	require.Error(t, s.checkReportRowCap([]string{"a"}, time.Time{}, end))

	// Wallets in one report share the cap.
	s.SetReportRowCap(6)
	require.NoError(t, s.checkReportRowCap([]string{"a", "b"}, start, end))
	s.SetReportRowCap(5)
	require.True(t, errors.As(s.checkReportRowCap([]string{"a", "b"}, start, end), &tooLarge))

	// readReportInput refuses before it opens the report snapshot.
	s.SetReportRowCap(3)
	_, err = s.readReportInput(chainParam(""), []string{"a"}, start, end, false)
	require.True(t, errors.As(err, &tooLarge))
}
