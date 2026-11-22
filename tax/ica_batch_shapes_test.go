package tax

import (
	"errors"
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
)

func distributionPaid(events []txlog.LogMessageEvent) sdk.Coins {
	paid := sdk.NewCoins()
	for _, r := range transferRuns(&txlog.LogMessage{Events: events}, distributionModuleAddress) {
		paid = paid.Add(r.coins...)
	}
	return paid
}

func rewardTotal(t *testing.T, rows []TaxableEvent) sdk.Coins {
	t.Helper()
	got := sdk.NewCoins()
	for _, r := range rows {
		if r.Category != string(CategoryReward) {
			continue
		}
		require.NotEmpty(t, r.ValidatorAddress)
		amount, ok := sdk.NewIntFromString(r.Amount)
		require.True(t, ok)
		got = got.Add(sdk.NewCoin(r.Denom, amount))
	}
	return got
}

func countCategory(rows []TaxableEvent, category Category) int {
	n := 0
	for _, r := range rows {
		if r.Category == string(category) {
			n++
		}
	}
	return n
}

func requireBatchRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var incomplete *ClassificationIncompleteError
	require.True(t, errors.As(err, &incomplete), "err = %v", err)
	require.True(t, strings.Contains(err.Error(), reason), "err = %v", err)
}

// SDK 0.45 logs carry no withdraw_rewards event for the withdrawals a
// delegate makes, only the distribution module's payments. With one payment
// per validator the batch touches, every payment is booked as that
// validator's reward to the host account.
func TestICAStakingBatchBooksPaymentsWithoutWithdrawEvents(t *testing.T) {
	fx := loadICAFixture(t, "delegates-merged-no-withdraw-events")
	_, stored := storedGroups(t, fx.Events)
	require.True(t, stored.IsZero())
	rows, err := parseICAEvents(t, fx.MessageType, fx.Events)
	require.NoError(t, err)
	paid := distributionPaid(fx.Events)
	require.False(t, paid.IsZero())
	require.True(t, rewardTotal(t, rows).IsEqual(paid), "booked %s, chain paid %s", rewardTotal(t, rows), paid)
	require.Equal(t, 10, countCategory(rows, CategoryDelegate))
	require.Equal(t, 10, countCategory(rows, CategoryReward))
}

// Without withdraw_rewards events, fewer payments than validators, or a
// validator touched twice, leave the payments' owners unproven.
func TestICAStakingBatchRefusesUnplacedPayments(t *testing.T) {
	for name, reason := range map[string]string{
		"delegates-merged-missing-payment":      "distribution payments without withdraw_rewards events for 62 validators",
		"redelegates-merged-no-withdraw-events": "a validator touched twice",
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadICAFixture(t, name)
			_, err := parseICAEvents(t, fx.MessageType, fx.Events)
			requireBatchRefused(t, err, reason)
		})
	}
}

// A MsgExec in a batch is opened in place. Its delegations and withdrawals
// are booked like the host's own, each reward to the delegator its group
// names, and the batch's bank send is booked from the message.
func TestICAStakingBatchOpensAuthzExec(t *testing.T) {
	for _, name := range []string{"authz-send", "authz-delegates"} {
		t.Run(name, func(t *testing.T) {
			fx := loadICAFixture(t, name)
			_, stored := storedGroups(t, fx.Events)
			require.False(t, stored.IsZero())
			rows, err := parseICAEvents(t, fx.MessageType, fx.Events)
			require.NoError(t, err)
			require.True(t, rewardTotal(t, rows).IsEqual(stored), "booked %s, chain paid %s", rewardTotal(t, rows), stored)
			delegators := map[string]bool{}
			for _, ev := range fx.Events {
				if ev.Type != eventWithdrawRewards {
					continue
				}
				groups, err := rewardAttributeGroups(ev)
				require.NoError(t, err)
				for _, g := range groups {
					delegators[g[attributeDelegator]] = true
				}
			}
			for _, r := range rows {
				if r.Category == string(CategoryReward) {
					require.True(t, delegators[r.ToAddr], "reward booked to a delegator no group names")
				}
			}
		})
	}
}

// A single MsgExec whose only effect is a zero withdrawal moves no coins and
// books nothing.
func TestICAExecZeroWithdrawBooksNothing(t *testing.T) {
	fx := loadICAFixture(t, "authz-zero-withdraw")
	rows, err := parseICAEvents(t, fx.MessageType, fx.Events)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// LSM redeem and tokenize batches with one event per emission are cut after
// each message's closing event, and each part is booked as a single message.
func TestICALiquidBatchSplitsAtClosingEvents(t *testing.T) {
	for name, category := range map[string]Category{"redeems": CategoryRedeemShares, "tokenizes": CategoryTokenizeShares} {
		t.Run(name, func(t *testing.T) {
			fx := loadICAFixture(t, name)
			_, stored := storedGroups(t, fx.Events)
			require.False(t, stored.IsZero())
			rows, err := parseICAEvents(t, fx.MessageType, fx.Events)
			require.NoError(t, err)
			require.True(t, rewardTotal(t, rows).IsEqual(stored), "booked %s, chain paid %s", rewardTotal(t, rows), stored)
			closing := 0
			for _, ev := range fx.Events {
				if ev.Type == string(category) {
					closing++
				}
			}
			require.Positive(t, closing)
			require.Equal(t, closing, countCategory(rows, category))
		})
	}
}

// A merged log has one closing event for several messages, so its parts
// cannot be told apart and the packet is refused.
func TestICALiquidBatchRefusesMergedLogs(t *testing.T) {
	fx := loadICAFixture(t, "redeems-merged")
	_, err := parseICAEvents(t, fx.MessageType, fx.Events)
	requireBatchRefused(t, err, "coins move after the last message's closing event")
}

// Undelegating a few base units can round to zero tokens; the unbond event
// still names the message's validator, so the batch is booked.
func TestICAStakingBatchAcceptsZeroTokenUnbond(t *testing.T) {
	fx := loadICAFixture(t, icaUndelegatesFixture)
	events := cloneEvents(fx.Events)
	for i := range events {
		if events[i].Type != eventUnbond {
			continue
		}
		for j := range events[i].Attributes {
			if events[i].Attributes[j].Key == attributeAmount {
				events[i].Attributes[j].Value = sdk.NewCoin("uatom", sdk.ZeroInt()).String()
			}
		}
		break
	}
	_, err := parseICAEvents(t, fx.MessageType, events)
	require.NoError(t, err)
}
