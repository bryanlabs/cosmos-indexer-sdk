package tax

import (
	"fmt"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// legacyAuthzSubLogs separates the raw log of a MsgExec whose SDK tagged no
// event with its inner message (the Hub on SDK 0.44 and 0.45 until about
// 2023, where REStake ran one Exec for many granters). The SDK runs the inner
// messages in order, every bank transfer adds a message{sender} beside its
// transfer record, and each staking or distribution handler closes with
// message{module, sender}. So the merged message event is an ordered trace
// that gives each inner message its transfers, and the k-th delegate, unbond,
// redelegate or withdraw_rewards record belongs to the k-th inner message of
// that kind. Every assignment is checked against the inner message's signer
// and validator; anything that does not fit is an error, never a guess.
// coin_spent and coin_received are left out: no rule for these inner
// messages reads them.
func legacyAuthzSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	merged := map[string][]txlog.Attribute{}
	for _, ev := range log.Events {
		switch ev.Type {
		case eventMessage, string(CategoryTransfer), eventCoinSpent, eventCoinReceived, string(CategoryDelegate), eventUnbond, string(CategoryRedelegate), eventWithdrawRewards:
			merged[ev.Type] = append(merged[ev.Type], ev.Attributes...)
		default:
			return nil, fmt.Errorf("event %s", ev.Type)
		}
	}

	type segment struct {
		module, signer string
		senders        []string
	}
	var segments []segment
	cur, module := segment{}, ""
	for _, a := range merged[eventMessage] {
		switch a.Key {
		case attributeAction:
		case "module":
			if module != "" {
				return nil, fmt.Errorf("message event module %s has no sender", module)
			}
			module = a.Value
		case attributeSender:
			if module == "" {
				cur.senders = append(cur.senders, a.Value)
				continue
			}
			cur.module, cur.signer = module, a.Value
			segments = append(segments, cur)
			cur, module = segment{}, ""
		default:
			return nil, fmt.Errorf("message attribute %s", a.Key)
		}
	}
	if module != "" || len(cur.senders) != 0 {
		return nil, fmt.Errorf("message event ends inside an inner message")
	}
	if len(segments) != len(inners) {
		return nil, fmt.Errorf("%d handlers closed for %d inner messages", len(segments), len(inners))
	}

	transfers, err := legacyRecords(merged[string(CategoryTransfer)], attributeRecipient)
	if err != nil {
		return nil, err
	}
	subs := make([]*txlog.LogMessage, len(inners))
	next := 0
	kinds := map[string][][]txlog.Attribute{}
	for _, kind := range []string{string(CategoryDelegate), eventUnbond, string(CategoryRedelegate), eventWithdrawRewards} {
		if kinds[kind], err = legacyRecords(merged[kind], ""); err != nil {
			return nil, err
		}
	}
	used := map[string]int{}
	for i, inner := range inners {
		var wantModule, signer, kind, validator string
		switch m := inner.(type) {
		case *stakingtypes.MsgDelegate:
			wantModule, signer, kind, validator = moduleStaking, m.DelegatorAddress, string(CategoryDelegate), m.ValidatorAddress
		case *stakingtypes.MsgUndelegate:
			wantModule, signer, kind, validator = moduleStaking, m.DelegatorAddress, eventUnbond, m.ValidatorAddress
		case *stakingtypes.MsgBeginRedelegate:
			wantModule, signer, kind, validator = moduleStaking, m.DelegatorAddress, string(CategoryRedelegate), m.ValidatorSrcAddress
		case *disttypes.MsgWithdrawDelegatorReward:
			wantModule, signer, kind, validator = "distribution", m.DelegatorAddress, eventWithdrawRewards, m.ValidatorAddress
		default:
			return nil, fmt.Errorf("inner %d is %T", i, inner)
		}
		seg := segments[i]
		if seg.module != wantModule || seg.signer != signer {
			return nil, fmt.Errorf("inner %d handler is %s for another signer", i, seg.module)
		}
		sub := &txlog.LogMessage{MessageIndex: i}
		msgAttrs := make([]txlog.Attribute, 0, len(seg.senders)+2)
		var transferAttrs []txlog.Attribute
		for _, sender := range seg.senders {
			if next >= len(transfers) || attrValue(transfers[next], attributeSender) != sender {
				return nil, fmt.Errorf("inner %d transfer from %s not in the transfer event", i, sender)
			}
			msgAttrs = append(msgAttrs, txlog.Attribute{Key: attributeSender, Value: sender})
			transferAttrs = append(transferAttrs, transfers[next]...)
			next++
		}
		msgAttrs = append(msgAttrs, txlog.Attribute{Key: "module", Value: seg.module}, txlog.Attribute{Key: attributeSender, Value: seg.signer})
		sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventMessage, Attributes: msgAttrs})
		if len(transferAttrs) != 0 {
			sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: string(CategoryTransfer), Attributes: transferAttrs})
		}
		k := used[kind]
		if k >= len(kinds[kind]) {
			return nil, fmt.Errorf("inner %d has no %s record", i, kind)
		}
		rec := kinds[kind][k]
		used[kind]++
		recValidator := attrValue(rec, attributeValidator)
		if kind == string(CategoryRedelegate) {
			recValidator = attrValue(rec, "source_validator")
		}
		if recValidator != validator {
			return nil, fmt.Errorf("inner %d %s record names another validator", i, kind)
		}
		sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: kind, Attributes: rec})
		subs[i] = sub
	}
	if next != len(transfers) {
		return nil, fmt.Errorf("%d transfers belong to no inner message", len(transfers)-next)
	}
	for kind, recs := range kinds {
		if used[kind] != len(recs) {
			return nil, fmt.Errorf("%d %s records belong to no inner message", len(recs)-used[kind], kind)
		}
	}
	return subs, nil
}

// legacyRecords splits a merged event's attributes into records. A record
// starts at first (when given) or whenever a key repeats within the current
// record.
func legacyRecords(attrs []txlog.Attribute, first string) ([][]txlog.Attribute, error) {
	var out [][]txlog.Attribute
	var cur []txlog.Attribute
	seen := map[string]bool{}
	for _, a := range attrs {
		if (first != "" && a.Key == first) || seen[a.Key] {
			if len(cur) != 0 {
				out = append(out, cur)
			}
			cur, seen = nil, map[string]bool{}
		}
		if first != "" && len(cur) == 0 && a.Key != first {
			return nil, fmt.Errorf("record starts with %s", a.Key)
		}
		cur = append(cur, a)
		seen[a.Key] = true
	}
	if len(cur) != 0 {
		out = append(out, cur)
	}
	return out, nil
}

func attrValue(attrs []txlog.Attribute, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}
