package taxapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The unchanged public path is the baseline. Both paths use the same indexed
// block fixture and must produce identical statuses, reasons and native values.
func coverageScanFixture(t *testing.T, private bool, targets []heightTime, grantsFrom int64, countError bool) ([]walletDay, int64, error, int) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: raw}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	s := &Server{db: db, readOnlyLedger: private}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/cosmos/base/tendermint/v1beta1/blocks/latest", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"block":{"header":{"height":"25000000"}}}`)
	}))
	t.Cleanup(server.Close)
	rest, err := servicehttp.New(server.Client(), server.URL)
	require.NoError(t, err)
	mock.ExpectQuery(`SELECT .*tax_ledger_scans`).WillReturnRows(sqlmock.NewRows([]string{"chain", "fee_grants_from", "fee_grants_to"}).AddRow(apiMainnet, grantsFrom, 25000000))
	mock.ExpectQuery(`SELECT height, time_stamp FROM "blocks"`).WithArgs(int64(1), int64(25000000)).WillReturnRows(sqlmock.NewRows([]string{"height", "time_stamp"}).AddRow(25000000, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)))
	counts := 0
	count := func() {
		counts++
		expectation := mock.ExpectQuery(`SELECT count\(\*\) FROM "blocks"`).WithArgs(int64(1), int64(20000000), int64(25000000)).WillDelayFor(time.Millisecond)
		if countError {
			expectation.WillReturnError(fmt.Errorf("fixture count unavailable"))
		} else {
			expectation.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4999999))
		}
	}
	shared := !private || len(targets) > 1
	if shared {
		count()
	}
	if !countError || !shared {
		mock.ExpectQuery(`SELECT h.height FROM tax_index_holes`).WillReturnRows(sqlmock.NewRows([]string{"height"}))
		for range targets {
			if grantsFrom > 0 && grantsFrom <= 20000001 {
				count()
				if countError {
					break
				}
			}
		}
	}
	out, anchor, callErr := s.rebuildHoldings(apiMainnet, []int64{1}, &ledgerNode{rest: rest}, "fixture-wallet", targets)
	require.NoError(t, mock.ExpectationsWereMet())
	return out, anchor, callErr, counts
}

func TestPrivateSingleHoldingAvoidsRepeatedIncompleteCoverageScan(t *testing.T) {
	target := []heightTime{{Height: 20000000}}
	for _, grantsFrom := range []int64{1, 20000002, 0} {
		t.Run(fmt.Sprint(grantsFrom), func(t *testing.T) {
			started := time.Now()
			baseline, baseAnchor, baseErr, baseQueries := coverageScanFixture(t, false, target, grantsFrom, false)
			baselineElapsed := time.Since(started)
			started = time.Now()
			candidate, anchor, err, queries := coverageScanFixture(t, true, target, grantsFrom, false)
			candidateElapsed := time.Since(started)
			require.NoError(t, baseErr)
			require.NoError(t, err)
			require.Equal(t, baseline, candidate)
			require.Equal(t, baseAnchor, anchor)
			require.Equal(t, baseQueries-1, queries)
			t.Logf("identical full holding output: block count queries %d -> %d; fixture wall time %s -> %s (1ms injected per count, not a production speed forecast)", baseQueries, queries, baselineElapsed, candidateElapsed)
		})
	}
}

func TestPrivateSingleHoldingCoverageErrorIsNotSuppressed(t *testing.T) {
	_, _, err, queries := coverageScanFixture(t, true, []heightTime{{Height: 20000000}}, 1, true)
	require.EqualError(t, err, "fixture count unavailable")
	require.Equal(t, 1, queries)
}

func TestPrivateMultiHoldingCoverageIsUnchanged(t *testing.T) {
	targets := []heightTime{{Height: 20000000}, {Height: 20000000}}
	baseline, baseAnchor, baseErr, baseQueries := coverageScanFixture(t, false, targets, 1, false)
	candidate, anchor, err, queries := coverageScanFixture(t, true, targets, 1, false)
	require.NoError(t, baseErr)
	require.NoError(t, err)
	require.Equal(t, baseline, candidate)
	require.Equal(t, baseAnchor, anchor)
	require.Equal(t, baseQueries, queries)
}
