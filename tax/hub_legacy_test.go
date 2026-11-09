package tax

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/x/authz"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// hubLegacyFixture loads a real cosmoshub-4 transaction from the archive
// (tax/testdata/hub-legacy/<name>.json): its body as broadcast and the raw
// log the chain wrote for each message.
func hubLegacyFixture(t *testing.T, name string) ([]sdk.Msg, []*txlog.LogMessage) {
	t.Helper()
	data, err := os.ReadFile("testdata/hub-legacy/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Code int                `json:"code"`
		Tx   string             `json:"tx"`
		Logs []txlog.LogMessage `json:"logs"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Code != 0 {
		t.Fatal("fixture transaction failed")
	}
	raw, err := base64.StdEncoding.DecodeString(fixture.Tx)
	if err != nil {
		t.Fatal(err)
	}
	var txRaw sdktx.TxRaw
	if err := txRaw.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	var body sdktx.TxBody
	if err := body.Unmarshal(txRaw.BodyBytes); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != len(fixture.Logs) {
		t.Fatalf("%d messages, %d logs", len(body.Messages), len(fixture.Logs))
	}
	msgs := make([]sdk.Msg, len(body.Messages))
	logs := make([]*txlog.LogMessage, len(body.Messages))
	for i, a := range body.Messages {
		if msgs[i], err = unpackAuthzInner(a); err != nil {
			t.Fatal(err)
		}
		logs[i] = &fixture.Logs[i]
	}
	return msgs, logs
}

const legacyParserID = "test"

func parseLegacy(t *testing.T, msg sdk.Msg, log *txlog.LogMessage) []TaxableEvent {
	t.Helper()
	data, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, config.IndexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return (*data).([]TaxableEvent)
}

func byCategory(events []TaxableEvent, category string) []TaxableEvent {
	var out []TaxableEvent
	for _, e := range events {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out
}

// SDK 0.42 wrote the delegate, unbond and redelegate amounts without a denom.
func TestPreV6StakingPrincipalBareAmounts(t *testing.T) {
	for _, tc := range []struct {
		name, category, amount string
		msg                    int
	}{
		{"prev6-withdraw-then-delegate", string(CategoryDelegate), "1009439", 1},
		{"prev6-delegate-autoclaim", string(CategoryDelegate), "1000000", 0},
		{"prev6-undelegate-autoclaim", string(CategoryUndelegate), "8608625", 0},
		{"prev6-redelegate-two-claims", string(CategoryRedelegate), "1000000000", 0},
	} {
		msgs, logs := hubLegacyFixture(t, tc.name)
		rows := byCategory(parseLegacy(t, msgs[tc.msg], logs[tc.msg]), tc.category)
		if len(rows) != 1 || rows[0].Amount != tc.amount || rows[0].Denom != fixtureNativeDenom {
			t.Fatalf("%s: principal %+v, want %s%s", tc.name, rows, tc.amount, fixtureNativeDenom)
		}
	}
}

// Before withdraw_rewards was emitted for a staking message's automatic
// withdrawal, the distribution module's transfer is the only record of it.
func TestAutoClaimedRewardsFromDistributionTransfers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		amounts    []string
		validators []string
	}{
		{"prev6-delegate-autoclaim", []string{"31"}, nil},
		{"prev6-undelegate-autoclaim", []string{"60528"}, nil},
		{"prev6-redelegate-two-claims", []string{"9324", "2499"}, nil},
		{"prev6-redelegate-one-claim", []string{"3641"}, []string{""}},
		{"v044-undelegate-autoclaim", []string{"4"}, nil},
	} {
		msgs, logs := hubLegacyFixture(t, tc.name)
		rewards := byCategory(parseLegacy(t, msgs[0], logs[0]), string(CategoryReward))
		if len(rewards) != len(tc.amounts) {
			t.Fatalf("%s: rewards %+v", tc.name, rewards)
		}
		var delegator string
		var fromMessage []string
		switch m := msgs[0].(type) {
		case *stakingtypes.MsgDelegate:
			delegator, fromMessage = m.DelegatorAddress, []string{m.ValidatorAddress}
		case *stakingtypes.MsgUndelegate:
			delegator, fromMessage = m.DelegatorAddress, []string{m.ValidatorAddress}
		case *stakingtypes.MsgBeginRedelegate:
			delegator, fromMessage = m.DelegatorAddress, []string{m.ValidatorSrcAddress, m.ValidatorDstAddress}
		}
		validators := tc.validators
		if validators == nil {
			validators = fromMessage
		}
		for i, r := range rewards {
			if r.Amount != tc.amounts[i] || r.Denom != fixtureNativeDenom || r.ToAddr != delegator || r.ValidatorAddress != validators[i] || r.RewardTrigger == "" {
				t.Fatalf("%s: reward %d %+v", tc.name, i, r)
			}
		}
	}
}

// An explicit withdrawal before Gaia v6 still emits withdraw_rewards, and the
// delegation after it in the same transaction withdrew nothing.
func TestPreV6WithdrawThenDelegate(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "prev6-withdraw-then-delegate")
	claim := byCategory(parseLegacy(t, msgs[0], logs[0]), string(CategoryReward))
	if len(claim) != 1 || claim[0].Amount != "1009457" {
		t.Fatalf("claim %+v", claim)
	}
	if again := byCategory(parseLegacy(t, msgs[1], logs[1]), string(CategoryReward)); len(again) != 0 {
		t.Fatalf("delegate after the claim withdrew %+v", again)
	}
}

func TestPreV6SendsFromTheBody(t *testing.T) {
	for _, name := range []string{"prev6-send", "prev6-multisend"} {
		msgs, logs := hubLegacyFixture(t, name)
		rows := byCategory(parseLegacy(t, msgs[0], logs[0]), string(CategoryTransfer))
		if len(rows) == 0 {
			t.Fatalf("%s: no transfer rows", name)
		}
		for _, r := range rows {
			if r.FromAddr == "" || r.ToAddr == "" || r.Denom != fixtureNativeDenom {
				t.Fatalf("%s: %+v", name, r)
			}
		}
	}
}

// execDelegates reads, from the inner messages alone, which delegator each
// reward and delegation belongs to.
func execDelegates(t *testing.T, exec sdk.Msg) []*stakingtypes.MsgDelegate {
	t.Helper()
	m, ok := exec.(*authz.MsgExec)
	if !ok {
		t.Fatalf("not an Exec: %T", exec)
	}
	var out []*stakingtypes.MsgDelegate
	for _, a := range m.Msgs {
		inner, err := unpackAuthzInner(a)
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := inner.(*stakingtypes.MsgDelegate); ok {
			out = append(out, d)
		}
	}
	return out
}

// One Exec restaking for many granters. With authz_msg_index on a merged raw
// log, each granter keeps its own reward and delegation.
func TestAuthzMergedIndexedLogSplitsPerGranter(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-indexed")
	delegates := execDelegates(t, msgs[0])
	out := parseLegacy(t, msgs[0], logs[0])
	assertPerGranter(t, out, delegates)
}

// The same restake before the SDK tagged inner events: the raw log's order
// assigns each granter its transfer and its delegate record.
func TestAuthzUntaggedLogSplitsPerGranter(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-delegates")
	delegates := execDelegates(t, msgs[0])
	out := parseLegacy(t, msgs[0], logs[0])
	assertPerGranter(t, out, delegates)
}

func assertPerGranter(t *testing.T, out []TaxableEvent, delegates []*stakingtypes.MsgDelegate) {
	t.Helper()
	if len(delegates) < 2 {
		t.Fatalf("fixture has %d delegates", len(delegates))
	}
	principal := byCategory(out, string(CategoryDelegate))
	rewards := byCategory(out, string(CategoryReward))
	if len(principal) != len(delegates) || len(rewards) != len(delegates) {
		t.Fatalf("%d delegates, %d principal rows, %d rewards", len(delegates), len(principal), len(rewards))
	}
	for i, d := range delegates {
		p := principal[i]
		if p.FromAddr != d.DelegatorAddress || p.Amount != d.Amount.Amount.String() || p.ValidatorAddress != d.ValidatorAddress {
			t.Fatalf("delegate %d: %+v for %s", i, p, d.DelegatorAddress)
		}
		if rewards[i].ToAddr != d.DelegatorAddress || rewards[i].ValidatorAddress != d.ValidatorAddress {
			t.Fatalf("reward %d: %+v for %s", i, rewards[i], d.DelegatorAddress)
		}
	}
}

// Thirteen Execs, each a granter's withdrawal then delegation, with no index.
func TestAuthzUntaggedWithdrawThenDelegate(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-withdraw-delegate")
	for i := range msgs {
		delegates := execDelegates(t, msgs[i])
		out := parseLegacy(t, msgs[i], logs[i])
		principal := byCategory(out, string(CategoryDelegate))
		rewards := byCategory(out, string(CategoryReward))
		if len(delegates) != 1 || len(principal) != 1 || len(rewards) != 1 {
			t.Fatalf("exec %d: %d delegates, principal %+v, rewards %+v", i, len(delegates), principal, rewards)
		}
		d := delegates[0]
		if principal[0].FromAddr != d.DelegatorAddress || principal[0].Amount != d.Amount.Amount.String() || rewards[0].ToAddr != d.DelegatorAddress || rewards[0].RewardTrigger != actionClaim {
			t.Fatalf("exec %d: principal %+v reward %+v", i, principal[0], rewards[0])
		}
	}
}

// A log that cannot be separated is reported, never classified by guess.
func TestAuthzUntaggedMismatchIsIncomplete(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-delegates")
	log := &txlog.LogMessage{}
	for _, ev := range logs[0].Events {
		if ev.Type == "delegate" {
			ev.Attributes = ev.Attributes[3:]
		}
		log.Events = append(log.Events, ev)
	}
	if _, err := (&Parser{ID: legacyParserID}).ParseMessage(msgs[0], log, config.IndexConfig{}); err == nil {
		t.Fatal("an Exec with a missing delegate record classified without error")
	}
}

// A validator's REStake-style Exec: its own reward, then its commission, with
// no index. The commission handler closes with the operator address.
func TestAuthzUntaggedRewardAndCommission(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-commission")
	exec := msgs[0].(*authz.MsgExec)
	var delegator, operator string
	for _, a := range exec.Msgs {
		inner, err := unpackAuthzInner(a)
		if err != nil {
			t.Fatal(err)
		}
		switch m := inner.(type) {
		case *disttypes.MsgWithdrawDelegatorReward:
			delegator = m.DelegatorAddress
		case *disttypes.MsgWithdrawValidatorCommission:
			operator = m.ValidatorAddress
		}
	}
	out := parseLegacy(t, msgs[0], logs[0])
	rewards := byCategory(out, string(CategoryReward))
	commission := byCategory(out, string(CategoryCommission))
	if len(rewards) != 1 || rewards[0].ToAddr != delegator || rewards[0].RewardTrigger != actionClaim {
		t.Fatalf("rewards %+v", rewards)
	}
	withdrawn, err := sdk.ParseCoinsNormalized(attrValue(eventAttrs(logs[0], eventWithdrawCommission), attributeAmount))
	if err != nil {
		t.Fatal(err)
	}
	if len(commission) != 1 || commission[0].ValidatorAddress != operator || commission[0].Amount != withdrawn.AmountOf(fixtureNativeDenom).String() {
		t.Fatalf("commission %+v", commission)
	}
}

// An Exec restaking for many granters that used up one grant: the chain's
// EventRevoke for it closes no handler and must not shift the others.
func TestAuthzUntaggedRestakeWithRevoke(t *testing.T) {
	msgs, logs := hubLegacyFixture(t, "v045-exec-revoke")
	out := parseLegacy(t, msgs[0], logs[0])
	assertPerGranter(t, out, execDelegates(t, msgs[0]))
}

func eventAttrs(log *txlog.LogMessage, typ string) []txlog.Attribute {
	for _, ev := range log.Events {
		if ev.Type == typ {
			return ev.Attributes
		}
	}
	return nil
}
