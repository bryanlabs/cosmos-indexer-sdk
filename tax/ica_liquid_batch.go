package tax

import (
	"fmt"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
)

const (
	eventRedeemShares                = "redeem_shares"
	eventTokenizeShares              = "tokenize_shares"
	eventWithdrawTokenizeShareReward = "withdraw_tokenize_share_reward"
)

// icaLiquidTerminal names, for each LSM message an ICA batch may hold, the
// event the module emits last for that message.
var icaLiquidTerminal = map[string]string{
	TypeURLRedeemTokensForShares:       eventRedeemShares,
	LegacyTypeURLRedeemTokensForShares: eventRedeemShares,
	TypeURLTokenizeShares:              eventTokenizeShares,
	LegacyTypeURLTokenizeShares:        eventTokenizeShares,
}

// icaLiquidMoneyEvents are the events that carry coins or rewards, which must
// all fall inside some message's part of the log.
var icaLiquidMoneyEvents = map[string]bool{
	string(CategoryTransfer): true, eventCoinReceived: true, eventCoinSpent: true, eventBurn: true, actionMint: true,
	eventWithdrawRewards: true, eventWithdrawTokenizeShareReward: true, string(CategoryDelegate): true,
	eventUnbond: true, string(CategoryRedelegate): true, eventRedeemShares: true, eventTokenizeShares: true,
}

// icaLiquidBatch classifies an EXECUTE_TX batch made only of LSM redeem or
// tokenize messages. Logs that keep one event per emission (SDK 0.47 and
// later) end each message's events with its own redeem_shares or
// tokenize_shares event, and the host runs the messages in order, so the log
// is cut after each of those events and each part goes through the
// single-message path. The packet is refused when the parts and the messages
// differ in number or kind, a part's closing event names another delegator,
// or coins move after the last part. Logs that merge same-type events have
// one closing event for several messages and are refused.
func icaLiquidBatch(msgs []*codectypes.Any, log *txlog.LogMessage, state *classificationState, depth int) (rows []TaxableEvent, applies bool) {
	if len(msgs) < 2 || log == nil {
		return nil, false
	}
	for _, a := range msgs {
		if icaLiquidTerminal[a.TypeUrl] == "" {
			return nil, false
		}
	}
	refuse := func(reason string) ([]TaxableEvent, bool) {
		state.issues = append(state.issues, fmt.Sprintf("ica host packet: %d inner messages share one event log (%s; coins moved %t): %s", len(msgs), icaTypeSummary(msgs), authzMovedCoins(log), reason))
		return nil, true
	}
	var parts []*txlog.LogMessage
	cur := &txlog.LogMessage{}
	for _, ev := range log.Events {
		cur.Events = append(cur.Events, ev)
		if ev.Type == eventRedeemShares || ev.Type == eventTokenizeShares {
			parts = append(parts, cur)
			cur = &txlog.LogMessage{}
		}
	}
	for _, ev := range cur.Events {
		if icaLiquidMoneyEvents[ev.Type] {
			return refuse("coins move after the last message's closing event")
		}
	}
	if len(parts) != len(msgs) {
		return refuse(fmt.Sprintf("%d closing events for %d messages", len(parts), len(msgs)))
	}
	var out []TaxableEvent
	for i, a := range msgs {
		path := "ica/" + strconv.Itoa(i)
		part := parts[i]
		closing := part.Events[len(part.Events)-1]
		if closing.Type != icaLiquidTerminal[a.TypeUrl] {
			return refuse(path + ": closing event is " + closing.Type)
		}
		groups := splitAttributeGroups(closing)
		if len(groups) != 1 {
			return refuse(path + ": closing event merges several messages")
		}
		inner, err := unpackAuthzInner(a)
		if err != nil {
			return refuse(fmt.Sprintf("%s: %v", path, err))
		}
		delegator := ""
		switch m := inner.(type) {
		case *MsgRedeemTokensForShares:
			delegator = m.DelegatorAddress
		case *LegacyMsgRedeemTokensForShares:
			delegator = m.DelegatorAddress
		case *MsgTokenizeShares:
			delegator = m.DelegatorAddress
		case *LegacyMsgTokenizeShares:
			delegator = m.DelegatorAddress
		}
		if d := groups[0][attributeDelegator]; delegator == "" || (d != "" && d != delegator) {
			return refuse(path + ": closing event names another delegator")
		}
		out = append(out, classifyWithState(inner, part, state, depth+1, path)...)
	}
	return out, true
}
