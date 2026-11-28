package taxapi

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	prev6Distribution = "cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl"
	prev6Other        = "cosmos1other"
	prev6Other2       = "cosmos1other2"
)

// prev6Fixture reads a recorded SDK 0.42 transaction's log into the message
// events the index stores, one message per log entry.
func prev6Fixture(t *testing.T, name string) (int64, []ledgerEvent) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "tax", "testdata", "hub-legacy", name+".json"))
	require.NoError(t, err)
	var f struct {
		Height int64 `json:"height"`
		Logs   []struct {
			MsgIndex uint64 `json:"msg_index"`
			Events   []struct {
				Type       string `json:"type"`
				Attributes []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"attributes"`
			} `json:"events"`
		} `json:"logs"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	var out []ledgerEvent
	id := uint64(0)
	for _, l := range f.Logs {
		for _, e := range l.Events {
			id++
			ev := ledgerEvent{ID: id, Msg: l.MsgIndex + 1, Height: f.Height, Phase: phaseTx, Type: e.Type}
			for _, a := range e.Attributes {
				ev.Attrs = append(ev.Attrs, ledgerAttr{Key: a.Key, Value: a.Value})
			}
			out = append(out, ev)
		}
	}
	return f.Height, out
}

func firstOfType(events []ledgerEvent, typ string) ledgerEvent {
	for _, e := range events {
		if e.Type == typ {
			return e
		}
	}
	return ledgerEvent{}
}

func walkLiquid(t *testing.T, addr string, h int64, events []ledgerEvent, bank int64) map[string]*big.Int {
	t.Helper()
	in := ledgerInput{
		Address:        addr,
		BondDenom:      "uatom",
		Anchor:         ledgerAnchor{Height: h, Bank: map[string]*big.Int{"uatom": bi(bank)}},
		Events:         events,
		TransfersBelow: hubCoinEventsFrom,
	}
	got, err := walkBack(in, []int64{h - 1})
	require.NoError(t, err)
	return got[h-1].Liquid
}

// A real SDK 0.42 send moves the sender's and recipient's coins only through
// its transfer record.
func TestPrev6SendFixture(t *testing.T) {
	h, msg := prev6Fixture(t, "prev6-send")
	rec := firstOfType(msg, "transfer").records()[0]
	from, to := rec["sender"], rec["recipient"]
	require.NotEqual(t, from, to)
	for _, who := range []string{from, to} {
		events, unknown := prev6Ledger(msg, nil, who, "uatom")
		require.Zero(t, unknown)
		got := walkLiquid(t, who, h, events, 100000000)
		want := int64(100000000 + 9023551)
		if who == to {
			want = 100000000 - 9023551
		}
		intsEqual(t, who, got, map[string]int64{"uatom": want})
	}
}

// A real MultiSend writes its outputs without a sender; the lone input,
// named only on the message event, spent their sum.
func TestPrev6MultiSendFixture(t *testing.T) {
	h, msg := prev6Fixture(t, "prev6-multisend")
	var sender string
	for _, a := range firstOfType(msg, "message").Attrs {
		if a.Key == "sender" {
			sender = a.Value
		}
	}
	recipient := firstOfType(msg, "transfer").records()[0]["recipient"]
	require.NotEmpty(t, sender)
	events, unknown := prev6Ledger(msg, nil, sender, "uatom")
	require.Zero(t, unknown)
	intsEqual(t, "sender", walkLiquid(t, sender, h, events, 1000), map[string]int64{"uatom": 1000 + 97345000})
	events, unknown = prev6Ledger(msg, nil, recipient, "uatom")
	require.Zero(t, unknown)
	intsEqual(t, "recipient", walkLiquid(t, recipient, h, events, 97345000), map[string]int64{})
}

func TestPrev6MultiSendSharedInputsUnknown(t *testing.T) {
	msg := []ledgerEvent{
		{ID: 1, Msg: 7, Height: 6000000, Phase: phaseTx, Type: "message", Attrs: []ledgerAttr{{"action", "multisend"}, {"sender", ledgerTestAddr}, {"sender", prev6Other}, {"module", "bank"}}},
		{ID: 2, Msg: 7, Height: 6000000, Phase: phaseTx, Type: "transfer", Attrs: []ledgerAttr{{"recipient", prev6Other2}, {"amount", "10uatom"}}},
	}
	events, unknown := prev6Ledger(msg, nil, ledgerTestAddr, "uatom")
	require.Equal(t, int64(6000000), unknown)
	require.Len(t, events, 1)
	// The other input's wallet is as unknown.
	_, unknown = prev6Ledger(msg, nil, prev6Other, "uatom")
	require.Equal(t, int64(6000000), unknown)
	// A recipient is still exact.
	events, unknown = prev6Ledger(msg, nil, prev6Other2, "uatom")
	require.Zero(t, unknown)
	intsEqual(t, "recipient", walkLiquid(t, prev6Other2, 6000000, events, 10), map[string]int64{})
}

func TestPrev6BlockEvents(t *testing.T) {
	block := []ledgerEvent{
		ev(10, 7000000, phaseEnd, "transfer", "recipient", ledgerTestAddr, "amount", "5uatom,3pool1"),
		ev(11, 7000000, phaseEnd, "transfer", "recipient", ledgerTestAddr, "sender", prev6Other, "amount", "2uatom"),
	}
	events, unknown := prev6Ledger(nil, block, ledgerTestAddr, "uatom")
	require.Zero(t, unknown)
	in := ledgerInput{
		Address: ledgerTestAddr, BondDenom: "uatom", TransfersBelow: hubCoinEventsFrom,
		Anchor: ledgerAnchor{Height: 7000000, Bank: map[string]*big.Int{"uatom": bi(10), "pool1": bi(3)}}, Events: events,
	}
	got, err := walkBack(in, []int64{6999999})
	require.NoError(t, err)
	intsEqual(t, "liquid", got[6999999].Liquid, map[string]int64{"uatom": 3})
	// An input taken from the wallet outside a transaction names no amount.
	_, unknown = prev6Ledger(nil, append(block, ev(12, 7000001, phaseEnd, "message", "sender", ledgerTestAddr)), ledgerTestAddr, "uatom")
	require.Equal(t, int64(7000001), unknown)
}

// Transfers at or after the first block with coin events are left to the
// coin events, so a stray one is never counted twice.
func TestTransfersIgnoredFromCoinEvents(t *testing.T) {
	in := ledgerInput{
		Address: ledgerTestAddr, BondDenom: "uatom", TransfersBelow: hubCoinEventsFrom,
		Anchor: ledgerAnchor{Height: hubCoinEventsFrom + 10, Bank: map[string]*big.Int{"uatom": bi(10)}},
		Events: []ledgerEvent{ev(1, hubCoinEventsFrom, phaseTx, "transfer", "recipient", ledgerTestAddr, "amount", "4uatom")},
	}
	got, err := walkBack(in, []int64{hubCoinEventsFrom - 1})
	require.NoError(t, err)
	intsEqual(t, "liquid", got[hubCoinEventsFrom-1].Liquid, map[string]int64{"uatom": 10})
	in.TransfersBelow = 0
	in.Events[0].Height = 100
	got, err = walkBack(in, []int64{99})
	require.NoError(t, err)
	intsEqual(t, "liquid off", got[99].Liquid, map[string]int64{"uatom": 10})
}

// Before v6 a bond, a self-bond and an unbonding payout move liquid coins
// with no bank event; after it the coin events carry that, so the staking
// events move only shares.
func TestWalkBackPrev6StakingLiquid(t *testing.T) {
	events := []ledgerEvent{
		ev(1, 6000010, phaseTx, "create_validator", "validator", ledgerTestVal, "amount", "100"),
		ev(2, 6000020, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "400"),
		ev(3, 6000030, phaseEnd, "complete_unbonding", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "70uatom"),
		ev(4, 9000000, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom"),
		ev(5, 9000000, phaseTx, "coin_spent", "spender", ledgerTestAddr, "amount", "50uatom"),
	}
	in := ledgerInput{
		Address: ledgerTestAddr, Operator: ledgerTestVal, BondDenom: "uatom", TransfersBelow: hubCoinEventsFrom,
		Anchor: ledgerAnchor{
			Height: 9000001, Bank: map[string]*big.Int{"uatom": bi(1000)},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(550)},
			Rates:       map[string]validatorRate{ledgerTestVal: unitRate(1000), ledgerTestVal2: unitRate(1000)},
		},
		Events: events,
	}
	got, err := walkBack(in, []int64{8999999, 6000025, 6000015, 6000000})
	require.NoError(t, err)
	intsEqual(t, "after v6 bond", got[8999999].Liquid, map[string]int64{"uatom": 1050})
	intsEqual(t, "after v6 bond shares", got[8999999].Delegations, map[string]int64{ledgerTestVal: 500})
	intsEqual(t, "before payout", got[6000025].Liquid, map[string]int64{"uatom": 980})
	intsEqual(t, "before bond", got[6000015].Liquid, map[string]int64{"uatom": 1380})
	intsEqual(t, "before bond shares", got[6000015].Delegations, map[string]int64{ledgerTestVal: 100})
	intsEqual(t, "before self-bond", got[6000000].Liquid, map[string]int64{"uatom": 1480})
	intsEqual(t, "before self-bond shares", got[6000000].Delegations, map[string]int64{})
	// Off, the same staking events move only shares.
	in.TransfersBelow = 0
	got, err = walkBack(in, []int64{6000000})
	require.NoError(t, err)
	intsEqual(t, "off", got[6000000].Liquid, map[string]int64{"uatom": 1050})
}

func TestAttributeUnnamedStakingFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, category, amount string
	}{
		{"prev6-delegate-autoclaim", "delegate", "1000000"},
		{"prev6-undelegate-autoclaim", "undelegate", "8608625"},
	} {
		h, msg := prev6Fixture(t, tc.name)
		// The automatic reward withdrawal pays the delegator.
		var delegator string
		for _, rec := range firstOfType(msg, "transfer").records() {
			if rec["sender"] == prev6Distribution {
				delegator = rec["recipient"]
			}
		}
		require.NotEmpty(t, delegator, tc.name)
		typ := stakingEventType[tc.category]
		module := firstOfType(msg, typ)
		val := module.records()[0]["validator"]
		row := stakingTaxRow{MessageID: module.Msg, BlockHeight: h, Category: tc.category, Amount: tc.amount, Denom: "uatom", ValidatorAddress: val}
		if tc.category == "undelegate" {
			row.ToAddr = delegator
		} else {
			row.FromAddr = delegator
		}
		var staking []ledgerEvent
		for _, e := range msg {
			if e.Type == typ {
				staking = append(staking, e)
			}
		}
		got, unattributed := attributeUnnamedStaking(staking, []stakingTaxRow{row}, delegator, "uatom")
		require.Zero(t, unattributed, tc.name)
		require.Len(t, got, 1, tc.name)
		rec := got[0].records()[0]
		require.Equal(t, delegator, rec["delegator"], tc.name)
		require.Equal(t, tc.amount, rec["amount"], tc.name)
		require.Equal(t, val, rec["validator"], tc.name)
		// Another wallet gets nothing from the same message.
		got, unattributed = attributeUnnamedStaking(staking, []stakingTaxRow{row}, prev6Other, "uatom")
		require.Zero(t, unattributed)
		require.Empty(t, got)

		// The whole message walks back: the bond and the reward.
		pre, unknown := prev6Ledger(msg, nil, delegator, "uatom")
		require.Zero(t, unknown)
		events, _ := attributeUnnamedStaking(staking, []stakingTaxRow{row}, delegator, "uatom")
		in := ledgerInput{
			Address: delegator, BondDenom: "uatom", TransfersBelow: hubCoinEventsFrom,
			Anchor: ledgerAnchor{
				Height: h, Time: time.Date(2021, 3, 7, 1, 0, 0, 0, time.UTC), Bank: map[string]*big.Int{"uatom": bi(50000000)},
				Delegations: map[string]*big.Int{val: bi(20000000)}, Rates: map[string]validatorRate{val: unitRate(1000)},
			},
			Events: append(pre, events...),
		}
		if tc.category == "undelegate" {
			in.Anchor.Unbonding = []unbondingEntry{{Validator: val, CreationHeight: h, Balance: bi(8608625)}}
		}
		held, err := walkBack(in, []int64{h - 1})
		require.NoError(t, err, tc.name)
		switch tc.category {
		case "delegate":
			intsEqual(t, "delegate liquid", held[h-1].Liquid, map[string]int64{"uatom": 50000000 + 1000000 - 31})
			intsEqual(t, "delegate shares", held[h-1].Delegations, map[string]int64{val: 19000000})
		case "undelegate":
			intsEqual(t, "undelegate liquid", held[h-1].Liquid, map[string]int64{"uatom": 50000000 - 60528})
			intsEqual(t, "undelegate shares", held[h-1].Delegations, map[string]int64{val: 20000000 + 8608625})
			intsEqual(t, "undelegate entries", held[h-1].Unbonding, map[string]int64{})
		}
	}
}

func TestAttributeUnnamedStakingShared(t *testing.T) {
	h := int64(12000000)
	exec := ev(5, h, phaseTx, "delegate",
		"validator", ledgerTestVal, "amount", "10uatom",
		"validator", ledgerTestVal2, "amount", "20uatom",
		"validator", ledgerTestVal, "amount", "10uatom")
	exec.Msg = 9
	rows := []stakingTaxRow{
		{MessageID: 9, BlockHeight: h, Category: "delegate", FromAddr: prev6Other, Amount: "10", Denom: "uatom", ValidatorAddress: ledgerTestVal},
		{MessageID: 9, BlockHeight: h, Category: "delegate", FromAddr: ledgerTestAddr, Amount: "20", Denom: "uatom", ValidatorAddress: ledgerTestVal2},
		{MessageID: 9, BlockHeight: h, Category: "delegate", FromAddr: ledgerTestAddr, Amount: "10", Denom: "uatom", ValidatorAddress: ledgerTestVal},
	}
	got, unattributed := attributeUnnamedStaking([]ledgerEvent{exec}, rows, ledgerTestAddr, "uatom")
	require.Zero(t, unattributed)
	require.Len(t, got, 2)
	total := map[string]string{}
	for _, e := range got {
		rec := e.records()[0]
		require.Equal(t, ledgerTestAddr, rec["delegator"])
		total[rec["validator"]] = rec["amount"]
	}
	require.Equal(t, map[string]string{ledgerTestVal: "10uatom", ledgerTestVal2: "20uatom"}, total)
	got, unattributed = attributeUnnamedStaking([]ledgerEvent{exec}, rows, prev6Other, "uatom")
	require.Zero(t, unattributed)
	require.Len(t, got, 1)

	// Rows that do not match the records refuse dates before the message.
	_, unattributed = attributeUnnamedStaking([]ledgerEvent{exec}, rows[1:], ledgerTestAddr, "uatom")
	require.Equal(t, h, unattributed)
	// So does a message whose events already name a delegator.
	named := ev(6, h+1, phaseTx, "unbond", "validator", ledgerTestVal, "amount", "5uatom", "delegator", prev6Other)
	named.Msg = 10
	_, unattributed = attributeUnnamedStaking([]ledgerEvent{named}, []stakingTaxRow{{MessageID: 10, BlockHeight: h + 1, Category: "undelegate", ToAddr: ledgerTestAddr, Amount: "5", Denom: "uatom", ValidatorAddress: ledgerTestVal}}, ledgerTestAddr, "uatom")
	require.Equal(t, h+1, unattributed)
}

func TestAttributeUnnamedRedelegation(t *testing.T) {
	h := int64(15000000)
	red := ev(3, h, phaseTx, "redelegate", "source_validator", ledgerTestVal, "destination_validator", ledgerTestVal2, "amount", "1000000uatom", "completion_time", "2023-04-01T00:00:00Z")
	red.Msg = 4
	row := stakingTaxRow{
		MessageID: 4, BlockHeight: h, Category: "redelegate", FromAddr: ledgerTestAddr, ToAddr: ledgerTestAddr, Amount: "1000000", Denom: "uatom", ValidatorAddress: ledgerTestVal2,
		Detail: `{"completion_time":"2023-04-01T00:00:00Z","destination_validator":"` + ledgerTestVal2 + `","source_validator":"` + ledgerTestVal + `"}`,
	}
	got, unattributed := attributeUnnamedStaking([]ledgerEvent{red}, []stakingTaxRow{row}, ledgerTestAddr, "uatom")
	require.Zero(t, unattributed)
	require.Len(t, got, 1)
	rec := got[0].records()[0]
	require.Equal(t, ledgerTestVal, rec["source_validator"])
	require.Equal(t, ledgerTestAddr, rec["delegator"])
	// A source the row does not name does not match.
	row.Detail = `{"source_validator":"cosmosvaloper1c"}`
	_, unattributed = attributeUnnamedStaking([]ledgerEvent{red}, []stakingTaxRow{row}, ledgerTestAddr, "uatom")
	require.Equal(t, h, unattributed)
}

func TestEarlyHubLedgerFromEnv(t *testing.T) {
	t.Setenv("TAX_BALANCE_PRE_V6_TRANSFERS", "")
	t.Setenv("TAX_BALANCE_UNNAMED_STAKING", "")
	require.Equal(t, earlyHubLedger{}, earlyHubLedgerFromEnv())
	t.Setenv("TAX_BALANCE_PRE_V6_TRANSFERS", "on")
	t.Setenv("TAX_BALANCE_UNNAMED_STAKING", "on")
	require.Equal(t, earlyHubLedger{transfers: true, unnamedStaking: true}, earlyHubLedgerFromEnv())
}

// The loaders read the index's own tables: transfer events that name the
// wallet, the transfers of messages it sent, block events, and the staking
// events of messages its staking rows name.
func TestEarlyHubLoadersPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres ledger verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	w := ledgerTestAddr
	for _, sql := range []string{
		tempSearchPath,
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, time_stamp timestamptz)",
		"CREATE TEMP TABLE txes (id bigint, block_id bigint)",
		"CREATE TEMP TABLE messages (id bigint, tx_id bigint)",
		"CREATE TEMP TABLE message_event_types (id bigint, type text)",
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index int)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, message_event_attribute_key_id bigint, value text, index int)",
		"CREATE TEMP TABLE block_event_types (id bigint, type text)",
		"CREATE TEMP TABLE block_events (id bigint, block_id bigint, block_event_type_id bigint, lifecycle_position int, index int)",
		"CREATE TEMP TABLE block_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE block_event_attributes (block_event_id bigint, block_event_attribute_key_id bigint, value text, index int)",
		"CREATE TEMP TABLE taxable_events (message_id bigint, sub_index int, block_height bigint, category text, amount text, denom text, from_addr text, to_addr text, validator_address text, detail text)",
		"INSERT INTO blocks VALUES (1,1,6000000,'2021-06-01'),(2,1,6000001,'2021-06-01'),(3,1,9000000,'2022-01-01'),(4,2,6000000,'2021-06-01')",
		"INSERT INTO txes VALUES (1,1),(2,2),(3,3),(4,4)",
		"INSERT INTO messages VALUES (1,1),(2,2),(3,3),(4,4)",
		"INSERT INTO message_event_types VALUES (1,'message'),(2,'transfer'),(3,'delegate')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'sender'),(2,'recipient'),(3,'amount'),(4,'action'),(5,'validator')",
		// Message 1: the wallet's lone MultiSend to another wallet.
		"INSERT INTO message_events VALUES (1,1,1,0),(2,1,2,1)",
		"INSERT INTO message_event_attributes VALUES (1,4,'multisend',0),(1,1,'" + w + "',1),(2,2,'" + prev6Other + "',0),(2,3,'7uatom',1)",
		// Message 2: a send to the wallet and a bond, at the next block.
		"INSERT INTO message_events VALUES (3,2,2,0),(4,2,3,1)",
		"INSERT INTO message_event_attributes VALUES (3,2,'" + w + "',0),(3,1,'" + prev6Other + "',1),(3,3,'9uatom',2),(4,5,'" + ledgerTestVal + "',0),(4,3,'4',1)",
		// Message 3: after v6, never read as a transfer.
		"INSERT INTO message_events VALUES (5,3,2,0)",
		"INSERT INTO message_event_attributes VALUES (5,2,'" + w + "',0),(5,3,'1uatom',1)",
		// Message 4: another chain.
		"INSERT INTO message_events VALUES (6,4,2,0)",
		"INSERT INTO message_event_attributes VALUES (6,2,'" + w + "',0),(6,3,'1uatom',1)",
		"INSERT INTO block_event_types VALUES (1,'transfer')",
		"INSERT INTO block_event_attribute_keys VALUES (1,'recipient'),(2,'amount')",
		"INSERT INTO block_events VALUES (1,2,1,1,0)",
		"INSERT INTO block_event_attributes VALUES (1,1,'" + w + "',0),(1,2,'3uatom',1)",
		"INSERT INTO taxable_events VALUES (2,0,6000001,'delegate','4','uatom','" + w + "','','" + ledgerTestVal + "','')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}
	events, unknown, err := s.prev6Events([]int64{1}, w, 5999999, 30000000)
	require.NoError(t, err)
	require.Zero(t, unknown)
	staking, unattributed, err := s.unnamedStakingEvents([]int64{1}, w, 5999999, 30000000)
	require.NoError(t, err)
	require.Zero(t, unattributed)
	require.Len(t, staking, 1)
	in := ledgerInput{
		Address: w, BondDenom: "uatom", TransfersBelow: hubCoinEventsFrom,
		Anchor: ledgerAnchor{
			Height: 9000000, Bank: map[string]*big.Int{"uatom": bi(100)}, Delegations: map[string]*big.Int{ledgerTestVal: bi(4)},
			Rates: map[string]validatorRate{ledgerTestVal: unitRate(10)},
		},
		Events: append(events, staking...),
	}
	got, err := walkBack(in, []int64{6000000, 5999999})
	require.NoError(t, err)
	// After block 6000001: +9 received, +3 in the block, -4 bonded, undone.
	intsEqual(t, "h6000000", got[6000000].Liquid, map[string]int64{"uatom": 100 - 9 - 3 + 4})
	intsEqual(t, "h6000000 stake", got[6000000].Delegations, map[string]int64{})
	intsEqual(t, "h5999999", got[5999999].Liquid, map[string]int64{"uatom": 100 - 9 - 3 + 4 + 7})
	// A window after v6 reads nothing.
	events, _, err = s.prev6Events([]int64{1}, w, hubCoinEventsFrom-1, 30000000)
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestHoldingsMismatch(t *testing.T) {
	got := HeightHoldings{
		Liquid:          map[string]string{"uatom": "100"},
		Delegations:     map[string]string{ledgerTestVal: "50", ledgerTestVal2: "10"},
		DelegationError: map[string]string{ledgerTestVal: "2"},
	}
	want := HeightHoldings{
		Liquid:      map[string]string{"uatom": "100"},
		Delegations: map[string]string{ledgerTestVal: "52", ledgerTestVal2: "10"},
	}
	require.Empty(t, HoldingsMismatch(got, want))
	want.Delegations[ledgerTestVal] = "53"
	want.Unbonding = map[string]string{ledgerTestVal2: "1"}
	want.Liquid["ibc/X"] = "1"
	require.Equal(t, []string{"delegations " + ledgerTestVal, "liquid ibc/X", "unbonding " + ledgerTestVal2}, HoldingsMismatch(got, want))
}
