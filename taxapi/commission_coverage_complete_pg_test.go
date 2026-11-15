package taxapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Unlike the missing-block fixture, this reaches the usable native holdings
// path, including a nonzero synthetic bank anchor and the actual ledger SQL.
func TestPrivateSingleHoldingCompleteCoverageSameSnapshotPostgres(t *testing.T) {
	db := isolatedAPIFixtureDB(t)
	require.NoError(t, dbpkg.MigrateModels(db))
	require.NoError(t, db.AutoMigrate(&LedgerScan{}, &IndexHole{}, &IndexHoleAddress{}, &FeeGrantUse{}, &FailedTxFee{}))
	for _, statement := range []string{
		"INSERT INTO chains(id,chain_id) VALUES (1,'cosmoshub-4')",
		"INSERT INTO addresses(id,address) VALUES (1,'fixture-wallet')",
		"INSERT INTO tax_ledger_scans VALUES ('mainnet',1,20000013)",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed,block_events_indexed) SELECT g,1,1,20000000+g,'2026-11-14T00:00:00Z',true,true FROM generate_series(1,13) g",
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	var anchorReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/cosmos/base/tendermint/v1beta1/blocks/latest" {
			_, _ = fmt.Fprint(w, `{"block":{"header":{"height":"20000013"}}}`)
			return
		}
		require.Equal(t, "20000013", r.Header.Get(heightHeader))
		anchorReads.Add(1)
		switch r.URL.Path {
		case "/cosmos/bank/v1beta1/balances/fixture-wallet":
			_, _ = fmt.Fprint(w, `{"balances":[{"denom":"uatom","amount":"1000000"}],"pagination":{"next_key":""}}`)
		case "/cosmos/staking/v1beta1/delegations/fixture-wallet":
			_, _ = fmt.Fprint(w, `{"delegation_responses":[],"pagination":{"next_key":""}}`)
		case "/cosmos/staking/v1beta1/delegators/fixture-wallet/unbonding_delegations":
			_, _ = fmt.Fprint(w, `{"unbonding_responses":[],"pagination":{"next_key":""}}`)
		default:
			t.Errorf("unexpected synthetic node route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	rest, err := servicehttp.New(server.Client(), server.URL)
	require.NoError(t, err)
	counts := 0
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:complete_coverage_counts", func(q *gorm.DB) {
		if q.Statement.Table == "blocks" && strings.Contains(q.Statement.SQL.String(), "count(*)") {
			counts++
		}
	}))
	defer func() { _ = db.Callback().Query().Remove("test:complete_coverage_counts") }()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var before, after string
		require.NoError(t, tx.Raw("SELECT txid_current_snapshot()::text").Scan(&before).Error)
		targets := []heightTime{{Height: 20000000}}
		baseline, baseAnchor, baseErr := (&Server{db: tx, native: DefaultNativeAsset}).rebuildHoldings(apiMainnet, []int64{1}, &ledgerNode{rest: rest}, "fixture-wallet", targets)
		baseQueries := counts
		counts = 0
		candidate, anchor, err := (&Server{db: tx, native: DefaultNativeAsset, readOnlyLedger: true}).rebuildHoldings(apiMainnet, []int64{1}, &ledgerNode{rest: rest}, "fixture-wallet", targets)
		require.NoError(t, baseErr)
		require.NoError(t, err)
		require.Equal(t, baseline, candidate)
		require.Equal(t, baseAnchor, anchor)
		require.Equal(t, int64(20000013), anchor)
		require.Empty(t, candidate[0].Status)
		require.Empty(t, candidate[0].Reason)
		require.NotNil(t, candidate[0].Holdings.Liquid)
		require.Equal(t, "1000000", candidate[0].Holdings.Liquid["uatom"].String())
		require.Equal(t, 1, baseQueries)
		require.Equal(t, 1, counts)
		require.NoError(t, tx.Raw("SELECT txid_current_snapshot()::text").Scan(&after).Error)
		require.Equal(t, before, after)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}))
	require.Equal(t, int32(6), anchorReads.Load())
}
