package tax

import (
	"fmt"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
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

// icaBatchMessage is one message of a batch after authz MsgExec is opened:
// granted says it ran through MsgExec, so its delegator is the granter.
type icaBatchMessage struct {
	any     *codectypes.Any
	path    string
	granted bool
}

// icaBatchMessages opens each MsgExec in the batch into its inner messages,
// one level deep, keeping message order. ok is false for a nested MsgExec or
// an Exec that does not decode.
func icaBatchMessages(msgs []*codectypes.Any) ([]icaBatchMessage, bool) {
	var out []icaBatchMessage
	for i, a := range msgs {
		path := "ica/" + strconv.Itoa(i)
		if a.TypeUrl != typeURLAuthzExec {
			out = append(out, icaBatchMessage{any: a, path: path})
			continue
		}
		inner, err := unpackAuthzInner(a)
		if err != nil {
			return nil, false
		}
		exec, ok := inner.(*authz.MsgExec)
		if !ok {
			return nil, false
		}
		for j, b := range exec.Msgs {
			if b.TypeUrl == typeURLAuthzExec {
				return nil, false
			}
			out = append(out, icaBatchMessage{any: b, path: path + "/" + strconv.Itoa(j), granted: true})
		}
	}
	return out, true
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
//
// A MsgExec in the batch is opened into its inner messages in place: the host
// runs them in that order inside the same log, and each names its granter as
// delegator, so each reward group must name its own message's delegator.
//
// SDK 0.45 does not emit withdraw_rewards for the withdrawal a delegate,
// undelegate or redelegate makes, only the distribution module's transfer, and
// it pays nothing when the pending reward rounds to zero. When a batch has no
// withdraw_rewards event and no explicit withdrawal, and the module made
// exactly one payment per validator the batch touches with no validator
// touched twice, the payments can only belong to those validators in message
// order, so they become the reward groups. Fewer payments than validators
// leave which validator paid nothing unknown, and the packet is refused.
func icaStakingBatch(msgs []*codectypes.Any, log *txlog.LogMessage, state *classificationState, depth int) (rows []TaxableEvent, applies bool) {
	batch, opened := icaBatchMessages(msgs)
	if !opened {
		return nil, false
	}
	staking := 0
	explicitWithdraw := false
	for _, m := range batch {
		switch {
		case icaStakingTypes[m.any.TypeUrl]:
			staking++
			explicitWithdraw = explicitWithdraw || m.any.TypeUrl == typeURLWithdrawDelegatorReward
		case icaBatchOtherTypes[m.any.TypeUrl]:
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

	if len(rewards) == 0 && !explicitWithdraw {
		synthesized, reason := icaPaymentRewardGroups(batch, log)
		if reason != "" {
			return refuse(reason)
		}
		rewards = synthesized
	}

	owner := ""
	ri, pi := 0, 0
	var out []TaxableEvent
	rewardRows := map[string][]TaxableEvent{}
	var owners []string
	triggers := map[string]bool{}
	for _, bm := range batch {
		a, path := bm.any, bm.path
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
		if delegator == "" {
			return refuse(path + ": no delegator")
		}
		if !bm.granted {
			if owner != "" && delegator != owner {
				return refuse(path + ": inner messages name different delegators")
			}
			owner = delegator
		}
		if _, seen := rewardRows[delegator]; !seen {
			rewardRows[delegator] = nil
			owners = append(owners, delegator)
		}

		sub := &txlog.LogMessage{}
		for _, v := range validators {
			if ri < len(rewards) && rewards[ri].attrs[attributeValidator] == v {
				g := rewards[ri].attrs
				if d := g[attributeDelegator]; d != "" && d != delegator {
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
			if !icaPrincipalMatches(principalKind, g, validators, amount, delegator) {
				return refuse(path + ": " + principalKind + " event does not carry the message's validator and amount")
			}
			sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: principalKind, Attributes: groupAttributes(g)})
			pi++
		}
		rows := classifyWithState(inner, sub, state, depth+1, path)
		for _, r := range rows {
			if r.Category == string(CategoryReward) {
				rewardRows[delegator] = append(rewardRows[delegator], r)
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
	if len(owners) == 1 {
		withdrawAddressPaid(state, log, owners[0], trigger, rewardRows[owners[0]])
		return out, true
	}
	if !distributionPaidSelf(log, rewards) {
		return refuse("granters' rewards were not all paid to the granters themselves")
	}
	return out, true
}

// distributionPaidSelf checks, for a batch whose reward groups name several
// delegators, that the distribution module paid each delegator exactly its
// own groups, so no reward went to a withdraw address that would need its own
// transfer row.
func distributionPaidSelf(log *txlog.LogMessage, rewards []icaGroup) bool {
	want := map[string]sdk.Coins{}
	for _, g := range rewards {
		d := g.attrs[attributeDelegator]
		if d == "" {
			return false
		}
		coins, err := sdk.ParseCoinsNormalized(g.attrs[attributeAmount])
		if err != nil {
			return false
		}
		want[d] = want[d].Add(coins...)
	}
	paid := map[string]sdk.Coins{}
	for _, p := range transferRuns(log, distributionModuleAddress) {
		if _, ok := want[p.recipient]; !ok {
			return false
		}
		paid[p.recipient] = paid[p.recipient].Add(p.coins...)
	}
	for d, w := range want {
		if !paid[d].IsEqual(w) {
			return false
		}
	}
	return true
}

// icaPaymentRewardGroups turns the distribution module's payments into reward
// groups for a batch whose log has no withdraw_rewards event (SDK 0.45). Every
// payment is matched to the validator the batch touches at the same position,
// which is only proven when there is one payment per touched validator, no
// validator is touched twice and one delegator owns the whole batch.
func icaPaymentRewardGroups(batch []icaBatchMessage, log *txlog.LogMessage) ([]icaGroup, string) {
	runs := transferRuns(log, distributionModuleAddress)
	if len(runs) == 0 {
		return nil, ""
	}
	var touched []string
	seen := map[string]bool{}
	owner := ""
	for _, bm := range batch {
		inner, err := unpackAuthzInner(bm.any)
		if err != nil {
			return nil, bm.path + ": " + err.Error()
		}
		var delegator string
		var validators []string
		switch m := inner.(type) {
		case *stakingtypes.MsgDelegate:
			delegator, validators = m.DelegatorAddress, []string{m.ValidatorAddress}
		case *stakingtypes.MsgUndelegate:
			delegator, validators = m.DelegatorAddress, []string{m.ValidatorAddress}
		case *stakingtypes.MsgBeginRedelegate:
			delegator, validators = m.DelegatorAddress, []string{m.ValidatorSrcAddress, m.ValidatorDstAddress}
		default:
			continue
		}
		if bm.granted || (owner != "" && delegator != owner) {
			return nil, "payments without withdraw_rewards events in a batch for several delegators"
		}
		owner = delegator
		for _, v := range validators {
			if seen[v] {
				return nil, "payments without withdraw_rewards events and a validator touched twice"
			}
			seen[v] = true
			touched = append(touched, v)
		}
	}
	if len(runs) != len(touched) {
		return nil, fmt.Sprintf("%d distribution payments without withdraw_rewards events for %d validators", len(runs), len(touched))
	}
	groups := make([]icaGroup, 0, len(runs))
	for i, r := range runs {
		if !r.coins.IsAllPositive() {
			return nil, "a distribution payment without withdraw_rewards events is not positive"
		}
		groups = append(groups, icaGroup{kind: eventWithdrawRewards, attrs: map[string]string{
			attributeValidator: touched[i], attributeDelegator: owner, attributeAmount: r.coins.String(),
		}})
	}
	return groups, ""
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
	if kind != string(CategoryDelegate) {
		// Undelegating a few base units can round to zero tokens.
		if zero, err := sdk.ParseCoinNormalized(g[attributeAmount]); err == nil && zero.Denom == amount.Denom && zero.IsZero() {
			return true
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
	// the event can carry a few base units less than the message asked for,
	// down to zero.
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
