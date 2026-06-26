package tax

import (
	"encoding/json"
	"errors"

	"github.com/DefiantLabs/cosmos-indexer/config"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
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
	"/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward",
	"/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission",
	"/ibc.applications.transfer.v1.MsgTransfer",
	"/ibc.core.channel.v1.MsgRecvPacket",
}

// Parser implements parsers.MessageParser. One instance handles all the message
// types above; ParseMessage type-switches and returns the taxable events for
// that message (height/time/hash are filled in IndexMessage where the Message
// model is available).
type Parser struct{ ID string }

func (p *Parser) Identifier() string { return p.ID }

func (p *Parser) ParseMessage(cosmosMsg sdk.Msg, log *indexerTxTypes.LogMessage, cfg config.IndexConfig) (*any, error) {
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
		// SDK 0.47 restricts MultiSend to a single input; fall back to "" if not.
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

	case *disttypes.MsgWithdrawDelegatorReward:
		// Reward amount lives in the transfer/coin_received events to the delegator.
		for _, c := range coinsReceivedBy(log, m.DelegatorAddress) {
			events = append(events, TaxableEvent{
				Category: string(CategoryReward),
				ToAddr:   m.DelegatorAddress,
				Amount:   c.Amount.String(), Denom: c.Denom,
			})
		}

	case *disttypes.MsgWithdrawValidatorCommission:
		// The recipient (validator's withdraw addr) isn't in the msg; take it from
		// the coin_received events.
		for recv, coins := range receivedCoinsByReceiver(log) {
			for _, c := range coins {
				events = append(events, TaxableEvent{
					Category: string(CategoryCommission),
					ToAddr:   recv,
					Amount:   c.Amount.String(), Denom: c.Denom,
				})
			}
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

	default:
		return nil, nil
	}

	if len(events) == 0 {
		return nil, nil
	}
	v := any(events)
	return &v, nil
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
