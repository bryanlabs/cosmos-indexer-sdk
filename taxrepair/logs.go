package main

import (
	"fmt"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	sdk "github.com/cosmos/cosmos-sdk/types"
	dist "github.com/cosmos/cosmos-sdk/x/distribution/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func attributes(e txlog.LogMessageEvent) map[string]string {
	out := map[string]string{}
	for _, a := range e.Attributes {
		out[a.Key] = a.Value
	}
	return out
}
func parse(msg sdk.Msg, log *txlog.LogMessage) ([]tax.TaxableEvent, error) {
	data, err := (&tax.Parser{ID: "taxrepair"}).ParseMessage(msg, log, config.IndexConfig{})
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("parser returned nil")
	}
	return (*data).([]tax.TaxableEvent), nil
}

// Production disabled storage of message_bytes. Standard staking messages can
// still be reconstructed from their authoritative message action/sender events:
// only the delegator is used by the reward classifier, not principal amounts.
// No reconstructed message is signed, submitted, or broadcast anywhere.
func fromStoredLog(typ string, log *txlog.LogMessage, old []tax.TaxableEvent) ([]tax.TaxableEvent, error) {
	sender := ""
	for _, e := range log.Events {
		a := attributes(e)
		if e.Type == "message" && a["action"] == typ {
			if sender != "" && sender != a["sender"] {
				return nil, fmt.Errorf("ambiguous message sender")
			}
			sender = a["sender"]
		}
	}
	if typ == "/cosmos.authz.v1beta1.MsgExec" {
		return authzFromStoredLog(log, old)
	}
	if sender == "" {
		return nil, fmt.Errorf("stored message has no action/sender event")
	}
	switch typ {
	case "/cosmos.staking.v1beta1.MsgDelegate":
		return parse(&staking.MsgDelegate{DelegatorAddress: sender}, log)
	case "/cosmos.staking.v1beta1.MsgUndelegate":
		return parse(&staking.MsgUndelegate{DelegatorAddress: sender}, log)
	case "/cosmos.staking.v1beta1.MsgBeginRedelegate":
		return parse(&staking.MsgBeginRedelegate{DelegatorAddress: sender}, log)
	case "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward":
		return parse(&dist.MsgWithdrawDelegatorReward{DelegatorAddress: sender}, log)
	case "/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission":
		validator := ""
		for _, e := range log.Events {
			if e.Type == "withdraw_commission" {
				a := attributes(e)
				if a["validator"] != "" {
					validator = a["validator"]
				}
			}
		}
		if validator == "" && strings.HasPrefix(sender, "cosmosvaloper1") {
			validator = sender
		}
		// Do not change commission amounts in a delegator-reward repair. Retain the
		// existing record, adding a verified operator address only when available.
		out := append([]tax.TaxableEvent(nil), old...)
		for i := range out {
			out[i].ID = 0
			if out[i].Category == "commission" {
				out[i].ValidatorAddress = validator
				out[i].RewardTrigger = "commission"
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported stored type %s", typ)
}

// For authz without body bytes, preserve non-reward classifications verbatim.
// Each on-chain withdrawal is passed individually through the same production
// classifier, retaining its delegator and validator. Event grouping identifies
// staking auto-withdrawals; unknown inner actions are labelled authz, not guessed.
// LSM uses module-account hops, so refuse mixed LSM logs rather than misattribute.
func authzFromStoredLog(log *txlog.LogMessage, old []tax.TaxableEvent) ([]tax.TaxableEvent, error) {
	triggers := map[string]string{}
	for _, e := range log.Events {
		if strings.Contains(e.Type, "tokeniz") || strings.Contains(e.Type, "redeem") {
			return nil, fmt.Errorf("authz LSM log needs original transaction body")
		}
		a := attributes(e)
		key := a["authz_msg_index"]
		switch e.Type {
		case "delegate":
			triggers[key] = "delegate"
		case "unbond":
			triggers[key] = "undelegate"
		case "redelegate":
			triggers[key] = "redelegate"
		}
	}
	out := []tax.TaxableEvent{}
	for _, e := range old {
		if e.Category != "reward" {
			e.ID = 0
			out = append(out, e)
		}
	}
	for _, event := range log.Events {
		if event.Type != "withdraw_rewards" {
			continue
		}
		a := attributes(event)
		if a["delegator"] == "" {
			return nil, fmt.Errorf("legacy authz withdrawal needs original transaction body")
		}
		rows, err := parse(&dist.MsgWithdrawDelegatorReward{DelegatorAddress: a["delegator"], ValidatorAddress: a["validator"]}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{event}})
		if err != nil {
			return nil, err
		}
		trigger := triggers[a["authz_msg_index"]]
		if trigger == "" {
			trigger = "authz"
		}
		for i := range rows {
			rows[i].RewardTrigger = trigger
		}
		out = append(out, rows...)
	}
	return out, nil
}
