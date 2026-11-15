package tax

import (
	"fmt"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// Bank payloads prove attribution without per-inner boundaries. Retained bank
// records must still match the complete payload multiset. InputOutputCoins
// emits recipient/amount transfers without sender, unlike SendCoins.
func icaBankOnlySubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	type key struct{ kind, who, sender, amount string }
	want, seen := map[key]int{}, map[key]int{}
	add := func(kind, who, sender string, coins sdk.Coins) { want[key{kind, who, sender, coins.String()}]++ }
	subs := make([]*txlog.LogMessage, len(inners))
	for i, inner := range inners {
		subs[i] = &txlog.LogMessage{MessageIndex: i}
		switch m := inner.(type) {
		case *banktypes.MsgSend:
			if m.FromAddress == "" || m.ToAddress == "" || !m.Amount.IsValid() || !m.Amount.IsAllPositive() {
				return nil, fmt.Errorf("invalid bank send payload")
			}
			add(eventCoinSpent, m.FromAddress, "", m.Amount)
			add(eventCoinReceived, m.ToAddress, "", m.Amount)
			add(string(CategoryTransfer), m.ToAddress, m.FromAddress, m.Amount)
		case *banktypes.MsgMultiSend:
			inputs, outputs := sdk.Coins{}, sdk.Coins{}
			for _, in := range m.Inputs {
				if in.Address == "" || !in.Coins.IsValid() || !in.Coins.IsAllPositive() {
					return nil, fmt.Errorf("invalid multisend input")
				}
				inputs = inputs.Add(in.Coins...)
			}
			for _, out := range m.Outputs {
				if out.Address == "" || !out.Coins.IsValid() || !out.Coins.IsAllPositive() {
					return nil, fmt.Errorf("invalid multisend output")
				}
				outputs = outputs.Add(out.Coins...)
			}
			if len(inputs) == 0 || !inputs.IsEqual(outputs) {
				return nil, fmt.Errorf("unbalanced multisend payload")
			}
			for _, in := range m.Inputs {
				add(eventCoinSpent, in.Address, "", in.Coins)
			}
			for _, out := range m.Outputs {
				add(eventCoinReceived, out.Address, "", out.Coins)
				add(string(CategoryTransfer), out.Address, "", out.Coins)
			}
		}
	}
	present := map[string]bool{}
	for _, ev := range log.Events {
		first := ""
		switch ev.Type {
		case eventICAPacket, eventRecvPacket, eventWriteAcknowledgement, eventMessage:
			continue
		case eventCoinSpent:
			first = attributeSpender
		case eventCoinReceived:
			first = attributeReceiver
		case string(CategoryTransfer):
			first = attributeRecipient
		default:
			return nil, fmt.Errorf("unassigned bank batch event")
		}
		present[ev.Type] = true
		records, err := legacyRecords(ev.Attributes, first)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			for _, a := range r {
				if a.Key != first && a.Key != attributeAmount && (ev.Type != string(CategoryTransfer) || a.Key != attributeSender) {
					return nil, fmt.Errorf("unexpected bank record field")
				}
			}
			coins, err := sdk.ParseCoinsNormalized(attrValue(r, attributeAmount))
			if err != nil || !coins.IsAllPositive() {
				return nil, fmt.Errorf("invalid bank record amount")
			}
			k := key{ev.Type, attrValue(r, first), attrValue(r, attributeSender), coins.String()}
			seen[k]++
			if seen[k] > want[k] {
				return nil, fmt.Errorf("bank record differs from explicit payloads")
			}
		}
	}
	// Older retained logs can omit whole event kinds, never individual records
	// within a retained kind. No-event committed packets keep baseline support.
	for k, n := range want {
		if present[k.kind] && seen[k] != n {
			return nil, fmt.Errorf("missing bank payload evidence")
		}
	}
	return subs, nil
}
