package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func testValoper(t *testing.T, n byte) string {
	t.Helper()
	data := make([]byte, 20)
	data[0] = n
	v, err := bech32.ConvertAndEncode("cosmos"+apiValoper, data)
	require.NoError(t, err)
	return v
}

// appendTestNode answers the node reads the append makes: the latest
// height, an empty validator set, the slashing parameters and each
// validator's stakers, recording the height every staker read asked for.
type appendTestNode struct {
	mu         sync.Mutex
	delegators map[string][]string
	unbonding  map[string][]string
	heights    map[string]bool
}

func (f *appendTestNode) serve(t *testing.T, latest string) *ledgerNode {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var body any
		switch {
		case p == "/cosmos/base/tendermint/v1beta1/blocks/latest":
			body = map[string]any{"block": map[string]any{"header": map[string]any{"height": latest}}}
		case p == "/cosmos/staking/v1beta1/validators":
			body = map[string]any{"validators": []any{}, "pagination": map[string]any{}}
		case p == "/cosmos/slashing/v1beta1/params":
			body = map[string]any{"params": map[string]any{"slash_fraction_downtime": "0.0001", "slash_fraction_double_sign": "0.05"}}
		case strings.HasSuffix(p, "/unbonding_delegations"):
			v := strings.Split(p, "/")[5]
			var out []any
			for _, a := range f.unbonding[v] {
				out = append(out, map[string]any{"delegator_address": a})
			}
			f.mu.Lock()
			f.heights[r.Header.Get(heightHeader)] = true
			f.mu.Unlock()
			body = map[string]any{"unbonding_responses": out, "pagination": map[string]any{}}
		case strings.HasSuffix(p, "/delegations"):
			v := strings.Split(p, "/")[5]
			var out []any
			for _, a := range f.delegators[v] {
				out = append(out, map[string]any{"delegation": map[string]any{"delegator_address": a}})
			}
			f.mu.Lock()
			f.heights[r.Header.Get(heightHeader)] = true
			f.mu.Unlock()
			body = map[string]any{"delegation_responses": out, "pagination": map[string]any{}}
		default:
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	rest, err := servicehttp.New(http.DefaultClient, srv.URL)
	require.NoError(t, err)
	return &ledgerNode{rest: rest}
}

func appendTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	database := balanceDaysTestDB(t)
	for _, sql := range []string{
		"ALTER TABLE block_event_attributes ADD COLUMN index bigint",
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, time_stamp timestamptz, tx_indexed boolean, block_events_indexed boolean)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint)",
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint)",
		"CREATE TEMP TABLE block_events (id bigint, block_id bigint, block_event_type_id bigint)",
		"CREATE TEMP TABLE block_event_types (id bigint, type text)",
		"CREATE TEMP TABLE block_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE addresses (id bigint, address text)",
		"CREATE TEMP TABLE fees (tx_id bigint, payer_address_id bigint)",
		"CREATE TEMP TABLE tax_fee_grants (tx_id bigint, granter text)",
		"CREATE TEMP TABLE tax_index_holes (chain text, height bigint, chain_txs bigint, addresses_known boolean NOT NULL DEFAULT false)",
		"CREATE TEMP TABLE tax_index_hole_addresses (chain text, height bigint, address text)",
		"CREATE TEMP TABLE tax_validator_consensus (cons_address text PRIMARY KEY, operator text)",
		"CREATE TEMP TABLE tax_validator_rates (chain text, operator text, height bigint, tokens numeric, shares numeric)",
		"INSERT INTO block_event_types VALUES (1, 'slash'), (2, 'transfer')",
		"INSERT INTO block_event_attribute_keys VALUES (1, 'address'), (2, 'reason'), (3, 'recipient')",
		// One indexed block per height, each with one transaction carrying
		// one message event, all keyed by the height.
		"INSERT INTO blocks SELECT h, 1, h, timestamptz '2025-03-01' + h * interval '1 minute', true, true FROM generate_series(100, 130) h",
		"INSERT INTO txes SELECT h, h FROM generate_series(100, 130) h",
		"INSERT INTO messages SELECT h, h FROM generate_series(100, 130) h",
		"INSERT INTO message_events SELECT h, h FROM generate_series(100, 130) h",
		"INSERT INTO block_events SELECT h, h, 2 FROM generate_series(100, 130) h",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	return database
}

func TestChangedRatesPostgres(t *testing.T) {
	database := appendTestDB(t)
	s := &Server{db: database}
	same, moved, gone, added := testValoper(t, 1), testValoper(t, 2), testValoper(t, 3), testValoper(t, 4)
	require.NoError(t, database.Exec(`INSERT INTO tax_validator_rates VALUES
('mainnet', ?, 10, 100, 100), ('mainnet', ?, 20, 300, 300), ('mainnet', ?, 30, 7, 7),
('mainnet', ?, 10, 100, 100), ('mainnet', ?, 20, 100, 100), ('mainnet', ?, 30, 99, 100),
('mainnet', ?, 10, 5, 5),
('mainnet', ?, 30, 5, 5)`, same, same, same, moved, moved, moved, gone, added).Error)

	changed, err := s.changedRates("mainnet", []int64{10, 20, 30})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{moved: true, gone: true, added: true}, changed, "an equal ratio in other numbers is unchanged")
	changed, err = s.changedRates("mainnet", []int64{10, 20})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{gone: true}, changed)

	_, err = s.changedRates("mainnet", []int64{10, 25})
	require.ErrorContains(t, err, "no validator rates are recorded at height 25")
}

func TestAppendWalletsPostgres(t *testing.T) {
	database := appendTestDB(t)
	s := &Server{db: database, native: DefaultNativeAsset}
	a := func(n byte) string { return testAddress(t, n) }
	slashed, moved, steady := testValoper(t, 50), testValoper(t, 51), testValoper(t, 52)
	named := testValoper(t, 53)
	namedAcct, err := OperatorAccount(named)
	require.NoError(t, err)

	// The last stored day ends at block 110, the new day at 120, and the
	// node and the ledger scan reach 125.
	const lo, end = 110, 120
	exec := func(sql string, args ...any) {
		t.Helper()
		require.NoError(t, database.Exec(sql, args...).Error, sql)
	}
	exec("INSERT INTO message_event_attributes VALUES (115, ?), (108, ?), (126, ?), (116, ?), (117, 'cosmos1zzzz'), (117, '1000uatom'), (117, 'cosmos1notbech32')",
		a(1), a(3), a(4), named)
	exec("INSERT INTO block_event_attributes (block_event_id, block_event_attribute_key_id, value) VALUES (109, 3, ?), (125, 3, ?)", a(2), a(14))
	exec("INSERT INTO addresses VALUES (1, ?), (2, ?)", a(5), a(15))
	exec("INSERT INTO fees VALUES (118, 1), (100, 2)")
	exec("INSERT INTO tax_fee_grants VALUES (119, ?)", a(6))
	exec("INSERT INTO tax_index_holes VALUES ('mainnet', 112, 1, true)")
	exec("INSERT INTO tax_index_hole_addresses VALUES ('mainnet', 112, ?), ('mainnet', 140, ?)", a(7), a(16))
	exec(`INSERT INTO tax_validator_rates VALUES ('mainnet', ?, 110, 100, 100), ('mainnet', ?, 120, 99, 100),
('mainnet', ?, 110, 100, 100), ('mainnet', ?, 120, 200, 200), ('mainnet', ?, 110, 1, 1), ('mainnet', ?, 120, 1, 1)`,
		moved, moved, steady, steady, slashed, slashed)
	exec("INSERT INTO tax_validator_consensus VALUES ('cosmosvalcons1known', ?)", slashed)
	exec("INSERT INTO block_events VALUES (1000, 117, 1)")
	exec("INSERT INTO block_event_attributes VALUES (1000, 1, 'cosmosvalcons1known', 0), (1000, 2, 'missing_signature', 1)")

	from, to := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2025, 3, d, 0, 0, 0, 0, time.UTC) }
	require.NoError(t, database.Create([]BalanceDay{
		{Chain: "mainnet", Address: a(11), Day: day(8), Status: balanceIncompleteS, Holdings: "{}"},
		{Chain: "mainnet", Address: a(12), Day: day(5), Status: balanceIncompleteS, Holdings: "{}"},
		{Chain: "mainnet", Address: a(12), Day: day(7), Holdings: `{"liquid":{"uatom":"1"}}`},
		{Chain: "mainnet", Address: a(17), Day: day(2), Holdings: `{"liquid":{"uatom":"1"}}`},
	}).Error)
	build := BalanceBuild{Chain: "mainnet", FromDay: &from, ToDay: &to}

	fake := &appendTestNode{
		delegators: map[string][]string{moved: {a(8)}, steady: {a(10)}, slashed: {a(13)}},
		unbonding:  map[string][]string{moved: {a(9)}},
		heights:    map[string]bool{},
	}
	node := fake.serve(t, "125")
	scan := LedgerScan{Chain: "mainnet", FeeGrantsTo: 125}
	ends := []heightTime{{Height: end}}

	wallets, why, err := s.appendWallets("mainnet", "cosmos", []int64{1}, node, scan, build, lo, ends)
	require.NoError(t, err)
	require.Empty(t, why)
	want := []string{
		a(1),      // named in the window
		a(2),      // named one block before it, where a slash can still reach
		a(5),      // paid a fee
		a(6),      // granted a fee
		a(7),      // named by a known index hole
		a(8),      // delegated to a validator whose rate moved
		a(9),      // unbonding from it
		a(11),     // its last stored day is a status row
		a(13),     // staked with a slashed validator
		a(14),     // named after the window, before the tip
		namedAcct, // an operator address, as its account
	}
	sort.Strings(want)
	require.Equal(t, want, wallets)
	require.Equal(t, map[string]bool{"125": true}, fake.heights, "stakers are read at the tip")
	for _, out := range []string{a(3), a(4), a(10), a(12), a(15), a(16), a(17)} {
		require.NotContains(t, wallets, out)
	}

	t.Run("unknown hole", func(t *testing.T) {
		exec("INSERT INTO tax_index_holes VALUES ('mainnet', 113, 1, false)")
		t.Cleanup(func() { exec("DELETE FROM tax_index_holes WHERE height = 113") })
		_, why, err := s.appendWallets("mainnet", "cosmos", []int64{1}, node, scan, build, lo, ends)
		require.NoError(t, err)
		require.Contains(t, why, "addresses are unknown")
	})
	t.Run("unmapped slash", func(t *testing.T) {
		exec("INSERT INTO block_events VALUES (1001, 118, 1)")
		exec("INSERT INTO block_event_attributes VALUES (1001, 1, 'cosmosvalcons1unknown', 0), (1001, 2, 'missing_signature', 1)")
		t.Cleanup(func() {
			exec("DELETE FROM block_events WHERE id = 1001")
			exec("DELETE FROM block_event_attributes WHERE block_event_id = 1001")
		})
		_, why, err := s.appendWallets("mainnet", "cosmos", []int64{1}, node, scan, build, lo, ends)
		require.NoError(t, err)
		require.Contains(t, why, "cannot be matched to an operator")
	})
	t.Run("double sign", func(t *testing.T) {
		exec("UPDATE block_event_attributes SET value = 'double_sign' WHERE block_event_id = 1000 AND block_event_attribute_key_id = 2")
		t.Cleanup(func() {
			exec("UPDATE block_event_attributes SET value = 'missing_signature' WHERE block_event_id = 1000 AND block_event_attribute_key_id = 2")
		})
		_, why, err := s.appendWallets("mainnet", "cosmos", []int64{1}, node, scan, build, lo, ends)
		require.NoError(t, err)
		require.Contains(t, why, "double sign")
	})
	t.Run("rates missing", func(t *testing.T) {
		_, _, err := s.appendWallets("mainnet", "cosmos", []int64{1}, node, scan, build, lo, []heightTime{{Height: 121}})
		require.ErrorContains(t, err, "no validator rates are recorded at height 121")
	})
}
