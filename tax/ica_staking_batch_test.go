package tax

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
)

type icaFixture struct {
	MessageType string                  `json:"message_type"`
	Height      int64                   `json:"height"`
	Events      []txlog.LogMessageEvent `json:"events"`
}

func loadICAFixture(t *testing.T, name string) icaFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/ica-host/" + name + ".json")
	require.NoError(t, err)
	var fx icaFixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	return fx
}

func parseICAEvents(t *testing.T, messageType string, events []txlog.LogMessageEvent) ([]TaxableEvent, error) {
	t.Helper()
	log := &txlog.LogMessage{Events: events}
	msg, err := ReconstructMsg(messageType, log)
	require.NoError(t, err)
	out, err := (&Parser{ID: legacyParserID}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		return nil, err
	}
	return (*out).([]TaxableEvent), nil
}

// storedGroups counts the attribute groups of each staking event type and sums
// every withdraw_rewards group, merged or not.
func storedGroups(t *testing.T, events []txlog.LogMessageEvent) (map[string]int, sdk.Coins) {
	t.Helper()
	counts := map[string]int{}
	rewards := sdk.NewCoins()
	for _, ev := range events {
		switch ev.Type {
		case eventWithdrawRewards:
			groups, err := rewardAttributeGroups(ev)
			require.NoError(t, err)
			for _, g := range groups {
				coins, err := sdk.ParseCoinsNormalized(g[attributeAmount])
				require.NoError(t, err)
				rewards = rewards.Add(coins...)
			}
		case string(CategoryDelegate), eventUnbond, string(CategoryRedelegate):
			counts[ev.Type] += len(splitAttributeGroups(ev))
		}
	}
	return counts, rewards
}

// Interchain accounts delegate, undelegate, redelegate and withdraw in batches
// of up to nineteen messages. Every reward group the chain paid in the packet is
// booked once to the host account, and every staking event becomes its
// record-only principal row, for logs that merge same-type events (before SDK
// 0.47) and logs that keep one event per emission.
const (
	icaDelegatesFixture   = "delegates"
	icaUndelegatesFixture = "undelegates"
)

func TestICAStakingBatchesBookEveryInnerMessage(t *testing.T) {
	delegate, undelegate, redelegate := string(CategoryDelegate), string(CategoryUndelegate), string(CategoryRedelegate)
	for name, trigger := range map[string]string{
		"delegates-merged":               delegate,
		icaDelegatesFixture:              delegate,
		"delegates-merged-new-validator": delegate,
		"withdrawals":                    actionClaim,
		"withdrawals-merged":             actionClaim,
		"withdrawals-second-account":     actionClaim,
		icaUndelegatesFixture:            undelegate,
		"undelegates-merged":             undelegate,
		"redelegates":                    redelegate,
		"redelegates-merged":             redelegate,
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadICAFixture(t, name)
			rows, err := parseICAEvents(t, fx.MessageType, fx.Events)
			require.NoError(t, err)
			counts, want := storedGroups(t, fx.Events)
			require.False(t, want.IsZero())

			got := sdk.NewCoins()
			principal := map[string]int{}
			host := ""
			for _, r := range rows {
				switch r.Category {
				case string(CategoryReward):
					if host == "" {
						host = r.ToAddr
					}
					require.Equal(t, host, r.ToAddr, "every reward belongs to the host account")
					require.Equal(t, trigger, r.RewardTrigger)
					require.NotEmpty(t, r.ValidatorAddress)
					amount, ok := sdk.NewIntFromString(r.Amount)
					require.True(t, ok)
					got = got.Add(sdk.NewCoin(r.Denom, amount))
				case string(CategoryDelegate):
					principal[string(CategoryDelegate)]++
				case string(CategoryUndelegate):
					principal[eventUnbond]++
				case string(CategoryRedelegate):
					principal[string(CategoryRedelegate)]++
				case string(CategoryTransfer):
					require.Equal(t, TriggerWithdrawAddress, r.RewardTrigger)
				default:
					t.Fatalf("unexpected row %s", r.Category)
				}
			}
			require.True(t, got.IsEqual(want), "booked %s, chain paid %s", got, want)
			require.Equal(t, counts, principal)
		})
	}
}

// A share of the log that cannot be proven to belong to one inner message
// refuses the whole packet instead of booking a guess.
func TestICAStakingBatchRefusesWhatItCannotPlace(t *testing.T) {
	firstOf := func(events []txlog.LogMessageEvent, kind string) int {
		for i, ev := range events {
			if ev.Type == kind {
				return i
			}
		}
		t.Fatalf("no %s event", kind)
		return -1
	}
	setAttr := func(ev *txlog.LogMessageEvent, key, value string) {
		for i := range ev.Attributes {
			if ev.Attributes[i].Key == key {
				ev.Attributes[i].Value = value
				return
			}
		}
		t.Fatalf("no %s attribute", key)
	}
	cases := map[string]struct {
		fixture string
		change  func([]txlog.LogMessageEvent) []txlog.LogMessageEvent
	}{
		"a reward the distribution module never paid": {icaDelegatesFixture, func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			i := firstOf(e, eventWithdrawRewards)
			return append(e[:i:i], e[i+1:]...)
		}},
		"a merged reward group dropped": {"withdrawals-merged", func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			i := firstOf(e, eventWithdrawRewards)
			e[i].Attributes = e[i].Attributes[2:]
			return e
		}},
		"a reward paid to another delegator": {icaDelegatesFixture, func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			setAttr(&e[firstOf(e, eventWithdrawRewards)], attributeDelegator, distributionModuleAddress)
			return e
		}},
		"an unbond from another validator": {icaUndelegatesFixture, func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			setAttr(&e[firstOf(e, eventUnbond)], attributeValidator, "cosmosvaloper1unknown")
			return e
		}},
		"a delegation of another amount": {"delegates-merged", func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			setAttr(&e[firstOf(e, string(CategoryDelegate))], attributeAmount, "1uatom")
			return e
		}},
		"an unbond larger than the message": {icaUndelegatesFixture, func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			setAttr(&e[firstOf(e, eventUnbond)], attributeAmount, "999999999999uatom")
			return e
		}},
		"a staking event no message made": {"redelegates", func(e []txlog.LogMessageEvent) []txlog.LogMessageEvent {
			extra := e[firstOf(e, string(CategoryRedelegate))]
			return append(e, extra)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fx := loadICAFixture(t, c.fixture)
			events := c.change(cloneEvents(fx.Events))
			_, err := parseICAEvents(t, fx.MessageType, events)
			var incomplete *ClassificationIncompleteError
			require.True(t, errors.As(err, &incomplete), "err = %v", err)
			require.True(t, strings.Contains(err.Error(), "inner messages share one event log"), "err = %v", err)
		})
	}
}

func cloneEvents(in []txlog.LogMessageEvent) []txlog.LogMessageEvent {
	out := make([]txlog.LogMessageEvent, len(in))
	for i, ev := range in {
		out[i] = txlog.LogMessageEvent{Type: ev.Type, Attributes: append([]txlog.Attribute(nil), ev.Attributes...)}
	}
	return out
}
