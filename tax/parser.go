package tax

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/DefiantLabs/cosmos-indexer/config"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MessageTypeURLs is every message type this parser classifies. main() registers
// the same parser instance under each.
var MessageTypeURLs = []string{
	"/cosmos.bank.v1beta1.MsgSend",
	"/cosmos.bank.v1beta1.MsgMultiSend",
	"/cosmos.staking.v1beta1.MsgDelegate",
	"/cosmos.staking.v1beta1.MsgUndelegate",
	"/cosmos.staking.v1beta1.MsgBeginRedelegate",
	"/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward",
	"/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission",
	"/cosmos.authz.v1beta1.MsgExec",
	"/ibc.applications.transfer.v1.MsgTransfer",
	"/ibc.core.channel.v1.MsgRecvPacket",
}

// Parser implements parsers.MessageParser. One instance handles all the message
// types above.
type Parser struct{ ID string }

func (p *Parser) Identifier() string { return p.ID }

func (p *Parser) ParseMessage(cosmosMsg sdk.Msg, log *indexerTxTypes.LogMessage, cfg config.IndexConfig) (*any, error) {
	events := classify(cosmosMsg, log)
	if len(events) == 0 {
		return nil, nil
	}
	v := any(events)
	return &v, nil
}

// classify turns a single message + its event log into taxable events. It's a
// standalone function so MsgExec can recurse into its inner messages.
func classify(cosmosMsg sdk.Msg, log *indexerTxTypes.LogMessage) []TaxableEvent {
	var events []TaxableEvent

	switch m := cosmosMsg.(type) {
	case *banktypes.MsgSend:
		for _, c := range m.Amount {
			events = append(events, TaxableEvent{
				Category: string(CategoryTransfer),
				FromAddr: m.FromAddress, ToAddr: m.ToAddress,
				Amount: c.Amount.String(), Denom: c.Denom,
			})
		}

	case *banktypes.MsgMultiSend:
		from := ""
		if len(m.Inputs) == 1 {
			from = m.Inputs[0].Address
		}
		for _, out := range m.Outputs {
			for _, c := range out.Coins {
				events = append(events, TaxableEvent{
					Category: string(CategoryTransfer),
					FromAddr: from, ToAddr: out.Address,
					Amount: c.Amount.String(), Denom: c.Denom,
				})
			}
		}

	// Staking delegate/undelegate/redelegate are NOT taxable themselves, but the
	// SDK auto-withdraws pending rewards on each — that reward is income.
	case *stakingtypes.MsgDelegate:
		events = append(events, rewardEvents(log, m.DelegatorAddress)...)
	case *stakingtypes.MsgUndelegate:
		events = append(events, rewardEvents(log, m.DelegatorAddress)...)
	case *stakingtypes.MsgBeginRedelegate:
		events = append(events, rewardEvents(log, m.DelegatorAddress)...)

	case *disttypes.MsgWithdrawDelegatorReward:
		events = append(events, rewardEvents(log, m.DelegatorAddress)...)

	case *disttypes.MsgWithdrawValidatorCommission:
		for recv, coins := range receivedCoinsByReceiver(log) {
			for _, c := range coins {
				events = append(events, TaxableEvent{
					Category: string(CategoryCommission),
					ToAddr:   recv,
					Amount:   c.Amount.String(), Denom: c.Denom,
				})
			}
		}

	case *authz.MsgExec:
		inners, err := m.GetMessages()
		if err != nil {
			return nil
		}
		for i, inner := range inners {
			sub := eventsForAuthzIndex(log, i)
			events = append(events, classify(inner, sub)...)
		}

	case *transfertypes.MsgTransfer:
		events = append(events, TaxableEvent{
			Category: string(CategoryIBCOut),
			FromAddr: m.Sender, ToAddr: m.Receiver,
			Amount: m.Token.Amount.String(), Denom: m.Token.Denom,
		})

	case *chantypes.MsgRecvPacket:
		var data transfertypes.FungibleTokenPacketData
		if err := json.Unmarshal(m.Packet.GetData(), &data); err == nil && data.Amount != "" {
			events = append(events, TaxableEvent{
				Category: string(CategoryIBCIn),
				FromAddr: data.Sender, ToAddr: data.Receiver,
				Amount: data.Amount, Denom: data.Denom,
			})
		}
	}

	return events
}

func (p *Parser) IndexMessage(dataset *any, db *gorm.DB, message models.Message, _ []parsers.MessageEventWithAttributes, cfg config.IndexConfig) error {
	events, ok := (*dataset).([]TaxableEvent)
	if !ok {
		return errors.New("tax: unexpected dataset type")
	}
	for i := range events {
		events[i].MessageID = message.ID
		events[i].SubIndex = i
		events[i].BlockHeight = message.Tx.Block.Height
		events[i].Timestamp = message.Tx.Block.TimeStamp
		events[i].TxHash = message.Tx.Hash
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "message_id"}, {Name: "sub_index"}},
			DoUpdates: clause.AssignmentColumns([]string{"category", "amount", "denom", "from_addr", "to_addr", "block_height", "timestamp", "tx_hash"}),
		}).Create(&events[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

// rewardEvents emits CategoryReward events for the coins credited to delegator
// in this message's transfer/coin_received events (the auto-withdrawn reward).
func rewardEvents(log *indexerTxTypes.LogMessage, delegator string) []TaxableEvent {
	var out []TaxableEvent
	for _, c := range coinsReceivedBy(log, delegator) {
		out = append(out, TaxableEvent{
			Category: string(CategoryReward),
			ToAddr:   delegator,
			Amount:   c.Amount.String(), Denom: c.Denom,
		})
	}
	return out
}

// coinsReceivedBy sums the coins credited to target across this message's
// transfer (recipient/amount) and coin_received (receiver/amount) events.
func coinsReceivedBy(log *indexerTxTypes.LogMessage, target string) sdk.Coins {
	total := sdk.NewCoins()
	for _, ev := range log.Events {
		if ev.Type != "transfer" && ev.Type != "coin_received" {
			continue
		}
		cur := ""
		for _, a := range ev.Attributes {
			switch a.Key {
			case "recipient", "receiver":
				cur = a.Value
			case "amount":
				if cur == target {
					if coins, err := sdk.ParseCoinsNormalized(a.Value); err == nil {
						total = total.Add(coins...)
					}
				}
			}
		}
	}
	return total
}

// receivedCoinsByReceiver groups coin_received amounts by receiver (used for
// validator commission, where the recipient isn't on the message).
func receivedCoinsByReceiver(log *indexerTxTypes.LogMessage) map[string]sdk.Coins {
	out := map[string]sdk.Coins{}
	for _, ev := range log.Events {
		if ev.Type != "coin_received" {
			continue
		}
		cur := ""
		for _, a := range ev.Attributes {
			switch a.Key {
			case "receiver":
				cur = a.Value
			case "amount":
				if cur != "" {
					if coins, err := sdk.ParseCoinsNormalized(a.Value); err == nil {
						out[cur] = out[cur].Add(coins...)
					}
				}
			}
		}
	}
	return out
}

// eventsForAuthzIndex returns a sub-log containing only the events tagged with
// the given authz_msg_index, so an inner MsgExec message classifies against its
// own events.
func eventsForAuthzIndex(log *indexerTxTypes.LogMessage, idx int) *indexerTxTypes.LogMessage {
	target := strconv.Itoa(idx)
	sub := &indexerTxTypes.LogMessage{}
	for _, ev := range log.Events {
		for _, a := range ev.Attributes {
			if a.Key == "authz_msg_index" && a.Value == target {
				sub.Events = append(sub.Events, ev)
				break
			}
		}
	}
	return sub
}
