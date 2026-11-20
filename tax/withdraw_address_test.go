package tax

import (
	"os"
	"reflect"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

const (
	withdrawTo      = "cosmos1withdrawaddrxxxxxxxxxxxxxxxxxxxxxxx"
	withdrawToOther = "cosmos1withdrawotherxxxxxxxxxxxxxxxxxxxxxx"
	otherDelegator  = "cosmos1seconddelegatorxxxxxxxxxxxxxxxxxxxx"
	otherValidator  = "cosmosvaloper1secondvalidatorxxxxxxxxxxxxxx"
)

var milliAtom = [2]string{attributeAmount, "1000uatom"}

const attributeModule = "module"

func distTransfer(to, amount string, extra ...[2]string) txlog.LogMessageEvent {
	attrs := append([][2]string{{attributeRecipient, to}, {attributeSender, distributionModuleAddress}, {attributeAmount, amount}}, extra...)
	return ev(string(CategoryTransfer), attrs...)
}

func withdrawRewards(delegator, validator, amount string, extra ...[2]string) txlog.LogMessageEvent {
	attrs := append([][2]string{{attributeAmount, amount}, {attributeValidator, validator}, {attributeDelegator, delegator}}, extra...)
	return ev(eventWithdrawRewards, attrs...)
}

func withoutWithdrawAddress(events []TaxableEvent) []TaxableEvent {
	var out []TaxableEvent
	for _, e := range events {
		if e.RewardTrigger != TriggerWithdrawAddress {
			out = append(out, e)
		}
	}
	return out
}

// assertMovedLast checks that the withdraw-address rows are exactly want and
// come after every other row.
func assertMovedLast(t *testing.T, out []TaxableEvent, want []TaxableEvent) {
	t.Helper()
	n := len(out) - len(want)
	if n < 0 {
		t.Fatalf("%d rows, want %d moves: %+v", len(out), len(want), out)
	}
	for i, e := range out[:n] {
		if e.RewardTrigger == TriggerWithdrawAddress {
			t.Fatalf("move at %d before other rows: %+v", i, out)
		}
	}
	if !reflect.DeepEqual(out[n:], want) {
		t.Fatalf("moves %+v, want %+v", out[n:], want)
	}
}

func moved(from, to, amount, denom, trigger string) TaxableEvent {
	return TaxableEvent{
		Category: string(CategoryTransfer), FromAddr: from, ToAddr: to, Amount: amount, Denom: denom,
		RewardTrigger: TriggerWithdrawAddress, Detail: `{"protocol":"distribution","reward_trigger":"` + trigger + `"}`,
	}
}

// Every reward-bearing message moves its reward on to the withdraw address,
// after its own rows, which stay exactly as an unredirected payout leaves them.
func TestWithdrawAddressEveryRewardTrigger(t *testing.T) {
	coin := sdk.NewInt64Coin(fixtureNativeDenom, 1000)
	for _, tc := range []struct {
		trigger string
		msg     sdk.Msg
		extra   []txlog.LogMessageEvent
	}{
		{actionClaim, &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}, nil},
		{
			string(CategoryDelegate), &stakingtypes.MsgDelegate{DelegatorAddress: del, ValidatorAddress: val, Amount: coin},
			[]txlog.LogMessageEvent{ev(string(CategoryDelegate), [2]string{attributeValidator, val}, milliAtom)},
		},
		{
			"undelegate", &stakingtypes.MsgUndelegate{DelegatorAddress: del, ValidatorAddress: val, Amount: coin},
			[]txlog.LogMessageEvent{ev(eventUnbond, [2]string{attributeValidator, val}, milliAtom)},
		},
		{
			string(CategoryRedelegate), &stakingtypes.MsgBeginRedelegate{DelegatorAddress: del, ValidatorSrcAddress: val, ValidatorDstAddress: otherValidator, Amount: coin},
			[]txlog.LogMessageEvent{ev(string(CategoryRedelegate), [2]string{"source_validator", val}, [2]string{"destination_validator", otherValidator}, milliAtom)},
		},
		{
			"cancel_unbonding", &stakingtypes.MsgCancelUnbondingDelegation{DelegatorAddress: del, ValidatorAddress: val, Amount: coin},
			[]txlog.LogMessageEvent{ev("cancel_unbonding_delegation", [2]string{attributeValidator, val}, milliAtom)},
		},
		{
			"tokenize", &MsgTokenizeShares{DelegatorAddress: del, ValidatorAddress: val},
			[]txlog.LogMessageEvent{ev("tokenize_shares", [2]string{attributeDelegator, del}, [2]string{attributeValidator, val}, milliAtom, [2]string{"share_record_id", "7"})},
		},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			build := func(to string) *txlog.LogMessage {
				events := []txlog.LogMessageEvent{
					ev(eventCoinReceived, [2]string{attributeReceiver, to}, [2]string{attributeAmount, fixtureHalfMilliAtom}),
					distTransfer(to, fixtureHalfMilliAtom),
					withdrawRewards(del, val, fixtureHalfMilliAtom),
				}
				return &txlog.LogMessage{Events: append(events, tc.extra...)}
			}
			plain := classify(tc.msg, build(del))
			if len(withoutWithdrawAddress(plain)) != len(plain) {
				t.Fatalf("unredirected payout moved coins: %+v", plain)
			}
			out := classify(tc.msg, build(withdrawTo))
			assertMovedLast(t, out, []TaxableEvent{moved(del, withdrawTo, "500", fixtureNativeDenom, tc.trigger)})
			if !reflect.DeepEqual(out[:len(out)-1], plain) {
				t.Fatalf("existing rows changed:\n%+v\n%+v", out[:len(out)-1], plain)
			}
		})
	}
}

// A payout to the delegator itself moves nothing.
func TestWithdrawAddressUnsetAddsNothing(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	out := classify(msg, &txlog.LogMessage{Events: []txlog.LogMessageEvent{distTransfer(del, fixtureHalfMilliAtom), withdrawRewards(del, val, fixtureHalfMilliAtom)}})
	if len(out) != 1 || out[0].Category != string(CategoryReward) {
		t.Fatalf("%+v", out)
	}
}

// What moves is capped per denom at the booked reward, so a distribution
// payment that is not this delegator's reward never moves its coins, and a
// denom with no reward moves nothing.
func TestWithdrawAddressCappedAtBookedReward(t *testing.T) {
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		distTransfer(withdrawTo, "300uatom,9uosmo"),
		distTransfer(withdrawToOther, "800uatom"),
		withdrawRewards(del, val, fixtureHalfMilliAtom),
	}}
	assertMovedLast(t, classify(msg, log), []TaxableEvent{
		moved(del, withdrawTo, "300", fixtureNativeDenom, actionClaim),
		moved(del, withdrawToOther, "200", fixtureNativeDenom, actionClaim),
	})
	// No reward booked: nothing to move.
	none := classify(msg, &txlog.LogMessage{Events: []txlog.LogMessageEvent{distTransfer(withdrawTo, fixtureHalfMilliAtom)}})
	if len(none) != 0 {
		t.Fatalf("moved coins with no reward: %+v", none)
	}
}

// A redelegation that withdrew from both validators moves the sum to the one
// withdraw address in a single row.
func TestWithdrawAddressRedelegateTwoWithdrawals(t *testing.T) {
	msg := &stakingtypes.MsgBeginRedelegate{DelegatorAddress: del, ValidatorSrcAddress: val, ValidatorDstAddress: otherValidator, Amount: sdk.NewInt64Coin(fixtureNativeDenom, 1000)}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		distTransfer(withdrawTo, fixtureQuarterMilliAtom),
		withdrawRewards(del, val, fixtureQuarterMilliAtom),
		distTransfer(withdrawTo, fixtureTenthMilliAtom),
		withdrawRewards(del, otherValidator, fixtureTenthMilliAtom),
		ev(string(CategoryRedelegate), [2]string{"source_validator", val}, [2]string{"destination_validator", otherValidator}, milliAtom),
	}}
	assertMovedLast(t, classify(msg, log), []TaxableEvent{moved(del, withdrawTo, "350", fixtureNativeDenom, "redelegate")})
}

// Before the SDK emitted withdraw_rewards for a staking message's automatic
// withdrawal the distribution module's transfer is the only record. The
// reward is read from it, and the part paid to another account moves there.
func TestWithdrawAddressTransferOnlyLog(t *testing.T) {
	msg := &stakingtypes.MsgUndelegate{DelegatorAddress: del, ValidatorAddress: val, Amount: sdk.NewInt64Coin(fixtureNativeDenom, 1000)}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, withdrawTo}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, fixtureStakingReward},
			[2]string{attributeRecipient, "cosmos1bondedpoolxxxxxxxxxxxxxxxxxxxxxxxxxxx"}, [2]string{attributeSender, "cosmos1notbondedxxxxxxxxxxxxxxxxxxxxxxxxxxx"}, milliAtom),
		ev(eventUnbond, [2]string{attributeValidator, val}, [2]string{attributeAmount, "1000"}),
	}}
	out := classify(msg, log)
	rewards := byCategory(out, string(CategoryReward))
	if len(rewards) != 1 || rewards[0].Amount != "350" || rewards[0].ToAddr != del {
		t.Fatalf("rewards %+v", rewards)
	}
	assertMovedLast(t, out, []TaxableEvent{moved(del, withdrawTo, "350", fixtureNativeDenom, "undelegate")})
}

// One Exec for two granters with authz_msg_index: each granter moves its own
// reward to its own withdraw address, after every row of the Exec.
func TestWithdrawAddressAuthzTaggedPerGranter(t *testing.T) {
	inner := []sdk.Msg{
		&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val},
		&stakingtypes.MsgDelegate{DelegatorAddress: otherDelegator, ValidatorAddress: val, Amount: sdk.NewInt64Coin(fixtureNativeDenom, 1000)},
	}
	exec := authz.NewMsgExec(sdk.AccAddress("grantee"), inner)
	i0, i1 := [2]string{attributeAuthzMsgIndex, "0"}, [2]string{attributeAuthzMsgIndex, "1"}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		distTransfer(withdrawTo, fixtureHalfMilliAtom, i0),
		withdrawRewards(del, val, fixtureHalfMilliAtom, i0),
		distTransfer(withdrawToOther, fixtureTenthMilliAtom, i1),
		withdrawRewards(otherDelegator, val, fixtureTenthMilliAtom, i1),
		ev(string(CategoryDelegate), [2]string{attributeValidator, val}, milliAtom, i1),
	}}
	out := parseLegacy(t, &exec, log)
	if got := withoutWithdrawAddress(out); len(got) != 3 || got[0].ToAddr != del || got[1].ToAddr != otherDelegator || got[2].Category != string(CategoryDelegate) {
		t.Fatalf("existing rows %+v", got)
	}
	assertMovedLast(t, out, []TaxableEvent{
		moved(del, withdrawTo, "500", fixtureNativeDenom, actionClaim),
		moved(otherDelegator, withdrawToOther, "100", fixtureNativeDenom, "delegate"),
	})
}

// The same for an untagged SDK 0.45 log, where the raw log's order gives each
// granter its own distribution transfer. Rewards are never lumped together.
func TestWithdrawAddressAuthzUntaggedPerGranter(t *testing.T) {
	inner := []sdk.Msg{
		&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val},
		&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: otherDelegator, ValidatorAddress: otherValidator},
	}
	exec := authz.NewMsgExec(sdk.AccAddress("grantee"), inner)
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev(eventMessage,
			[2]string{attributeAction, "/cosmos.authz.v1beta1.MsgExec"},
			[2]string{attributeSender, distributionModuleAddress}, [2]string{attributeModule, moduleDistribution}, [2]string{attributeSender, del},
			[2]string{attributeSender, distributionModuleAddress}, [2]string{attributeModule, moduleDistribution}, [2]string{attributeSender, otherDelegator}),
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, withdrawTo}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, fixtureHalfMilliAtom},
			[2]string{attributeRecipient, withdrawToOther}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, fixtureTenthMilliAtom}),
		ev(eventWithdrawRewards,
			[2]string{attributeAmount, fixtureHalfMilliAtom}, [2]string{attributeValidator, val},
			[2]string{attributeAmount, fixtureTenthMilliAtom}, [2]string{attributeValidator, otherValidator}),
	}}
	out := parseLegacy(t, &exec, log)
	if got := withoutWithdrawAddress(out); len(got) != 2 || got[0].ToAddr != del || got[0].Amount != "500" || got[1].ToAddr != otherDelegator || got[1].Amount != "100" {
		t.Fatalf("existing rows %+v", got)
	}
	assertMovedLast(t, out, []TaxableEvent{
		moved(del, withdrawTo, "500", fixtureNativeDenom, actionClaim),
		moved(otherDelegator, withdrawToOther, "100", fixtureNativeDenom, actionClaim),
	})
}

// A wasm execute's supplement rows stay where they were: withdraw-address
// moves go after them.
func TestWithdrawAddressAfterLateRows(t *testing.T) {
	state := &classificationState{late: []TaxableEvent{{Category: string(CategoryTransfer), Amount: "1", Denom: fixtureNativeDenom}}}
	msg := &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	out := classifyWithState(msg, &txlog.LogMessage{Events: []txlog.LogMessageEvent{distTransfer(withdrawTo, fixtureHalfMilliAtom), withdrawRewards(del, val, fixtureHalfMilliAtom)}}, state, 0, "")
	if len(out) != 3 || out[1].Amount != "1" {
		t.Fatalf("%+v", out)
	}
	assertMovedLast(t, out, []TaxableEvent{moved(del, withdrawTo, "500", fixtureNativeDenom, actionClaim)})
}

// On the real archive fixtures, the moves add only rows after the existing
// ones, and move exactly what the chain paid to other accounts.
func TestWithdrawAddressArchiveFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata/hub-legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		msgs, logs := hubLegacyFixture(t, name)
		for i := range msgs {
			out := parseLegacy(t, msgs[i], logs[i])
			n := len(withoutWithdrawAddress(out))
			for j, r := range out {
				if (j < n) == (r.RewardTrigger == TriggerWithdrawAddress) {
					t.Fatalf("%s msg %d: move out of place at %d", name, i, j)
				}
			}
			paid := sdkmath.ZeroInt()
			for _, r := range out[n:] {
				amount, _ := sdkmath.NewIntFromString(r.Amount)
				paid = paid.Add(amount)
			}
			if !paid.IsZero() {
				t.Logf("%s msg %d: %s moved to withdraw addresses", name, i, paid)
			}
		}
	}
}
