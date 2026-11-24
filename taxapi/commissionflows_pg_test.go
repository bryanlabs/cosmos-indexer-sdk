package taxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	flowPGValidator = "cosmosvaloper130mdu9a0etmeuw52qfxk73pn0ga6gawkxsrlwf"
	flowPGReceiver1 = "cosmos1receiverone"
	flowPGReceiver2 = "cosmos1receivertwo"
	flowPGDeposit   = "cosmos1depositlike"
	flowPGJan       = "2025-01"
	flowPGFeb       = "2025-02"
	flowPGIBCDenom  = "ibc/ABC"
)

var (
	flowPGWindowStart = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	flowPGWindowEnd   = time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	flowPGOther       = flowPGValoper(1)
	flowPGTestnet     = flowPGValoper(2)
)

func flowPGValoper(b byte) string {
	v, _ := bech32.ConvertAndEncode(flowValoperHRP, bytes.Repeat([]byte{b}, 20))
	return v
}

// flowPGServer opens a connection with its own throwaway schema, so the
// real migration runs and nothing persists.
func flowPGServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres commission flow verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	database = database.WithContext(ctx)
	schema := fmt.Sprintf("commission_flows_test_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = database.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error
		cancel()
		_ = connection.Close()
	})
	for _, sql := range []string{
		"CREATE SCHEMA " + schema,
		"SET search_path = " + schema,
		"CREATE TABLE chains (id bigint, chain_id text)",
		"CREATE TABLE blocks (id bigint, chain_id bigint, height bigint)",
		"CREATE TABLE txes (id bigint, block_id bigint, hash text, code bigint, memo text)",
		"CREATE TABLE messages (id bigint, tx_id bigint)",
		"CREATE TABLE taxable_events (id bigserial, message_id bigint, sub_index int, category text, amount text, denom text, from_addr text, to_addr text, block_height bigint, timestamp timestamptz, tx_hash text, asset text, validator_address text, reward_trigger text, detail text)",
		"INSERT INTO chains VALUES (1,'cosmoshub-4'),(2,'theta-testnet-001')",
		"INSERT INTO blocks VALUES (1,1,100),(2,1,200),(3,1,300),(4,2,100),(5,1,400)",
		"INSERT INTO txes VALUES (1,1,'A',0,''),(2,2,'B',0,''),(3,3,'C',0,''),(4,4,'D',0,'')",
		"INSERT INTO messages VALUES (1,1),(2,2),(3,3),(4,4)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	events := []string{
		// January: 1 ATOM to the first receiver.
		fmt.Sprintf("(1,'commission','1000000','uatom','','%s','2025-01-10T12:00:00Z','%s')", flowPGReceiver1, flowPGValidator),
		// February: 2 ATOM plus an IBC token to the first, 0.5 ATOM to the second.
		fmt.Sprintf("(2,'commission','2000000','uatom','','%s','2025-02-03T12:00:00Z','%s')", flowPGReceiver1, flowPGValidator),
		fmt.Sprintf("(2,'commission','42','ibc/ABC','','%s','2025-02-03T12:00:00Z','%s')", flowPGReceiver1, flowPGValidator),
		fmt.Sprintf("(3,'commission','500000','uatom','','%s','2025-02-20T12:00:00Z','%s')", flowPGReceiver2, flowPGValidator),
		fmt.Sprintf("(3,'commission','300000','uatom','','%s','2025-02-20T12:00:00Z','%s')", flowPGReceiver2, flowPGOther),
		// The same validator on the testnet, and a testnet-only validator.
		fmt.Sprintf("(4,'commission','9000000','uatom','','%s','2025-02-21T12:00:00Z','%s')", flowPGReceiver1, flowPGValidator),
		fmt.Sprintf("(4,'commission','9000000','uatom','','%s','2025-02-21T12:00:00Z','%s')", flowPGReceiver1, flowPGTestnet),
	}
	require.NoError(t, database.Exec("INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp, validator_address) VALUES "+strings.Join(events, ",")).Error)
	// A deposit-style address: 120 senders, each with its own account memo.
	for _, sql := range []string{
		"INSERT INTO txes SELECT 100+g, 5, 'M'||g, 0, 'acct'||(100000+g) FROM generate_series(1,120) g",
		"INSERT INTO messages SELECT 100+g, 100+g FROM generate_series(1,120) g",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp) SELECT 100+g, 'transfer', '1', 'uatom', 'cosmos1sender'||g, '" + flowPGDeposit + "', '2025-03-01T00:00:00Z' FROM generate_series(1,120) g",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	require.NoError(t, CommissionFlowsMigrate(database))
	require.NoError(t, CommissionFlowsMigrate(database), "the migration is repeatable")

	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"usd":10,"found":true}`))
	}))
	t.Cleanup(prices.Close)
	return &Server{db: database, native: DefaultNativeAsset, oracle: NewOracle(prices.URL, ""), flowSources: map[string][]commissionServingSource{}}
}

// A wallet that pays the native coin out to more than the limit of distinct
// receivers in the trace window is a wide distributor, even when it is a
// validator operator account. Repeat payments, other coins, other categories
// and payments outside the window do not count.
func TestCommissionFlowWideDistributorPostgres(t *testing.T) {
	s := flowPGServer(t)
	const hub = "cosmos1widedistributor"
	for _, sql := range []string{
		"CREATE TABLE message_event_attributes (value text)",
		"CREATE TABLE block_event_attributes (value text)",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', '" + hub + "', 'cosmos1payee'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,30) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', '" + hub + "', 'cosmos1payee'||g, '2025-03-01T00:00:00Z' FROM generate_series(1,30) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', '" + hub + "', 'cosmos1late'||g, '2025-05-01T00:00:00Z' FROM generate_series(1,10) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'ibc/ABC', '" + hub + "', 'cosmos1token'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,10) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'ibc_out', '1', 'uatom', '" + hub + "', 'osmo1ibc'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,10) g",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	n, err := s.fanoutReceivers(hub, flowPGWindowStart, flowPGWindowEnd, 100)
	require.NoError(t, err)
	require.Equal(t, 30, n, "distinct native transfer receivers in the window")
	n, err = s.fanoutReceivers(hub, flowPGWindowStart, flowPGWindowEnd, 10)
	require.NoError(t, err)
	require.Equal(t, 11, n, "the count stops at the limit plus one")

	operator := AddressLabel{Chain: apiMainnet, Address: hub, Name: "Some Validator", Kind: labelValidatorOperator, Source: "derived", CreatedAt: nowUTC()}
	loader := func(limit int) *dbFlowLoader {
		return &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{hub: operator}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true, fanoutLimit: limit}
	}
	narrow := loader(30)
	stop, err := narrow.stop(hub, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{}, stop, "exactly the limit is still traced")
	require.Len(t, narrow.evidence().Labels, 1)

	wide := loader(29)
	stop, err = wide.stop(hub, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowWideDistributor, Detail: flowFanoutHeuristic}, stop, "a validator operator account is not exempt")
	src, err := wide.distributor(hub, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, stop, src)
	stop, err = wide.stop(hub, flowPGWindowEnd, flowPGWindowEnd.AddDate(0, 2, 0))
	require.NoError(t, err)
	require.Equal(t, flowStop{}, stop, "the count is over the trace window only")
	labels := wide.evidence().Labels
	require.Len(t, labels, 2)
	require.Equal(t, labelValidatorOperator, labels[0].Kind, "the operator label is kept")
	require.Equal(t, labelWideDistributor, labels[1].Kind)
	require.Equal(t, flowFanoutHeuristic, labels[1].Source)
	require.Equal(t, "more than 29 distinct receivers of uatom from 2025-01-01 to 2025-04-01", labels[1].Evidence)
	require.Equal(t, flowStop{}, wide.known(hub), "known never reads the index")

	// At the real threshold: a wallet just under it is still traced, one
	// receiver more is a distributor.
	const big = "cosmos1nearlywide"
	require.NoError(t, s.db.Exec("INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', ?, 'cosmos1crowd'||g, '2025-02-01T00:00:00Z' FROM generate_series(1, ?) g", big, flowFanoutMaxReceivers-1).Error)
	full := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
	stop, err = full.stop(big, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{}, stop, "49,999 receivers are still traced")
	require.NoError(t, s.db.Exec("INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '1', 'uatom', ?, 'cosmos1crowdlast', '2025-02-02T00:00:00Z'), ('transfer', '1', 'uatom', ?, 'cosmos1crowd7', '2025-02-02T00:00:00Z')", big, big).Error)
	stop, err = full.stop(big, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{}, stop, "exactly 50,000 receivers are still traced")
	require.NoError(t, s.db.Exec("INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer', '1', 'uatom', ?, 'cosmos1crowdnext', '2025-02-03T00:00:00Z')", big).Error)
	stop, err = full.distributor(big, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowWideDistributor, Detail: flowFanoutHeuristic}, stop, "50,001 receivers make a distributor")

	var stored int64
	require.NoError(t, s.db.Model(&AddressLabel{}).Where("kind = ?", labelWideDistributor).Count(&stored).Error)
	require.Zero(t, stored, "a wide distributor depends on the window and is never stored as a label")
}

func TestCommissionFlowLabelsAndScopePostgres(t *testing.T) {
	s := flowPGServer(t)

	validators, err := s.commissionValidators(apiMainnet)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{flowPGValidator, flowPGOther}, validators, "testnet-only validators are not traced on the Hub")
	first, err := s.commissionFirstWithdrawals(apiMainnet)
	require.NoError(t, err)
	require.ElementsMatch(t, validators, slices.Collect(maps.Keys(first)), "the expected list is the same validators, with their first withdrawal")
	for _, at := range first {
		require.False(t, at.IsZero())
	}
	assertFirstWithdrawalsMatchPlainRead(t, s, first)

	op := operatorAccount(flowPGValidator)
	require.NoError(t, s.db.Create(&AddressLabel{Chain: apiMainnet, Address: op, Name: "Named Exchange", Kind: labelExchange, Source: "manual", CreatedAt: nowUTC()}).Error)
	require.NoError(t, s.seedAddressLabels(apiMainnet, validators))
	require.NoError(t, s.seedAddressLabels(apiMainnet, validators), "seeding twice is a no-op")
	var kinds []struct {
		Kind string
		N    int
	}
	require.NoError(t, s.db.Table(AddressLabel{}.TableName()).Select("kind, COUNT(*) AS n").Group("kind").Order("kind").Scan(&kinds).Error)
	require.Equal(t, []struct {
		Kind string
		N    int
	}{{labelExchange, 1}, {labelIBCEscrow, flowEscrowChannels}, {labelModule, len(hubModuleNames)}, {labelValidatorOperator, 1}}, kinds)
	var kept AddressLabel
	require.NoError(t, s.db.Where("chain = ? AND address = ?", apiMainnet, op).First(&kept).Error)
	require.Equal(t, "Named Exchange", kept.Name, "a seeded label never replaces one from another source")

	var labels []AddressLabel
	require.NoError(t, s.db.Where("chain = ?", apiMainnet).Find(&labels).Error)
	loader := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
	for _, l := range labels {
		loader.labels[l.Address] = l
	}
	stop, err := loader.stop(moduleAddress("distribution"), flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowCommunityPool, stop.Class)
	stop, err = loader.stop(ics20EscrowAddress("transfer", "channel-141"), flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowIBCOut, Detail: "transfer/channel-141"}, stop)
	stop, err = loader.stop(op, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchange, Detail: "Named Exchange"}, stop)
	stop, err = loader.stop(flowPGDeposit, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowMemoHeuristic}, stop)
	// Workers share one loader: lookups run while the memo heuristic stores
	// a new label. Run under -race.
	shared := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
	for _, l := range labels {
		shared.labels[l.Address] = l
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				got, err := shared.stop(op, flowPGWindowStart, flowPGWindowEnd)
				if err == nil && got.Class != flowExchange {
					err = fmt.Errorf("labelled address stopped as %s", got.Class)
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
		go func() {
			defer wg.Done()
			got, err := shared.stop(flowPGDeposit, flowPGWindowStart, flowPGWindowEnd)
			if err == nil && got.Class != flowExchangeLike {
				err = fmt.Errorf("deposit address stopped as %s", got.Class)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var inferred AddressLabel
	require.NoError(t, s.db.Where("chain = ? AND address = ?", apiMainnet, flowPGDeposit).First(&inferred).Error)
	require.Equal(t, labelExchangeLike, inferred.Kind, "the inferred label is stored with its evidence")
	require.Contains(t, inferred.Evidence, "120 distinct senders")

	receivers, err := s.commissionReceivers(apiMainnet, flowPGValidator)
	require.NoError(t, err)
	require.Len(t, receivers, 2)
	require.Equal(t, flowPGReceiver1, receivers[0].Address)
	require.Equal(t, "3000000", receivers[0].AmountBase, "the IBC token and the testnet payment are not Hub ATOM")
	require.Equal(t, 3, receivers[0].Withdrawals)
	require.Equal(t, "500000", receivers[1].AmountBase)

	untracked, err := s.commissionUntracked(apiMainnet, flowPGValidator, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, map[string]map[string]string{flowPGFeb: {flowPGIBCDenom: "42"}}, untracked)
}

func flowPGRun(validator, status, commission string) CommissionFlowRun {
	return CommissionFlowRun{
		Chain: apiMainnet, Validator: validator, FromDay: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), AsOf: time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC),
		Status: status, Receivers: 2, Wallets: 3, Overdraw: `{"fifo":"0","pro_rata":"0"}`, DriftBase: "12", Commission: commission, Untracked: `{"2025-02":{"ibc/ABC":"42"}}`, ComputedAt: nowUTC(),
	}
}

func flowPGRows(validator string, lines ...[3]string) []CommissionFlow {
	var out []CommissionFlow
	for _, rule := range flowRuleNames {
		for _, l := range lines {
			out = append(out, CommissionFlow{Chain: apiMainnet, Validator: validator, Rule: rule, Month: l[0], Class: l[1], AmountBase: l[2]})
		}
	}
	return out
}

func flowPGGet(t *testing.T, h http.HandlerFunc, target string, status int, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	require.Equal(t, status, rec.Code, rec.Body.String())
	if out != nil {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
	}
	return rec
}

func TestCommissionFlowsSaveAndServePostgres(t *testing.T) {
	s := flowPGServer(t)
	commission := `{"2025-01":"1000000","2025-02":"2500000"}`

	// A first result, then a replacement with one class fewer.
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowPGValidator, flowStatusReconciled, commission), flowPGRows(flowPGValidator,
		[3]string{flowPGJan, flowExchange, "1000000"}, [3]string{flowPGFeb, flowHeld, "2000000"}, [3]string{flowPGFeb, flowFees, "500000"})))
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowPGValidator, flowStatusReconciled, commission), flowPGRows(flowPGValidator,
		[3]string{flowPGJan, flowExchange, "1000000"}, [3]string{flowPGFeb, flowHeld, "2500000"})))
	var n int64
	require.NoError(t, s.db.Model(&CommissionFlow{}).Where("validator = ?", flowPGValidator).Count(&n).Error)
	require.Equal(t, int64(2*flowRules), n, "a new result replaces every stored line")

	var got ValidatorCommissionFlows
	h := s.handleValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?chain=cosmoshub&validator="+flowPGValidator, http.StatusOK, &got)
	require.Equal(t, flowStatusReconciled, got.Status)
	require.Equal(t, "2023-01-01", got.From)
	require.Len(t, got.Months, 2)
	require.Equal(t, "2.5", got.Months[1].Withdrawn.String())
	require.Equal(t, "25", got.Months[1].USD.String(), "withdrawn ATOM at the month's commission-weighted price")
	require.Equal(t, map[string]string{flowPGIBCDenom: "42"}, got.Months[1].Untracked)
	require.Equal(t, "1", got.Months[1].Flows[0].Share.String())
	require.Equal(t, "3.5", got.Total.Withdrawn.String())
	require.Len(t, got.Receivers, 2)
	require.Equal(t, "3", got.Receivers[0].Amount)
	require.Equal(t, "0.000012", got.Reconciliation["drift"])

	flowPGGet(t, h, "/validator-commission-flows?validator=cosmos1notavaloper", http.StatusBadRequest, nil)
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGTestnet, http.StatusNotFound, nil)
	var feb ValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGValidator+"&start=2025-02&end=2025-02", http.StatusOK, &feb)
	require.Len(t, feb.Months, 1)
	require.Equal(t, "2.5", feb.Total.Withdrawn.String())
	rec := flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGValidator+"&format=csv", http.StatusOK, nil)
	require.Equal(t, "text/csv", rec.Header().Get(apiContentType))
	require.Contains(t, rec.Body.String(), flowPGFeb)

	// Another validator traced, then failed on a later run: its own page keeps
	// the last good flows and says it failed; the Hub leaves it out.
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowPGOther, flowStatusReconciled, `{"2025-02":"300000"}`), flowPGRows(flowPGOther,
		[3]string{flowPGFeb, flowExchange, "300000"})))
	var hub CommissionFlowsHub
	chain := chainParam("cosmoshub")

	// Until the list of validators that ever withdrew commission is known
	// the Hub has no true coverage to give, so it asks the caller to retry
	// instead of stating 0 of 0. An empty list is not a known list either.
	s.flowExp = map[string]*flowExpected{chain: {loading: true}}
	rec = flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusServiceUnavailable, nil)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	require.NotContains(t, rec.Body.String(), `"expected"`)
	flowPGExpect(s, chain)
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusServiceUnavailable, nil)
	// While any of them has no run, the Hub shows coverage, not totals.
	flowPGExpect(s, chain, flowPGValidator, flowPGOther, "cosmosvaloper1untraced")
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.False(t, hub.Complete)
	require.Equal(t, 2, hub.Traced)
	require.Equal(t, 3, hub.Expected)
	require.Empty(t, hub.Months, "two of three validators is not a Hub figure")

	// The daily trace stores a run for every validator that ever withdrew
	// commission, including one with none since the trace start, so a full
	// run completes the set.
	require.NoError(t, s.saveCommissionFlows(flowPGRun("cosmosvaloper1untraced", flowStatusOK, "{}"), nil))
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.True(t, hub.Complete, "a run with no commission in the window still counts as traced")
	require.Equal(t, 3, hub.Traced)
	require.Equal(t, "2.8", hub.Months[1].Withdrawn.String())
	require.NoError(t, s.db.Where("validator = ?", "cosmosvaloper1untraced").Delete(&CommissionFlowRun{}).Error)

	// A validator that first withdrew after the served as_of is not expected
	// of a trace that ends before it.
	flowPGExpect(s, chain, flowPGValidator, flowPGOther)
	s.flowExp[chain].first["cosmosvaloper1later"] = flowPGWindowEnd.AddDate(1, 0, 0)
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.True(t, hub.Complete)
	require.Equal(t, 2, hub.Traced)
	require.Equal(t, 2, hub.Expected)
	require.Equal(t, "2.8", hub.Months[1].Withdrawn.String())
	require.Equal(t, 0, hub.Unreconciled.Validators)

	// A run whose rebuilt balances do not match the chain is left out of both
	// sides and counted as left out, with the commission it withdrew.
	drift := flowPGRun(flowPGOther, flowStatusDrift, `{"2025-02":"300000"}`)
	require.NoError(t, s.saveCommissionFlows(drift, flowPGRows(flowPGOther, [3]string{flowPGFeb, flowExchange, "300000"})))
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.True(t, hub.Complete)
	require.Equal(t, "2.5", hub.Months[1].Withdrawn.String())
	require.Equal(t, "0.3", hub.Unreconciled.Withdrawn.String())
	require.Equal(t, 1, hub.Unreconciled.Validators)
	for _, l := range hub.Months[1].Flows {
		require.NotEqual(t, flowExchange, l.Class, "only the drifted validator sent February commission to an exchange")
	}

	failed := flowPGRun(flowPGOther, flowStatusError, "{}")
	failed.Reason = "node unreachable"
	require.NoError(t, s.saveCommissionFlows(failed, nil))
	var other ValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGOther, http.StatusOK, &other)
	require.Equal(t, flowStatusError, other.Status)
	require.Equal(t, "node unreachable", other.Reason)
	require.Equal(t, "0.3", other.Total.Withdrawn.String(), "a failed run keeps the last good result")
	require.Len(t, other.Total.Flows, 1)

	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows", http.StatusOK, &hub)
	require.Equal(t, 2, hub.Validators)
	require.Equal(t, map[string]int{flowStatusReconciled: 1, flowStatusError: 1}, hub.Statuses)
	require.Equal(t, "2025-03-31", hub.AsOf)
	require.Equal(t, "2.5", hub.Months[1].Withdrawn.String())
	shares := hub.Months[1].Flows[0].Share
	for _, l := range hub.Months[1].Flows[1:] {
		shares = shares.Add(l.Share)
	}
	require.Equal(t, "1", shares.String(), "the failed validator's flows are not summed against commission it left out")
	for _, l := range hub.Months[1].Flows {
		require.NotEqual(t, flowExchange, l.Class, "only the failed validator sent February commission to an exchange")
	}
}

func flowPGExpect(s *Server, chain string, validators ...string) {
	first := map[string]time.Time{}
	for _, v := range validators {
		first[v] = flowPGWindowStart
	}
	s.flowExp = map[string]*flowExpected{chain: {first: first, at: time.Now()}}
}

// A returning one-hop ATOM row counts only when its message credited the
// wallet that exact native amount. Failed receives (an error acknowledgement
// emits its coin events under another type) and credits to another account
// do not.
func TestFlowReturnCreditsPostgres(t *testing.T) {
	s := flowPGServer(t)
	holder := flowPGReceiver1
	for _, sql := range []string{
		"CREATE TABLE message_event_types (id bigint, type text)",
		"CREATE TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index bigint)",
		"CREATE TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TABLE message_event_attributes (id bigint, message_event_id bigint, message_event_attribute_key_id bigint, value text, index bigint)",
		"INSERT INTO message_event_types VALUES (1,'coin_received'),(2,'ibccallbackerror-coin_received')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'receiver'),(2,'amount')",
		// Message 501: credited. 502: error ack. 503: credited to someone else.
		// 504: credited in a list of coins.
		"INSERT INTO message_events VALUES (1,501,1,0),(2,502,2,0),(3,503,1,0),(4,504,1,0)",
		"INSERT INTO message_event_attributes VALUES " +
			"(1,1,1,'" + holder + "',0),(2,1,2,'26560000uatom',1)," +
			"(3,2,1,'" + holder + "',0),(4,2,2,'1000000uatom',1)," +
			"(5,3,1,'" + flowPGReceiver2 + "',0),(6,3,2,'7000000uatom',1)," +
			"(7,4,1,'" + holder + "',0),(8,4,2,'5ibc/ABC,9000000uatom',1)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	row := func(msg uint, amount string) tax.TaxableEvent {
		return tax.TaxableEvent{MessageID: msg, Category: string(tax.CategoryIBCIn), Amount: amount, Denom: "transfer/channel-1/uatom", ToAddr: holder}
	}
	rows := []tax.TaxableEvent{row(501, "26560000"), row(502, "1000000"), row(503, "7000000"), row(504, "9000000")}
	got, err := s.flowReturnCredits(holder, rows)
	require.NoError(t, err)
	var kept []uint
	for _, e := range rows {
		if got[flowCredit{e.MessageID, e.Amount + "uatom"}] {
			kept = append(kept, e.MessageID)
		}
	}
	require.Equal(t, []uint{501, 504}, kept)
}

// The payout read against real tables: a merged 2021-style transfer event
// pays the wallet a 2 uatom reward beside 6198375 uatom of principal, and a
// newer message pays the wallet as another delegator's withdraw address.
func TestFlowRewardPayoutsPostgres(t *testing.T) {
	s := flowPGServer(t)
	dist := moduleAddress("distribution")
	for _, sql := range []string{
		"ALTER TABLE blocks ADD COLUMN time_stamp timestamptz",
		"UPDATE blocks SET time_stamp = '2025-01-01T00:00:00Z'::timestamptz + (height || ' minutes')::interval",
		"CREATE TABLE message_event_types (id bigint, type text)",
		"CREATE TABLE message_events (id bigint, message_id bigint, index bigint, message_event_type_id bigint)",
		"CREATE TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TABLE message_event_attributes (id bigserial, message_event_id bigint, value text, index bigint, message_event_attribute_key_id bigint)",
		"INSERT INTO message_event_types VALUES (1,'transfer'),(2,'coin_received')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'recipient'),(2,'sender'),(3,'amount'),(4,'receiver')",
		"INSERT INTO message_events VALUES (10,1,1,1),(11,2,0,2),(12,2,3,1),(13,3,0,1)",
		"INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES " +
			"(10,'" + flowPGReceiver1 + "',0,1),(10,'" + dist + "',1,2),(10,'2uatom',2,3)," +
			"(10,'" + flowPGReceiver1 + "',3,1),(10,'cosmos1notbonded',4,2),(10,'6198375uatom',5,3)," +
			"(11,'" + flowPGReceiver1 + "',0,4),(11,'50uatom',1,3)," +
			"(12,'" + flowPGReceiver1 + "',0,1),(12,'" + dist + "',1,2),(12,'50uatom,7ibc/ABC',2,3)," +
			"(13,'" + flowPGReceiver2 + "',0,1),(13,'" + dist + "',1,2),(13,'9uatom',2,3)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	start, end := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	paid, err := s.flowRewardPayouts(apiMainnet, flowPGReceiver1, start, end)
	require.NoError(t, err)
	require.Len(t, paid, 2)
	require.Equal(t, "2", paid[1].Amount.String())
	require.Equal(t, "50", paid[2].Amount.String())
	require.Equal(t, int64(200), paid[2].Key.Height)
	// Message 2 has no reward row to the wallet: it becomes a reward inflow.
	got := reconcileRewardPayouts(flowPGReceiver1, nil, paid)
	require.Len(t, got, 2)
	none, err := s.flowRewardPayouts(apiMainnet, flowPGReceiver1, end, end.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Empty(t, none)
}

// The share-record read against real tables: a redeem by another wallet
// settles the receiver's record (the record's account withdraws 40 uatom and
// pays it on), beside an ordinary 50 uatom distribution payout and a plain
// send from the same record account with no settlement, which stays out.
func TestFlowShareRecordPayoutsPostgres(t *testing.T) {
	s := flowPGServer(t)
	dist := moduleAddress("distribution")
	const record = "cosmos1record"
	for _, sql := range []string{
		"ALTER TABLE blocks ADD COLUMN time_stamp timestamptz",
		"INSERT INTO blocks VALUES (6,1,250),(7,1,260)",
		"INSERT INTO txes VALUES (5,6,'E',0,''),(6,7,'F',0,'')",
		"INSERT INTO messages VALUES (5,5),(6,6)",
		"UPDATE blocks SET time_stamp = '2025-01-01T00:00:00Z'::timestamptz + (height || ' minutes')::interval",
		"CREATE TABLE message_event_types (id bigint, type text)",
		"CREATE TABLE message_events (id bigint, message_id bigint, index bigint, message_event_type_id bigint)",
		"CREATE TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TABLE message_event_attributes (id bigserial, message_event_id bigint, value text, index bigint, message_event_attribute_key_id bigint)",
		"INSERT INTO message_event_types VALUES (1,'transfer'),(18,'withdraw_rewards'),(1245,'withdraw_tokenize_share_reward')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'recipient'),(2,'sender'),(3,'amount'),(4,'delegator'),(5,'withdraw_address'),(6,'validator')",
		"INSERT INTO message_events VALUES (12,2,3,1),(20,5,3,1),(21,5,5,18),(22,5,12,1),(23,5,14,1245),(30,6,0,18),(31,6,1,1)",
		"INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES " +
			"(12,'" + flowPGReceiver1 + "',0,1),(12,'" + dist + "',1,2),(12,'50uatom',2,3)," +
			"(20,'" + record + "',0,1),(20,'" + dist + "',1,2),(20,'40uatom',2,3)," +
			"(21,'40uatom',0,3),(21,'" + flowPGValidator + "',1,6),(21,'" + record + "',2,4)," +
			"(22,'" + flowPGReceiver1 + "',0,1),(22,'" + record + "',1,2),(22,'7ibc/ABC,40uatom',2,3)," +
			"(23,'" + flowPGReceiver1 + "',0,5),(23,'7ibc/ABC,40uatom',1,3)," +
			"(30,'70uatom',0,3),(30,'" + record + "',1,4)," +
			"(31,'" + flowPGReceiver1 + "',0,1),(31,'" + record + "',1,2),(31,'70uatom',2,3)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	start, end := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	paid, err := s.flowRewardPayouts(apiMainnet, flowPGReceiver1, start, end)
	require.NoError(t, err)
	require.Len(t, paid, 2)
	require.Equal(t, "50", paid[2].Amount.String())
	require.Equal(t, "40", paid[5].Amount.String())
	require.Equal(t, int64(250), paid[5].Key.Height)
	_, sent := paid[6]
	require.False(t, sent)
}

// Withdraw-address moves are not payments to a receiver: they add nobody to a
// wallet's fan-out and no sender to a deposit-style count.
func TestCommissionFlowWithdrawAddressMovesPostgres(t *testing.T) {
	s := flowPGServer(t)
	const delegator = "cosmos1delegatorwide"
	for _, sql := range []string{
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer', '1', 'uatom', '" + delegator + "', 'cosmos1payee'||g, '2025-02-01T00:00:00Z' FROM generate_series(1,5) g",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp, reward_trigger) VALUES ('transfer', '7', 'uatom', '" + delegator + "', 'cosmos1withdrawaddr', '2025-02-02T00:00:00Z', '" + tax.TriggerWithdrawAddress + "')",
		"INSERT INTO txes VALUES (900, 5, 'W', 0, 'acct900001')",
		"INSERT INTO messages VALUES (900, 900)",
		"INSERT INTO taxable_events (message_id, category, amount, denom, from_addr, to_addr, timestamp, reward_trigger) VALUES (900, 'transfer', '7', 'uatom', 'cosmos1anotherdelegator', '" + flowPGDeposit + "', '2025-03-01T00:00:00Z', '" + tax.TriggerWithdrawAddress + "')",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	n, err := s.fanoutReceivers(delegator, flowPGWindowStart, flowPGWindowEnd, 100)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	senders, err := s.memoDepositSenders(flowPGDeposit)
	require.NoError(t, err)
	require.Equal(t, 120, senders)
}

// assertFirstWithdrawalsMatchPlainRead checks the warm-up's planner settings
// change only how Postgres reads the rows: the same query without them gives
// the same first withdrawal for every validator, and the settings end with the
// warm-up's transaction.
func assertFirstWithdrawalsMatchPlainRead(t *testing.T, s *Server, first map[string]time.Time) {
	t.Helper()
	var plain []struct {
		Validator string
		FirstAt   time.Time
	}
	require.NoError(t, s.scopedTaxEvents(apiMainnet).
		Select("taxable_events.validator_address AS validator, MIN(taxable_events.timestamp) AS first_at").
		Where("taxable_events.category = ? AND taxable_events.validator_address <> ''", apiCommission).
		Group("taxable_events.validator_address").Scan(&plain).Error)
	require.Len(t, first, len(plain))
	for _, r := range plain {
		got, ok := first[r.Validator]
		require.True(t, ok, r.Validator)
		require.True(t, got.Equal(r.FirstAt), "%s: %s against %s", r.Validator, got, r.FirstAt)
	}

	// flowPGServer holds one connection, so the SETs and SHOWs below run on
	// the connection the warm-up used.
	require.NoError(t, s.db.Exec("SET enable_hashjoin = on").Error)
	require.NoError(t, s.db.Exec("SET work_mem = '4MB'").Error)
	_, err := s.commissionFirstWithdrawals(apiMainnet)
	require.NoError(t, err)
	var hashjoin, workMem string
	require.NoError(t, s.db.Raw("SHOW enable_hashjoin").Scan(&hashjoin).Error)
	require.NoError(t, s.db.Raw("SHOW work_mem").Scan(&workMem).Error)
	require.Equal(t, "on", hashjoin, "the warm-up's planner setting does not outlive its transaction")
	require.Equal(t, "4MB", workMem, "the warm-up's work_mem does not outlive its transaction")
}
