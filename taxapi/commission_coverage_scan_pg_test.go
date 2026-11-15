package taxapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Real PostgreSQL equivalence uses only thirteen synthetic blocks. Both readers
// run inside the very same repeatable-read transaction, not successive captures
// of a changing database. No production inputs or database are needed.
func TestPrivateSingleHoldingCoverageSameSnapshotPostgres(t *testing.T) {
	db := isolatedAPIFixtureDB(t)
	for _, statement := range []string{
		"CREATE TABLE tax_ledger_scans (chain text, fee_grants_from bigint, fee_grants_to bigint)",
		"CREATE TABLE blocks (chain_id bigint, height bigint, time_stamp timestamptz, tx_indexed boolean, block_events_indexed boolean)",
		"CREATE TABLE tax_index_holes (chain text, height bigint, addresses_known boolean)",
		"CREATE TABLE tax_index_hole_addresses (chain text, height bigint, address text)",
		"INSERT INTO tax_ledger_scans VALUES ('mainnet',1,20000013)",
		"INSERT INTO blocks SELECT 1,20000000+g,'2026-11-14T00:00:00Z',true,true FROM generate_series(1,13) g WHERE g <> 7",
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"block":{"header":{"height":"20000013"}}}`)
	}))
	defer server.Close()
	rest, err := servicehttp.New(server.Client(), server.URL)
	require.NoError(t, err)
	counts := 0
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:coverage_counts", func(q *gorm.DB) {
		if q.Statement.Table == "blocks" && strings.Contains(q.Statement.SQL.String(), "count(*)") {
			counts++
		}
	}))
	defer func() { _ = db.Callback().Query().Remove("test:coverage_counts") }()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var before, after string
		require.NoError(t, tx.Raw("SELECT txid_current_snapshot()::text").Scan(&before).Error)
		targets := []heightTime{{Height: 20000000}}
		baseline, baseAnchor, baseErr := (&Server{db: tx}).rebuildHoldings(apiMainnet, []int64{1}, &ledgerNode{rest: rest}, "fixture-wallet", targets)
		baseQueries := counts
		counts = 0
		candidate, anchor, err := (&Server{db: tx, readOnlyLedger: true}).rebuildHoldings(apiMainnet, []int64{1}, &ledgerNode{rest: rest}, "fixture-wallet", targets)
		require.NoError(t, baseErr)
		require.NoError(t, err)
		require.Equal(t, baseline, candidate)
		require.Equal(t, baseAnchor, anchor)
		require.Equal(t, balanceIncompleteS, candidate[0].Status)
		require.True(t, strings.HasPrefix(candidate[0].Reason, "1 blocks between this date"))
		require.Equal(t, 2, baseQueries)
		require.Equal(t, 1, counts)
		require.NoError(t, tx.Raw("SELECT txid_current_snapshot()::text").Scan(&after).Error)
		require.Equal(t, before, after)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}))
}
