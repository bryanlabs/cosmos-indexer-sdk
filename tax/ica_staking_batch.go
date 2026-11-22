package tax

import (
	"fmt"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

const (
	icaBatchMixedTrigger = "ica_batch"
	attrSourceValidator  = "source_validator"
	attrDestValidator    = "destination_validator"
	attrCompletionTime   = "completion_time"
)

// icaStakingTypes are the inner messages whose share of a batch's log can be
// recovered: each emits at most one reward group per validator it touches
// and one principal group carrying its own validator and amount.
var icaStakingTypes = map[string]bool{
	typeURLDelegate: true, typeURLUndelegate: true, typeURLBeginRedelegate: true, typeURLWithdrawDelegatorReward: true,
}

// icaBatchOtherTypes are booked from the message (bank sends) or from the one
// send_packet that carries exactly it (ICS-20 transfers), so they may sit in
// a staking batch without reading its staking events.
var icaBatchOtherTypes = map[string]bool{
	typeURLBankSend: true, typeURLBankMultiSend: true, typeURLIBCTransfer: true,
}

type icaGroup struct {
	kind  string
	attrs map[string]string
}

// icaStakingBatch classifies an EXECUTE_TX batch of delegations, undelegations,
// redelegations and reward withdrawals by splitting the shared log per inner
// message. The SDK runs the messages in order and each one withdraws the
// pending reward of every delegation it changes before emitting its own
// principal event, so the reward groups and the principal groups appear in
// message order whether the log keeps one event per emission or merges
// same-type events (SDK before 0.47). Each group is given to the message whose
// validator and amount it carries; anything left over, out of order, paid to
// another delegator, or not matched by the distribution module's payments
// refuses the whole packet. applies is false when the batch holds a type this
// split cannot place, which keeps the unresolved issue.
func icaStakingBatch(msgs []*codectypes.Any, log *txlog.LogMessage, state *classificationState, depth int) (rows []TaxableEvent, applies bool) {
	staking := 0
	for _, a := range msgs {
		switch {
		case icaStakingTypes[a.TypeUrl]:
			staking++
		case icaBatchOtherTypes[a.TypeUrl]:
		default:
			return nil, false
		}
	}
	if staking == 0 || log == nil {
		return nil, false
	}
	refuse := func(reason string) ([]TaxableEvent, bool) {
		state.issues = append(state.issues, fmt.Sprintf("ica host packet: %d inner messages share one event log (%s; coins moved %t): %s", len(msgs), icaTypeSummary(msgs), authzMovedCoins(log), reason))
		return nil, true
	}

	var rewards []icaGroup
	var principals []icaGroup
	for _, ev := range log.Events {
		switch ev.Type {
		case eventWithdrawRewards:
			groups, err := rewardAttributeGroups(ev)
			if err != nil {
				return refuse(err.Error())
			}
			for _, g := range groups {
				rewards = append(rewards, icaGroup{kind: ev.Type, attrs: g})
			}
		case string(CategoryDelegate), eventUnbond, string(CategoryRedelegate):
			for _, g := range splitAttributeGroups(ev) {
				principals = append(principals, icaGroup{kind: ev.Type, attrs: g})
			}
		}
	}

	owner := ""
	ri, pi := 0, 0
	var out, rewardRows []TaxableEvent
	triggers := map[string]bool{}
	for i, a := range msgs {
		path := "ica/" + strconv.Itoa(i)
		inner, err := unpackAuthzInner(a)
		if err != nil {
			return refuse(fmt.Sprintf("%s: %v", path, err))
		}
		var delegator, principalKind string
		var validators []string
		var amount sdk.Coin
		switch m := inner.(type) {
		case *stakingtypes.MsgDelegate:
			delegator, principalKind, validators, amount = m.DelegatorAddress, string(CategoryDelegate), []string{m.ValidatorAddress}, m.Amount
		case *stakingtypes.MsgUndelegate:
			delegator, principalKind, validators, amount = m.DelegatorAddress, eventUnbond, []string{m.ValidatorAddress}, m.Amount
		case *stakingtypes.MsgBeginRedelegate:
			delegator, principalKind, validators, amount = m.DelegatorAddress, string(CategoryRedelegate), []string{m.ValidatorSrcAddress, m.ValidatorDstAddress}, m.Amount
		case *disttypes.MsgWithdrawDelegatorReward:
			delegator, validators = m.DelegatorAddress, []string{m.ValidatorAddress}
		case *transfertypes.MsgTransfer:
			out = append(out, classifyWithState(inner, icaTransferPacketLog(log, m), state, depth+1, path)...)
			continue
		default:
			out = append(out, classifyWithState(inner, &txlog.LogMessage{}, state, depth+1, path)...)
			continue
		}
		if delegator == "" || (owner != "" && delegator != owner) {
			return refuse(path + ": inner messages name different delegators")
		}
		owner = delegator

		sub := &txlog.LogMessage{}
		for _, v := range validators {
			if ri < len(rewards) && rewards[ri].attrs[attributeValidator] == v {
				g := rewards[ri].attrs
				if d := g[attributeDelegator]; d != "" && d != owner {
					return refuse(path + ": reward group names another delegator")
				}
				sub.Events = append(sub.Events, groupEvent(eventWithdrawRewards, g, attributeAmount, attributeValidator, attributeDelegator))
				ri++
			}
		}
		if principalKind != "" {
			if pi >= len(principals) || principals[pi].kind != principalKind {
				return refuse(path + ": no " + principalKind + " event in message order")
			}
			g := principals[pi].attrs
			if !icaPrincipalMatches(principalKind, g, validators, amount, owner) {
				return refuse(path + ": " + principalKind + " event does not carry the message's validator and amount")
			}
			sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: principalKind, Attributes: groupAttributes(g)})
			pi++
		}
		rows := classifyWithState(inner, sub, state, depth+1, path)
		for _, r := range rows {
			if r.Category == string(CategoryReward) {
				rewardRows = append(rewardRows, r)
				triggers[r.RewardTrigger] = true
			}
		}
		out = append(out, rows...)
	}
	if ri != len(rewards) {
		return refuse(fmt.Sprintf("%d of %d reward groups match no inner message", len(rewards)-ri, len(rewards)))
	}
	if pi != len(principals) {
		return refuse(fmt.Sprintf("%d of %d staking events match no inner message", len(principals)-pi, len(principals)))
	}
	if !distributionPaidRewards(log, rewards) {
		return refuse("distribution payments differ from the reward groups")
	}
	trigger := icaBatchMixedTrigger
	if len(triggers) == 1 {
		for t := range triggers {
			trigger = t
		}
	}
	withdrawAddressPaid(state, log, owner, trigger, rewardRows)
	return out, true
}

// splitAttributeGroups splits a staking event into one attribute group per
// emission: SDK 0.45 logs merge same-type events, so a key seen again starts
// the next group. Message indexes are not part of a group.
func splitAttributeGroups(ev txlog.LogMessageEvent) []map[string]string {
	var groups []map[string]string
	cur := map[string]string{}
	for _, a := range ev.Attributes {
		if a.Key == "msg_index" || a.Key == "authz_msg_index" {
			continue
		}
		if _, seen := cur[a.Key]; seen {
			groups = append(groups, cur)
			cur = map[string]string{}
		}
		cur[a.Key] = a.Value
	}
	if len(cur) != 0 {
		groups = append(groups, cur)
	}
	return groups
}

func icaPrincipalMatches(kind string, g map[string]string, validators []string, amount sdk.Coin, owner string) bool {
	if d := g[attributeDelegator]; d != "" && d != owner {
		return false
	}
	switch kind {
	case string(CategoryRedelegate):
		if g[attrSourceValidator] != validators[0] || g[attrDestValidator] != validators[1] {
			return false
		}
	default:
		if g[attributeValidator] != validators[0] {
			return false
		}
	}
	coins, err := stakingCoins(g[attributeAmount], amount.Denom)
	if err != nil || len(coins) != 1 || coins[0].Denom != amount.Denom {
		return false
	}
	if kind == string(CategoryDelegate) {
		return coins[0].Amount.Equal(amount.Amount)
	}
	// Undelegating and redelegating convert the amount to shares and back, so
	// the event can carry a few base units less than the message asked for.
	return coins[0].Amount.IsPositive() && coins[0].Amount.LTE(amount.Amount)
}

// distributionPaidRewards checks that the distribution module paid out
// exactly the reward groups, denom by denom, so no withdrawal is booked twice
// or missed.
func distributionPaidRewards(log *txlog.LogMessage, rewards []icaGroup) bool {
	want := sdk.NewCoins()
	for _, g := range rewards {
		coins, err := sdk.ParseCoinsNormalized(g.attrs[attributeAmount])
		if err != nil {
			return false
		}
		want = want.Add(coins...)
	}
	paid := sdk.NewCoins()
	for _, p := range transferRuns(log, distributionModuleAddress) {
		paid = paid.Add(p.coins...)
	}
	return paid.IsEqual(want)
}

func groupEvent(kind string, g map[string]string, keys ...string) txlog.LogMessageEvent {
	ev := txlog.LogMessageEvent{Type: kind}
	for _, k := range keys {
		if v, ok := g[k]; ok {
			ev.Attributes = append(ev.Attributes, txlog.Attribute{Key: k, Value: v})
		}
	}
	return ev
}

func groupAttributes(g map[string]string) []txlog.Attribute {
	keys := []string{attributeValidator, attrSourceValidator, attrDestValidator, attributeDelegator, attributeAmount, "new_shares", attrCompletionTime}
	var out []txlog.Attribute
	for _, k := range keys {
		if v, ok := g[k]; ok {
			out = append(out, txlog.Attribute{Key: k, Value: v})
		}
	}
	return out
}
