package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const fakeChain = "testhub-1"

var fakeDay0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeChainNode serves six blocks a day from height 1000 (2026-01-01) to
// 1026 (2026-01-05). A proposal passing at 1014 raises the community tax
// from 0.02 to 0.10 in that block's end block.
type fakeChainNode struct {
	missing   map[int64]bool
	taxChange int64 // height whose state first holds 0.10
	proposal  int64 // height with the passed proposal (0 = none)
}

func (f *fakeChainNode) blockTime(h int64) time.Time {
	return fakeDay0.Add(5*time.Second + time.Duration(h-1000)*4*time.Hour)
}

func fakeMint(h int64) int64 { return 1000 + h }

func (f *fakeChainNode) blockResults(h int64) string {
	ev := func(typ, mode string, kv ...string) string {
		var a []string
		for i := 0; i < len(kv); i += 2 {
			a = append(a, fmt.Sprintf(`{"key":%q,"value":%q,"index":true}`, kv[i], kv[i+1]))
		}
		a = append(a, fmt.Sprintf(`{"key":"mode","value":%q}`, mode))
		return fmt.Sprintf(`{"type":%q,"attributes":[%s]}`, typ, strings.Join(a, ","))
	}
	evs := []string{
		ev("mint", "BeginBlock", "bonded_ratio", "0.6"+strconv.FormatInt(h%7, 10), "inflation", "0.1"+strconv.FormatInt(h%5, 10),
			"annual_provisions", strconv.FormatInt(365*fakeMint(h), 10)+".5", "amount", strconv.FormatInt(fakeMint(h), 10)),
		ev("commission", "BeginBlock", "amount", "0.1uatom", "validator", "valA"),
		ev("rewards", "BeginBlock", "amount", strconv.FormatInt(h%3, 10)+".5uatom", "validator", "valA"),
		ev("rewards", "BeginBlock", "amount", "1.25uatom", "validator", "valB"),
	}
	if h == f.proposal {
		evs = append(evs, ev("active_proposal", "EndBlock", "proposal_id", "77", "proposal_result", "proposal_passed"))
	}
	txs := "[]"
	if h == 1007 {
		txs = `[{"code":0,"events":[` + ev("withdraw_rewards", "", "amount", "5uatom") + `,` +
			ev("unbond", "", "amount", "10uatom", "completion_time", "2026-01-22T03:04:05Z") + `]}]`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":-1,"result":{"height":"%d","txs_results":%s,"finalize_block_events":[%s]}}`, h, txs, strings.Join(evs, ","))
}

func (f *fakeChainNode) taxAt(h int64) string {
	if f.taxChange > 0 && h >= f.taxChange {
		return "100000000000000000"
	}
	return "20000000000000000"
}

func (f *fakeChainNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const lo, hi = 1000, 1026
	h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
	unavailable := func() {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height %d is not available, lowest height is %d"}}`, h, lo)
	}
	switch r.URL.Path {
	case "/status":
		fmt.Fprintf(w, `{"result":{"node_info":{"network":%q},"sync_info":{"earliest_block_height":"%d","latest_block_height":"%d","latest_block_time":%q,"catching_up":false}}}`,
			fakeChain, lo, hi, f.blockTime(hi).Format(time.RFC3339Nano))
	case "/header":
		if h < lo || h > hi {
			unavailable()
			return
		}
		fmt.Fprintf(w, `{"result":{"header":{"height":"%d","time":%q}}}`, h, f.blockTime(h).Format(time.RFC3339Nano))
	case "/block_results":
		if h < lo || h > hi || f.missing[h] {
			unavailable()
			return
		}
		fmt.Fprint(w, f.blockResults(h))
	case "/abci_query":
		if r.URL.Query().Get("path") != `"/cosmos.distribution.v1beta1.Query/Params"` || h < lo || h > hi {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		tax := f.taxAt(h)
		inner := append([]byte{0x0a, byte(len(tax))}, tax...)
		msg := append([]byte{0x0a, byte(len(inner))}, inner...)
		fmt.Fprintf(w, `{"result":{"response":{"code":0,"value":%q,"height":"%d"}}}`, base64.StdEncoding.EncodeToString(msg), h)
	default:
		http.NotFound(w, r)
	}
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN to a throwaway Postgres")
	}
	schema := fmt.Sprintf("staking_cmd_%d", time.Now().UnixNano())
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, boot.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		boot.Exec("DROP SCHEMA " + schema + " CASCADE")
		if c, err := boot.DB(); err == nil {
			_ = c.Close()
		}
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	require.NoError(t, db.Exec(taxapi.StakingTablesDDL).Error)
	require.NoError(t, db.Exec(`INSERT INTO tax_staking_state VALUES (?, '2026-01-01', 1000, ?, '2026-01-01', 1005, ?, 'seed', ?)`,
		fakeChain, fakeDay0.Add(5*time.Second), time.Now().UTC(), taxapi.StakingTablesVersion).Error)
	return db
}

// expectDay works a day out with plain rationals, independently of buildDay.
func expectDay(f *fakeChainNode, first, last int64) map[string]string {
	minted, pool := new(big.Rat), new(big.Rat)
	rewards, commission := new(big.Rat), new(big.Rat)
	for h := first; h <= last; h++ {
		m := new(big.Rat).SetInt64(fakeMint(h))
		minted.Add(minted, m)
		tax := "0.02"
		if f.taxAt(h-1) != "20000000000000000" {
			tax = "0.1"
		}
		pool.Add(pool, new(big.Rat).Mul(m, rat(tax)))
		rewards.Add(rewards, rat(strconv.FormatInt(h%3, 10)+".5"))
		rewards.Add(rewards, rat("1.25"))
		commission.Add(commission, rat("0.1"))
	}
	toStakers := new(big.Rat).Sub(minted, pool)
	com := new(big.Rat).Mul(toStakers, new(big.Rat).Quo(commission, rewards))
	return map[string]string{
		"minted":     minted.FloatString(0),
		"pool":       roundHalfEven(pool).String(),
		"stakers":    new(big.Int).Sub(minted.Num(), roundHalfEven(pool)).String(),
		"commission": roundHalfEven(com).String(),
		"supply":     roundHalfEven(new(big.Rat).Quo(rat(strconv.FormatInt(365*fakeMint(first), 10)+".5"), rat("0.1"+strconv.FormatInt(first%5, 10)))).String(),
	}
}

func TestRefreshAddsWholeDays(t *testing.T) {
	db := testDB(t)
	f := &fakeChainNode{taxChange: 1014, proposal: 1014}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	now := time.Date(2026, 1, 5, 0, 20, 0, 0, time.UTC)

	st, added, err := refreshDays(ctx, db, newNode(srv.URL, 3), fakeChain, 3, now)
	require.NoError(t, err)
	require.Equal(t, 3, added)
	require.Equal(t, "2026-01-04", st.LastDay)
	require.Equal(t, int64(1023), st.LastHeight)
	require.Equal(t, 0, lagDays(st.LastDay, now))

	days, err := taxapi.ReadStakingDays(db, fakeChain, "2026-01-02", "2026-01-04")
	require.NoError(t, err)
	require.Len(t, days, 3)
	for i, d := range days {
		first := int64(1006 + 6*i)
		require.Equal(t, first, d.FirstHeight)
		require.Equal(t, first+5, d.LastHeight)
		require.Equal(t, int64(6), d.Blocks)
		e := expectDay(f, first, first+5)
		require.Equal(t, e["minted"], d.Minted, d.Day)
		require.Equal(t, e["pool"], d.CommunityPool, d.Day)
		require.Equal(t, e["stakers"], d.Stakers, d.Day)
		require.Equal(t, e["commission"], d.Commission, d.Day)
		require.Equal(t, e["supply"], d.SupplyStart, d.Day)
	}
	require.Equal(t, []string{"0.02", "0.02", "0.10"}, []string{days[0].CommunityTax, days[1].CommunityTax, days[2].CommunityTax})
	require.Equal(t, "[]", days[0].Proposals)
	require.JSONEq(t, `[[1014,"77","proposal_passed"]]`, days[1].Proposals)
	require.Equal(t, "5", days[0].WithdrawnRewards)
	require.Equal(t, "0", days[0].WithdrawnCommission)
	// 2026-01-02 is heights 1006..1011: inflation 0.11 0.12 0.13 0.14 0.10 0.11, bonded 0.6(h%7).
	require.Equal(t, "0.11", days[0].InflationStart)
	require.Equal(t, "0.11833333", days[0].InflationAvg)
	require.Equal(t, "0.62833333", days[0].BondedAvg)
	require.Equal(t, "16", days[0].RewardsEvents)
	require.Equal(t, "0", days[0].CommissionEvents)
	vals, err := taxapi.ReadStakingValidatorDays(db, fakeChain, "2026-01-02", "2026-01-02")
	require.NoError(t, err)
	require.Equal(t, []taxapi.StakingValidatorRow{
		{Day: "2026-01-02", Operator: "valA", Rewards: "9", Commission: "0.6"},
		{Day: "2026-01-02", Operator: "valB", Rewards: "7.5", Commission: "0"},
	}, vals)
	var unb string
	require.NoError(t, db.Raw(`SELECT completion_day::text || ' ' || amount_uatom::text FROM tax_staking_unbonding WHERE chain = ?`, fakeChain).Row().Scan(&unb))
	require.Equal(t, "2026-01-22 10", unb)

	// Rerun: nothing to add, nothing changes.
	st2, added, err := refreshDays(ctx, db, newNode(srv.URL, 3), fakeChain, 3, now)
	require.NoError(t, err)
	require.Equal(t, 0, added)
	require.Equal(t, st.LastHeight, st2.LastHeight)

	// The check recomputes the same days and finds no difference.
	n := newNode(srv.URL, 3)
	ranges, err := nodeDays(ctx, n, fakeChain, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	cs, err := computeDays(ctx, n, ranges, 2, true)
	require.NoError(t, err)
	td, err := readTableDays(db, fakeChain, "2026-01-02", "2026-01-04")
	require.NoError(t, err)
	for i, d := range td.days {
		require.Empty(t, compareDay(d, td.vals[d.Day], td.unbond[d.Day], cs[i]), d.Day)
	}
	// A changed cell is reported.
	require.NoError(t, db.Exec(`UPDATE tax_staking_days SET commission_uatom = commission_uatom + 1 WHERE chain = ? AND day = '2026-01-03'`, fakeChain).Error)
	td, err = readTableDays(db, fakeChain, "2026-01-03", "2026-01-03")
	require.NoError(t, err)
	require.Len(t, compareDay(td.days[0], td.vals["2026-01-03"], td.unbond["2026-01-03"], cs[1]), 1)
}

func TestRefreshRefusesMissingBlock(t *testing.T) {
	db := testDB(t)
	f := &fakeChainNode{missing: map[int64]bool{1013: true}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	now := time.Date(2026, 1, 5, 0, 20, 0, 0, time.UTC)
	st, added, err := refreshDays(context.Background(), db, newNode(srv.URL, 2), fakeChain, 2, now)
	require.ErrorContains(t, err, "day=2026-01-03")
	require.ErrorContains(t, err, "missing=1 first=1013")
	require.Equal(t, 1, added, "the whole day before the gap is kept")
	require.Equal(t, "2026-01-02", st.LastDay)
	require.Equal(t, 2, lagDays(st.LastDay, now))
	days, err := taxapi.ReadStakingDays(db, fakeChain, "2026-01-01", "2026-01-31")
	require.NoError(t, err)
	require.Len(t, days, 1)

	// Once the node serves the block, a rerun completes from where it stopped.
	f.missing = nil
	st, added, err = refreshDays(context.Background(), db, newNode(srv.URL, 2), fakeChain, 2, now)
	require.NoError(t, err)
	require.Equal(t, 2, added)
	require.Equal(t, "2026-01-04", st.LastDay)
}

func TestRefreshRefusesTaxChangeWithoutProposal(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(&fakeChainNode{taxChange: 1014})
	defer srv.Close()
	_, added, err := refreshDays(context.Background(), db, newNode(srv.URL, 2), fakeChain, 2, time.Date(2026, 1, 5, 0, 20, 0, 0, time.UTC))
	require.ErrorContains(t, err, "community tax changed in block 1014 without a passed proposal")
	require.Equal(t, 1, added)
}

func TestRefreshOnlyWholeDays(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(&fakeChainNode{})
	defer srv.Close()
	// At 2026-01-03 00:20 only 2026-01-02 is whole.
	st, added, err := refreshDays(context.Background(), db, newNode(srv.URL, 2), fakeChain, 2, time.Date(2026, 1, 3, 0, 20, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, added)
	require.Equal(t, "2026-01-02", st.LastDay)
	// Later than the node's last block: days it cannot close yet are left for later.
	st, added, err = refreshDays(context.Background(), db, newNode(srv.URL, 2), fakeChain, 2, time.Date(2026, 1, 9, 0, 20, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 2, added)
	require.Equal(t, "2026-01-04", st.LastDay)
	require.Equal(t, 4, lagDays(st.LastDay, time.Date(2026, 1, 9, 0, 20, 0, 0, time.UTC)))
}
