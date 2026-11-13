package taxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	flowPGOther   = flowPGValoper(1)
	flowPGTestnet = flowPGValoper(2)
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
	return &Server{db: database, native: DefaultNativeAsset, oracle: NewOracle(prices.URL, "")}
}

func TestCommissionFlowLabelsAndScopePostgres(t *testing.T) {
	s := flowPGServer(t)

	validators, err := s.commissionValidators(apiMainnet)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{flowPGValidator, flowPGOther}, validators, "testnet-only validators are not traced on the Hub")

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
	stop, err := loader.stop(moduleAddress("distribution"))
	require.NoError(t, err)
	require.Equal(t, flowCommunityPool, stop.Class)
	stop, err = loader.stop(ics20EscrowAddress("transfer", "channel-141"))
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowIBCOut, Detail: "transfer/channel-141"}, stop)
	stop, err = loader.stop(op)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchange, Detail: "Named Exchange"}, stop)
	stop, err = loader.stop(flowPGDeposit)
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
				got, err := shared.stop(op)
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
			got, err := shared.stop(flowPGDeposit)
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

	// Until the list of validators that ever withdrew commission is known,
	// and while any of them has no run, the Hub shows coverage, not totals.
	s.flowExp = map[string]*flowExpected{chain: {loading: true}}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.False(t, hub.Complete)
	require.Equal(t, 0, hub.Expected)
	require.Empty(t, hub.Months)
	require.True(t, hub.Total.Withdrawn.IsZero())
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

	flowPGExpect(s, chain, flowPGValidator, flowPGOther)
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows?chain=cosmoshub", http.StatusOK, &hub)
	require.True(t, hub.Complete)
	require.Equal(t, 2, hub.Traced)
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
	set := map[string]bool{}
	for _, v := range validators {
		set[v] = true
	}
	s.flowExp = map[string]*flowExpected{chain: {set: set, at: time.Now()}}
}
