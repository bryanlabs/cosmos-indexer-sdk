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
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, from_addr text, to_addr text, denom text, category text, timestamp timestamptz)",
		"CREATE INDEX ON taxable_events (from_addr)",
		"CREATE INDEX ON taxable_events (to_addr)",
		"CREATE INDEX ON taxable_events (timestamp)",
		"CREATE INDEX ON taxable_events (from_addr, denom, timestamp) INCLUDE (category, to_addr)",
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

	// A period inside the stored span cannot use the all-time received count
	// as exact: wallet x has four rows in the period, and one of its three
	// received rows is from 2021, outside it.
	s.SetReportRowCap(4)
	require.NoError(t, s.checkReportRowCap([]string{"x"}, start, end))
	s.SetReportRowCap(3)
	require.Error(t, s.checkReportRowCap([]string{"x"}, start, end))

	// readReportInput refuses before it opens the report snapshot.
	s.SetReportRowCap(3)
	_, err = s.readReportInput(chainParam(""), []string{"a"}, start, end, false)
	require.True(t, errors.As(err, &tooLarge))
}

// TestReportRowCapBoundsMatchExactCount is the verdict comparator: on random
// wallets, periods and caps the bounded check refuses exactly when the exact
// count does, and every path (refused by the bounds, allowed by the bounds,
// decided by the exact count) is taken.
func TestReportRowCapBoundsMatchExactCount(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)
	for _, sql := range []string{
		tempSearchPath,
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, from_addr text, to_addr text, denom text, category text, timestamp timestamptz)",
		"CREATE INDEX ON taxable_events (to_addr)",
		"CREATE INDEX ON taxable_events (from_addr)",
		"CREATE INDEX ON taxable_events (timestamp)",
		"CREATE INDEX ON taxable_events (from_addr, denom, timestamp) INCLUDE (category, to_addr)",
		// Wallets w0..w7 with skewed sizes; w0 mostly receives, w1 mostly
		// sends, w2 sends to itself, years 2019 to 2024.
		`INSERT INTO taxable_events (from_addr, to_addr, denom, category, timestamp)
		 SELECT CASE WHEN r < 0.15 THEN 'w1' WHEN r < 0.25 THEN 'w2' ELSE 'w' || (3 + (i % 5)) END,
		        CASE WHEN r2 < 0.35 THEN 'w0' WHEN r < 0.25 AND r >= 0.15 AND r2 < 0.6 THEN 'w2' ELSE 'w' || (i % 8) END,
		        'uatom', 'transfer', timestamptz '2019-01-01' + (i % 2190) * interval '1 day'
		 FROM (SELECT i, random() AS r, random() AS r2 FROM generate_series(1, 6000) i) g`,
		"ANALYZE taxable_events",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	exact := func(addrs []string, start, end time.Time) int64 {
		var total int64
		for _, a := range addrs {
			var n int64
			require.NoError(t, database.Raw(`SELECT count(*) FROM taxable_events WHERE (from_addr = ? OR to_addr = ?) AND timestamp >= ? AND timestamp < ?`, a, a, start, end).Scan(&n).Error)
			total += n
		}
		return total
	}
	periods := [][2]time.Time{
		{time.Time{}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2023, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	wallets := [][]string{{"w0"}, {"w1"}, {"w2"}, {"w5"}, {"w0", "w1"}, {"w2", "w3", "w4"}, {"nobody"}}
	s := &Server{db: database}
	paths := map[string]int{}
	for _, p := range periods {
		for _, w := range wallets {
			n := exact(w, p[0], p[1])
			for _, c := range []int64{1, n / 3, n / 2, n - 1, n, n + 1, 2 * n, 3000} {
				if c <= 0 {
					continue
				}
				s.SetReportRowCap(c)
				got := s.checkReportRowCap(w, p[0], p[1])
				var tooLarge reportTooLarge
				require.Equal(t, n > c, errors.As(got, &tooLarge), "wallets %v period %v cap %d exact %d: %v", w, p, c, n, got)
				bounds, err := s.reportRowBounds(w, p[0], p[1])
				require.NoError(t, err)
				var lower, upper int64
				for _, b := range bounds {
					lower += b.lower
					if upper >= 0 && b.upper >= 0 {
						upper += b.upper
					} else {
						upper = -1
					}
				}
				require.LessOrEqual(t, min(lower, c+1), n, "lower bound over the exact count")
				switch {
				case lower > c:
					paths["refused by bounds"]++
				case upper >= 0 && upper <= c:
					require.GreaterOrEqual(t, upper, n, "upper bound under the exact count")
					paths["allowed by bounds"]++
				default:
					paths["exact count"]++
				}
			}
		}
	}
	t.Logf("paths %v", paths)
	for _, p := range []string{"refused by bounds", "allowed by bounds", "exact count"} {
		require.Positive(t, paths[p], p)
	}
}
