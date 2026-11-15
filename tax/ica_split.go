package tax

import (
	"fmt"
	"strings"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// icaHostSubLogs never reuses the packet's complete economic log for an inner
// message. Outer IBC bookkeeping is not inner execution evidence. Modern logs
// retain event order; historical ABCI logs merge attributes by event type.
func icaHostSubLogs(anys []*codectypes.Any, inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	bankOnly := true
	for _, inner := range inners {
		switch inner.(type) {
		case *banktypes.MsgSend, *banktypes.MsgMultiSend:
		default:
			bankOnly = false
		}
	}
	if bankOnly {
		return icaBankOnlySubLogs(inners, log)
	}
	bodyOnly := true
	for _, a := range anys {
		if !authzBodyOnly(a.TypeUrl) {
			bodyOnly = false
		}
	}
	if bodyOnly && !authzMovedCoins(log) {
		for _, ev := range log.Events {
			switch ev.Type {
			case eventICAPacket, eventRecvPacket, eventWriteAcknowledgement, eventMessage, eventAuthzGrant, eventAuthzRevoke, "set_withdraw_address":
			default:
				return nil, fmt.Errorf("unassigned event in body-only batch: %s", ev.Type)
			}
		}
		subs := make([]*txlog.LogMessage, len(inners))
		for i := range subs {
			subs[i] = &txlog.LogMessage{MessageIndex: i}
		}
		return subs, nil
	}
	economic := &txlog.LogMessage{}
	for _, ev := range log.Events {
		switch ev.Type {
		case eventICAPacket, eventRecvPacket, eventWriteAcknowledgement:
			continue
		}
		economic.Events = append(economic.Events, ev)
	}
	if subs, err := icaIndexedExecSubLogs(inners, economic); err == nil {
		return subs, nil
	}
	// Repeated keys identify the historical merged layout. Do not apply an
	// ordered boundary rule to such a log, even when there is just one terminal.
	merged := false
	for _, ev := range economic.Events {
		seen := map[string]bool{}
		for _, a := range ev.Attributes {
			if seen[a.Key] {
				merged = true
			}
			seen[a.Key] = true
		}
	}
	if !merged {
		if subs, err := icaOrderedSubLogs(inners, economic); err == nil {
			return subs, nil
		}
	}
	// Handler-closing message records give legacy staking/distribution batches
	// their transfer boundaries. Strip only the outer MsgRecvPacket markers.
	legacy := &txlog.LogMessage{}
	for _, ev := range economic.Events {
		if ev.Type == eventMessage {
			attrs := make([]txlog.Attribute, 0, len(ev.Attributes))
			for j := 0; j < len(ev.Attributes); j++ {
				a := ev.Attributes[j]
				if a.Key == attributeAction && a.Value == typeURLIBCRecvPacket {
					// SDK 0.47 createEvents prepends action, then the
					// outer message's first signer. SDK 0.45 has no signer.
					if j+1 < len(ev.Attributes) && ev.Attributes[j+1].Key == attributeSender {
						j++
					}
					continue
				}
				if a.Key == attributeModule && a.Value == moduleIBCChannel {
					continue
				}
				attrs = append(attrs, a)
			}
			ev.Attributes = attrs
		}
		legacy.Events = append(legacy.Events, ev)
	}
	if subs, err := icaLegacyExecSubLogs(inners, legacy); err == nil {
		return subs, nil
	}
	if subs, err := legacyAuthzSubLogs(inners, legacy); err == nil && icaLegacyTerminals(inners, subs) {
		return subs, nil
	}
	if subs, err := icaLegacyStakingSubLogs(inners, legacy); err == nil && icaLegacyTerminals(inners, subs) {
		return subs, nil
	}
	subs, err := icaLegacyRedeemSubLogs(inners, economic)
	if err == nil {
		return subs, nil
	}
	return nil, fmt.Errorf("inner event boundaries are not proven: %w", err)
}

func icaLegacyTerminals(inners []sdk.Msg, subs []*txlog.LogMessage) bool {
	for i, m := range inners {
		if authzNoTaxEffect[sdk.MsgTypeURL(m)] {
			continue
		}
		count := 0
		for _, e := range subs[i].Events {
			if icaTerminal(m, e) {
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	return true
}

// Ordered handlers finish with their operation's event. Bank sends finish with
// transfer; their following message{sender} is non-economic bookkeeping. Every
// economic event must precede exactly one matching terminal, with no remainder.
func icaOrderedSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	subs := make([]*txlog.LogMessage, len(inners))
	next := 0
	pending := &txlog.LogMessage{}
	for _, ev := range log.Events {
		if ev.Type == eventMessage {
			continue
		}
		if next == len(inners) {
			return nil, fmt.Errorf("economic events after final inner")
		}
		if !icaEconomicEvent(ev.Type) {
			return nil, fmt.Errorf("unsupported inner event %s", ev.Type)
		}
		pending.Events = append(pending.Events, ev)
		if icaTerminal(inners[next], ev) {
			pending.MessageIndex = next
			subs[next] = pending
			pending = &txlog.LogMessage{}
			next++
		}
	}
	if next != len(inners) || len(pending.Events) != 0 {
		return nil, fmt.Errorf("missing inner terminal")
	}
	// Body-only messages must not swallow the preceding operation's rewards or
	// other economic events simply because their own transfer is a terminal.
	for i, inner := range inners {
		if _, ok := inner.(*banktypes.MsgSend); ok {
			for _, ev := range subs[i].Events {
				switch ev.Type {
				case string(CategoryTransfer), eventCoinSpent, eventCoinReceived:
				default:
					return nil, fmt.Errorf("bank send contains another operation")
				}
			}
			count := 0
			send := inner.(*banktypes.MsgSend)
			for _, ev := range subs[i].Events {
				if ev.Type == string(CategoryTransfer) {
					count++
					if !icaTerminal(send, ev) {
						return nil, fmt.Errorf("bank send contains another transfer")
					}
				}
				a := attrMap(ev)
				if ev.Type == eventCoinSpent && (a[attributeSpender] != send.FromAddress || a[attributeAmount] != send.Amount.String()) {
					return nil, fmt.Errorf("bank debit mismatch")
				}
				if ev.Type == eventCoinReceived && (a[attributeReceiver] != send.ToAddress || a[attributeAmount] != send.Amount.String()) {
					return nil, fmt.Errorf("bank credit mismatch")
				}
			}
			if count != 1 {
				return nil, fmt.Errorf("bank send transfer evidence is not singular")
			}
		}
	}
	return subs, nil
}

func icaEconomicEvent(kind string) bool {
	switch kind {
	case eventCoinSpent, eventCoinReceived, string(CategoryTransfer), eventWithdrawRewards, eventWithdrawCommission,
		string(CategoryDelegate), eventUnbond, string(CategoryRedelegate), string(CategoryRedeemShares), eventWithdrawTokenizeShareReward, eventBurn:
		return true
	}
	return false
}

func icaTerminal(inner sdk.Msg, ev txlog.LogMessageEvent) bool {
	a := attrMap(ev)
	switch m := inner.(type) {
	case *banktypes.MsgSend:
		return ev.Type == string(CategoryTransfer) && a[attributeSender] == m.FromAddress && a[attributeRecipient] == m.ToAddress && a[attributeAmount] == m.Amount.String()
	case *stakingtypes.MsgDelegate:
		return ev.Type == string(CategoryDelegate) && a[attributeValidator] == m.ValidatorAddress && a[attributeAmount] == m.Amount.String()
	case *stakingtypes.MsgUndelegate:
		return ev.Type == eventUnbond && a[attributeValidator] == m.ValidatorAddress && a[attributeAmount] == m.Amount.String()
	case *stakingtypes.MsgBeginRedelegate:
		return ev.Type == string(CategoryRedelegate) && a["source_validator"] == m.ValidatorSrcAddress && a["destination_validator"] == m.ValidatorDstAddress && a[attributeAmount] == m.Amount.String()
	case *disttypes.MsgWithdrawDelegatorReward:
		return ev.Type == eventWithdrawRewards && a[attributeValidator] == m.ValidatorAddress && (a[attributeDelegator] == "" || a[attributeDelegator] == m.DelegatorAddress)
	case *disttypes.MsgWithdrawValidatorCommission:
		return ev.Type == eventWithdrawCommission
	case *MsgRedeemTokensForShares:
		return icaRedeemTerminal(m, ev)
	case *LegacyMsgRedeemTokensForShares:
		return icaRedeemTerminal(&m.MsgRedeemTokensForShares, ev)
	}
	return false
}

func icaRedeemTerminal(m *MsgRedeemTokensForShares, ev txlog.LogMessageEvent) bool {
	var coin sdk.Coin
	if coin.Unmarshal(m.AmountRaw) != nil {
		return false
	}
	a := attrMap(ev)
	validator, _ := shareDenomRecord(m.AmountRaw)
	return ev.Type == string(CategoryRedeemShares) && a[attributeDelegator] == m.DelegatorAddress && a[attributeAmount] == coin.String() && a[attributeValidator] == validator
}

// Historical redeem batches have no handler-closing message event. Their
// bank trace does retain ordered share-token sends and principal returns.
// A unique share send followed immediately by the return closes the principal
// operation; an optional following distribution withdrawal belongs to it only
// when it is not a payment to another record. All reward records must then
// reconcile one-for-one to the assigned distribution/payout transfers.
func icaLegacyRedeemSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	msgs := make([]*MsgRedeemTokensForShares, len(inners))
	records := map[string]bool{}
	for i, inner := range inners {
		switch m := inner.(type) {
		case *MsgRedeemTokensForShares:
			msgs[i] = m
		case *LegacyMsgRedeemTokensForShares:
			msgs[i] = &m.MsgRedeemTokensForShares
		default:
			return nil, fmt.Errorf("not a redeem-only batch")
		}
		_, id := shareDenomRecord(msgs[i].AmountRaw)
		if id == "" || records[lsmRecordAccount(id)] {
			return nil, fmt.Errorf("missing or repeated record identity")
		}
		records[lsmRecordAccount(id)] = true
	}
	if err := icaRedeemBankEvidence(log, msgs); err != nil {
		return nil, err
	}
	byType := map[string][]txlog.Attribute{}
	for _, ev := range log.Events {
		switch ev.Type {
		case eventMessage, eventCoinSpent, eventCoinReceived, eventBurn:
			continue
		case string(CategoryRedeemShares), string(CategoryTransfer), eventWithdrawRewards, eventWithdrawTokenizeShareReward:
			byType[ev.Type] = append(byType[ev.Type], ev.Attributes...)
		default:
			return nil, fmt.Errorf("unexpected redeem event")
		}
	}
	terminals, err := legacyRecords(byType[string(CategoryRedeemShares)], attributeDelegator)
	if err != nil || len(terminals) != len(msgs) {
		return nil, fmt.Errorf("redeem terminal count")
	}
	transfers, err := legacyRecords(byType[string(CategoryTransfer)], attributeRecipient)
	if err != nil {
		return nil, err
	}
	rewards, err := legacyRecords(byType[eventWithdrawRewards], attributeAmount)
	if err != nil {
		return nil, err
	}
	payouts, err := legacyRecords(byType[eventWithdrawTokenizeShareReward], attributeWithdrawAddress)
	if err != nil {
		return nil, err
	}
	subs := make([]*txlog.LogMessage, len(msgs))
	start, rw, po := 0, 0, 0
	for i, m := range msgs {
		terminal := txlog.LogMessageEvent{Type: string(CategoryRedeemShares), Attributes: terminals[i]}
		if !icaRedeemTerminal(m, terminal) {
			return nil, fmt.Errorf("redeem terminal mismatch")
		}
		var coin sdk.Coin
		if err := coin.Unmarshal(m.AmountRaw); err != nil {
			return nil, err
		}
		anchor := -1
		for j := start; j < len(transfers); j++ {
			if attrValue(transfers[j], attributeSender) == m.DelegatorAddress && attrValue(transfers[j], attributeAmount) == coin.String() {
				if anchor != -1 {
					return nil, fmt.Errorf("ambiguous share transfer")
				}
				anchor = j
			}
		}
		if anchor < 0 || anchor+1 >= len(transfers) {
			return nil, fmt.Errorf("missing share/principal transfer")
		}
		module := attrValue(transfers[anchor], attributeRecipient)
		principal := transfers[anchor+1]
		if attrValue(principal, attributeSender) != module || attrValue(principal, attributeRecipient) != m.DelegatorAddress {
			return nil, fmt.Errorf("principal return mismatch")
		}
		end := anchor + 2
		if end < len(transfers) && attrValue(transfers[end], attributeSender) == distributionModuleAddress && !records[attrValue(transfers[end], attributeRecipient)] {
			end++
		}
		sub := &txlog.LogMessage{MessageIndex: i, Events: []txlog.LogMessageEvent{terminal}}
		_, id := shareDenomRecord(m.AmountRaw)
		record := lsmRecordAccount(id)
		validator, _ := shareDenomRecord(m.AmountRaw)
		for _, tr := range transfers[start:end] {
			sender, recipient, amount := attrValue(tr, attributeSender), attrValue(tr, attributeRecipient), attrValue(tr, attributeAmount)
			if sender == distributionModuleAddress {
				if rw >= len(rewards) || attrValue(rewards[rw], attributeAmount) != amount || attrValue(rewards[rw], attributeValidator) != validator {
					return nil, fmt.Errorf("distribution reward mismatch")
				}
				who := attrValue(rewards[rw], attributeDelegator)
				if who != "" && who != record && who != m.DelegatorAddress {
					return nil, fmt.Errorf("reward delegator mismatch")
				}
				if records[recipient] && recipient != record {
					return nil, fmt.Errorf("reward belongs to another record")
				}
				sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventWithdrawRewards, Attributes: rewards[rw]})
				rw++
			}
			if sender == record {
				if po >= len(payouts) || attrValue(payouts[po], attributeWithdrawAddress) != recipient || attrValue(payouts[po], attributeAmount) != amount {
					return nil, fmt.Errorf("record payout mismatch")
				}
				sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventWithdrawTokenizeShareReward, Attributes: payouts[po]})
				po++
			}
			// Every non-principal external transfer must be this record's
			// reward receipt or owner payout. Pool-to-pool moves are internal
			// principal evidence, never an excuse to absorb another wallet's
			// transfer into an unrelated redeem.
			if sender != distributionModuleAddress && sender != record && !icaRedeemPrincipalTransfer(sender, recipient, m.DelegatorAddress, module) {
				return nil, fmt.Errorf("unassigned external redeem transfer")
			}
			if strings.Contains(amount, "/") && amount == coin.String() && sender != m.DelegatorAddress {
				return nil, fmt.Errorf("unexpected share movement")
			}
			sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: string(CategoryTransfer), Attributes: tr})
		}
		// SDK emits a zero own-withdrawal record even when bank sends nothing.
		// Consume only a proven zero for this validator, never an unmatched
		// positive amount or a record from the next operation.
		if rw < len(rewards) && attrValue(rewards[rw], attributeValidator) == validator {
			coins, err := sdk.ParseCoinsNormalized(attrValue(rewards[rw], attributeAmount))
			who := attrValue(rewards[rw], attributeDelegator)
			if err == nil && coins.IsZero() && (who == "" || who == m.DelegatorAddress) {
				sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: eventWithdrawRewards, Attributes: rewards[rw]})
				rw++
			}
		}
		subs[i], start = sub, end
	}
	if start != len(transfers) || rw != len(rewards) || po != len(payouts) {
		return nil, fmt.Errorf("unassigned redeem evidence")
	}
	return subs, nil
}
