package tax

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
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
	"/cosmwasm.wasm.v1.MsgExecuteContract",
	TypeURLWithdrawTokenizeShareReward,
	TypeURLWithdrawAllTokenizeShareReward,
	TypeURLV2RecvPacket,
	TypeURLTFMint,
	TypeURLTFForceTransfer,
}

// Parser implements parsers.MessageParser. One instance handles all the message
// types above.
type Parser struct{ ID string }

func (p *Parser) Identifier() string { return p.ID }

func (p *Parser) ParseMessage(cosmosMsg sdk.Msg, log *indexerTxTypes.LogMessage, cfg config.IndexConfig) (*any, error) {
	if log != nil {
		for _, event := range log.Events {
			if event.Type != "withdraw_rewards" {
				continue
			}
			groups, err := rewardAttributeGroups(event)
			if err != nil {
				return nil, err
			}
			for _, a := range groups {
				if _, err := sdk.ParseCoinsNormalized(a["amount"]); err != nil {
					return nil, fmt.Errorf("tax: malformed withdraw_rewards amount: %w", err)
				}
			}
		}
	}
	events := classify(cosmosMsg, log)
	// Persist empty results too, so reclassification removes stale rows.
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
	// SDK auto-withdraws pending rewards on each — that reward is income. This
	// auto-withdrawal uses the same withdraw-address routing as an explicit
	// MsgWithdrawDelegatorReward, so it needs the same redirect-safe attribution.
	case *stakingtypes.MsgDelegate:
		events = append(events, delegatorRewardEvents(log, m.DelegatorAddress, "delegate")...)
	case *stakingtypes.MsgUndelegate:
		events = append(events, delegatorRewardEvents(log, m.DelegatorAddress, "undelegate")...)
	case *stakingtypes.MsgBeginRedelegate:
		events = append(events, delegatorRewardEvents(log, m.DelegatorAddress, "redelegate")...)

	case *disttypes.MsgWithdrawDelegatorReward:
		events = append(events, delegatorRewardEvents(log, m.DelegatorAddress, "claim")...)

	case *disttypes.MsgWithdrawValidatorCommission:
		for recv, coins := range receivedCoinsByReceiver(log) {
			for _, c := range coins {
				events = append(events, TaxableEvent{
					Category: string(CategoryCommission),
					ToAddr:   recv,
					Amount:   c.Amount.String(), Denom: c.Denom,
					ValidatorAddress: m.ValidatorAddress, RewardTrigger: "commission",
				})
			}
		}

	// Liquid Staking Module reward withdrawals. The amount is not on the
	// message, and delegatorRewardEvents cannot be reused here: LSM pays the
	// record's module account first and then forwards to the owner, so summing
	// coin_received counts the same reward twice and picks up the tip payee. The
	// module emits withdraw_tokenize_share_reward with the settled amount per
	// record, which is the authoritative figure.
	case *MsgWithdrawTokenizeShareRecordReward:
		events = append(events, tokenizeShareRewardEvents(log, m.OwnerAddress)...)

	case *MsgWithdrawAllTokenizeShareRecordReward:
		events = append(events, tokenizeShareRewardEvents(log, m.OwnerAddress)...)

	case *authz.MsgExec:
		// GetMessages is all-or-nothing: one inner message the codec cannot
		// decode makes it error, which used to discard the entire MsgExec.
		// Restake bots wrap reward withdrawals and delegations in here, so that
		// silently dropped real income. Unpack them one at a time instead and
		// keep the index alignment the event lookup depends on.
		for i, anyMsg := range m.Msgs {
			inner, ok := anyMsg.GetCachedValue().(sdk.Msg)
			if !ok || inner == nil {
				config.Log.Debugf("tax: skipping undecodable authz inner message %d of type %s", i, anyMsg.TypeUrl)
				continue
			}
			sub := eventsForAuthzIndex(log, i)
			events = append(events, classify(inner, sub)...)
		}

	case *transfertypes.MsgTransfer:
		events = append(events, TaxableEvent{
			Category: string(CategoryIBCOut),
			FromAddr: m.Sender, ToAddr: m.Receiver,
			Amount: m.Token.Amount.String(), Denom: m.Token.Denom,
		})

	// CosmWasm contract calls. The dominant taxable case on Cosmos Hub is NFT
	// marketplace sales (Stargaze Marketplace v2), which emit an authoritative
	// wasm-finalize-sale event carrying the asset, price, seller and buyer.
	case *wasmtypes.MsgExecuteContract:
		events = append(events, nftSaleEvents(log)...)
		events = append(events, swapEvents(log)...)
		events = append(events, nftMintEvents(log)...)

	// A tokenfactory mint credits the recipient with newly created coins, which
	// is an acquisition for them. Cosmos Hub's observed traffic is STARS
	// distributed this way. Amount and recipient are both on the message.
	case *MsgTFMint:
		if m.Amount != "" && m.Denom != "" {
			events = append(events, TaxableEvent{
				Category: string(CategoryTransfer),
				FromAddr: m.Sender, ToAddr: m.Recipient(),
				Amount: m.Amount, Denom: m.Denom,
			})
		}

	// An admin moving someone else's coins is still a movement for both sides.
	case *MsgTFForceTransfer:
		if m.Amount != "" && m.Denom != "" {
			events = append(events, TaxableEvent{
				Category: string(CategoryTransfer),
				FromAddr: m.TransferFromAddress, ToAddr: m.TransferToAddress,
				Amount: m.Amount, Denom: m.Denom,
			})
		}

	// IBC channel v2 receive: an inbound transfer, same as the v1 case below.
	// The amount comes from the fungible_token_packet event rather than the
	// packet payload, because v2 payloads on Cosmos Hub arrive solidity-ABI
	// encoded from the Ethereum bridge. See tax/ibcv2.go.
	case *MsgRecvPacketV2:
		events = append(events, ibcV2ReceiveEvents(log)...)

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
	return db.Transaction(func(tx *gorm.DB) error {
		for i := range events {
			events[i].MessageID = message.ID
			events[i].SubIndex = i
			events[i].BlockHeight = message.Tx.Block.Height
			events[i].Timestamp = message.Tx.Block.TimeStamp
			events[i].TxHash = message.Tx.Hash
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "message_id"}, {Name: "sub_index"}},
				DoUpdates: clause.AssignmentColumns([]string{"category", "amount", "denom", "asset", "from_addr", "to_addr", "block_height", "timestamp", "tx_hash", "validator_address", "reward_trigger"}),
			}).Create(&events[i]).Error; err != nil {
				return err
			}
		}
		return tx.Where("message_id = ? AND sub_index >= ?", message.ID, len(events)).Delete(&TaxableEvent{}).Error
	})
}

// nftSaleEvents turns each wasm-finalize-sale event (CosmWasm NFT marketplace,
// e.g. Stargaze Marketplace v2) into a taxable NFT sale: the seller disposes the
// NFT for `price` of `denom`, the buyer (nft_recipient) acquires it. One event
// holds both parties; direction is derived per-address at export time.
func nftSaleEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != "wasm-finalize-sale" {
			continue
		}
		var collection, tokenID, denom, price, seller, buyer string
		for _, a := range ev.Attributes {
			switch a.Key {
			case "collection":
				collection = a.Value
			case "token_id":
				tokenID = a.Value
			case "denom":
				denom = a.Value
			case "price":
				price = a.Value
			case "seller_recipient":
				seller = a.Value
			case "nft_recipient":
				buyer = a.Value
			}
		}
		if price == "" || collection == "" {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryNFTSale),
			FromAddr: seller, ToAddr: buyer,
			Amount: price, Denom: denom,
			Asset: collection + "/" + tokenID,
		})
	}
	return out
}

// swapEvents turns each CosmWasm DEX swap (wasm event, action=swap) into two
// taxable legs for the receiver: a disposal of the offered asset and an
// acquisition of the returned asset (a crypto-to-crypto trade).
func swapEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != "wasm" {
			continue
		}
		a := attrMap(ev)
		if a["action"] != "swap" {
			continue
		}
		recv := a["receiver"]
		if recv == "" || a["offer_asset"] == "" || a["ask_asset"] == "" {
			continue
		}
		// Disposal: the offered asset leaves the receiver.
		out = append(out, TaxableEvent{
			Category: string(CategorySwap),
			FromAddr: recv,
			Amount:   a["offer_amount"], Denom: a["offer_asset"],
		})
		// Acquisition: the returned asset arrives.
		out = append(out, TaxableEvent{
			Category: string(CategorySwap),
			ToAddr:   recv,
			Amount:   a["return_amount"], Denom: a["ask_asset"],
		})
	}
	return out
}

// nftMintEvents turns each CosmWasm NFT mint (wasm event, action=mint) into an
// acquisition for the owner, with the mint cost (coins the owner spent in this
// message) as the basis.
func nftMintEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != "wasm" {
			continue
		}
		a := attrMap(ev)
		if a["action"] != "mint" || a["token_id"] == "" {
			continue
		}
		owner := a["owner"]
		if owner == "" {
			owner = a["minter"]
		}
		collection := a["_contract_address"]
		// Mint cost = what the owner spent in this message (the mint price).
		cost := coinsSpentBy(log, owner)
		amount, denom := "", ""
		if len(cost) > 0 {
			amount, denom = cost[0].Amount.String(), cost[0].Denom
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryNFTMint),
			ToAddr:   owner,
			Amount:   amount, Denom: denom,
			Asset: collection + "/" + a["token_id"],
		})
	}
	return out
}

// attrMap flattens an event's attributes into a map.
func attrMap(ev indexerTxTypes.LogMessageEvent) map[string]string {
	m := make(map[string]string, len(ev.Attributes))
	for _, a := range ev.Attributes {
		m[a.Key] = a.Value
	}
	return m
}

// coinsSpentBy sums the coins debited from target via coin_spent events.
func coinsSpentBy(log *indexerTxTypes.LogMessage, target string) sdk.Coins {
	total := sdk.NewCoins()
	for _, ev := range log.Events {
		if ev.Type != "coin_spent" {
			continue
		}
		cur := ""
		for _, a := range ev.Attributes {
			switch a.Key {
			case "spender":
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

// delegatorRewardEvents reads only distribution's authoritative withdraw_rewards
// events. coin_received also includes principal credited to staking pools and
// must never be used to infer rewards. Attribute income to the event's delegator
// even when its payment is redirected to a different withdrawal address.
// Redelegation may withdraw from BOTH validators; retain each event separately.
func delegatorRewardEvents(log *indexerTxTypes.LogMessage, delegator, trigger string) []TaxableEvent {
	var out []TaxableEvent
	if log == nil {
		return out
	}
	for _, event := range log.Events {
		if event.Type != "withdraw_rewards" {
			continue
		}
		groups, err := rewardAttributeGroups(event)
		if err != nil {
			continue
		}
		for _, a := range groups {
			// Older SDKs omit the delegator, so use the message's delegator.
			if a["delegator"] != "" && a["delegator"] != delegator {
				continue
			}
			coins, err := sdk.ParseCoinsNormalized(a["amount"])
			if err != nil {
				continue
			}
			for _, coin := range coins {
				if !coin.IsPositive() {
					continue
				}
				out = append(out, TaxableEvent{
					Category: string(CategoryReward), ToAddr: delegator,
					Amount: coin.Amount.String(), Denom: coin.Denom,
					ValidatorAddress: a["validator"], RewardTrigger: trigger,
				})
			}
		}
	}
	return out
}

// Older SDK ABCI logs combine same-type events into one ordered attribute list.
// Split repeated reward fields instead of flattening away all but the last
// validator. Modern per-event logs are the single-group case. Delegator was
// absent in older SDKs; the message supplies that identity in that case.
func rewardAttributeGroups(event indexerTxTypes.LogMessageEvent) ([]map[string]string, error) {
	var groups []map[string]string
	current := map[string]string{}
	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		if _, ok := current["amount"]; !ok || current["validator"] == "" {
			return errors.New("tax: incomplete withdraw_rewards event")
		}
		groups = append(groups, current)
		current = map[string]string{}
		return nil
	}
	for _, a := range event.Attributes {
		if a.Key != "amount" && a.Key != "validator" && a.Key != "delegator" {
			continue
		}
		if _, exists := current[a.Key]; exists {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		current[a.Key] = a.Value
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, errors.New("tax: empty withdraw_rewards event")
	}
	return groups, nil
}

// ibcCallbackErrorPrefix is what ibc-go's callbacks middleware prepends to the
// type and attribute keys of events emitted during a callback it had to revert.
const ibcCallbackErrorPrefix = "ibccallbackerror-"

// ibcV2ReceiveEvents turns the fungible_token_packet events of a channel v2
// receive into inbound transfers. The module emits one per payload with the
// sender, receiver, denom and amount already extracted, which is exactly what
// the v1 path digs out of the packet's JSON data.
//
// Unlike v1, the event carries a success flag, so a receive that wrote an error
// acknowledgement and moved no funds is not recorded as an acquisition.
func ibcV2ReceiveEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		// When a destination callback fails, ibc-go's callbacks middleware
		// reverts the callback and re-emits that execution's events with an
		// ibccallbackerror- prefix on the type AND on every attribute key. The
		// transfer itself is not reverted, so the recipient still received the
		// coins and it is still an acquisition. Matching the bare type only
		// silently dropped those.
		if strings.TrimPrefix(ev.Type, ibcCallbackErrorPrefix) != "fungible_token_packet" {
			continue
		}
		var sender, receiver, denom, amount, success string
		for _, a := range ev.Attributes {
			switch strings.TrimPrefix(a.Key, ibcCallbackErrorPrefix) {
			case "sender":
				sender = a.Value
			case "receiver":
				receiver = a.Value
			case "denom":
				denom = a.Value
			case "amount":
				amount = a.Value
			case "success":
				success = a.Value
			}
		}
		if success != "" && success != "true" {
			continue
		}
		if amount == "" || receiver == "" {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryIBCIn),
			FromAddr: sender, ToAddr: receiver,
			Amount: amount, Denom: denom,
		})
	}
	return out
}

// tokenizeShareRewardEvents reads the withdraw_tokenize_share_reward events the
// x/liquid module emits, one per tokenize-share record settled, and attributes
// the income to the record owner. WithdrawAll settles several records in one
// message and so emits several events.
//
// Income is attributed to the owner even when the event's withdraw_address
// differs, matching how redirected staking rewards are attributed to the
// delegator rather than the withdraw address (INF-213).
func tokenizeShareRewardEvents(log *indexerTxTypes.LogMessage, owner string) []TaxableEvent {
	total := sdk.NewCoins()
	for _, ev := range log.Events {
		if ev.Type != "withdraw_tokenize_share_reward" {
			continue
		}
		for _, a := range ev.Attributes {
			if a.Key != "amount" {
				continue
			}
			if coins, err := sdk.ParseCoinsNormalized(a.Value); err == nil {
				total = total.Add(coins...)
			}
		}
	}
	var out []TaxableEvent
	for _, c := range total {
		out = append(out, TaxableEvent{
			Category: string(CategoryReward),
			ToAddr:   owner,
			Amount:   c.Amount.String(), Denom: c.Denom,
		})
	}
	return out
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
