package tax

import (
	"fmt"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// A staking handler may auto-withdraw before closing its principal operation.
// The legacy message trace proves its transfer slice; reconcile the ordered
// distribution withdrawals to that slice rather than reusing all rewards for
// every delegation. A positive withdrawal with no bank payment is unresolved;
// an explicit zero record is retained without inventing a bank movement.
func icaLegacyStakingSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	principal := &txlog.LogMessage{}
	var rewards []map[string]string
	var rewardAttrs [][]txlog.Attribute
	for _, inner := range inners {
		switch inner.(type) {
		case *stakingtypes.MsgDelegate, *stakingtypes.MsgUndelegate, *stakingtypes.MsgBeginRedelegate, *banktypes.MsgSend:
		default:
			return nil, fmt.Errorf("not a staking/send batch")
		}
	}
	for _, ev := range log.Events {
		if ev.Type != eventWithdrawRewards {
			principal.Events = append(principal.Events, ev)
			continue
		}
		groups, err := rewardAttributeGroups(ev)
		if err != nil {
			return nil, err
		}
		attrs, err := legacyRecords(ev.Attributes, attributeAmount)
		if err != nil || len(attrs) != len(groups) {
			return nil, fmt.Errorf("malformed withdrawal groups")
		}
		rewards = append(rewards, groups...)
		rewardAttrs = append(rewardAttrs, attrs...)
	}
	subs, err := legacyAuthzSubLogs(inners, principal)
	if err != nil {
		return nil, err
	}
	next := 0
	for i, sub := range subs {
		var who string
		validators := map[string]bool{}
		switch m := inners[i].(type) {
		case *stakingtypes.MsgDelegate:
			who = m.DelegatorAddress
			validators[m.ValidatorAddress] = true
		case *stakingtypes.MsgUndelegate:
			who = m.DelegatorAddress
			validators[m.ValidatorAddress] = true
		case *stakingtypes.MsgBeginRedelegate:
			who = m.DelegatorAddress
			validators[m.ValidatorSrcAddress] = true
			validators[m.ValidatorDstAddress] = true
		}
		used := map[string]bool{}
		consumeZero := func() {
			for next < len(rewards) {
				r := rewards[next]
				coins, err := sdk.ParseCoinsNormalized(r[attributeAmount])
				if err != nil || !coins.IsZero() || !validators[r[attributeValidator]] || used[r[attributeValidator]] || r[attributeDelegator] != "" && r[attributeDelegator] != who {
					return
				}
				used[r[attributeValidator]] = true
				sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventWithdrawRewards, Attributes: rewardAttrs[next]})
				next++
			}
		}
		for _, p := range transferRuns(sub, distributionModuleAddress) {
			consumeZero()
			if next == len(rewards) || who == "" {
				return nil, fmt.Errorf("unmatched distribution transfer")
			}
			r := rewards[next]
			coins, err := sdk.ParseCoinsNormalized(r[attributeAmount])
			if err != nil || !coins.IsEqual(p.coins) || !validators[r[attributeValidator]] || used[r[attributeValidator]] || r[attributeDelegator] != "" && r[attributeDelegator] != who {
				return nil, fmt.Errorf("staking withdrawal mismatch")
			}
			used[r[attributeValidator]] = true
			sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventWithdrawRewards, Attributes: rewardAttrs[next]})
			next++
		}
		consumeZero()
	}
	if next != len(rewards) {
		return nil, fmt.Errorf("unassigned staking withdrawals")
	}
	return subs, nil
}
