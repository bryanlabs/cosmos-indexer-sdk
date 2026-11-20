package tax

import (
	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// TriggerWithdrawAddress marks the transfer from a delegator to the withdraw
// address the distribution module paid its reward to.
const TriggerWithdrawAddress = "withdraw_address"

const detailRewardTrigger = "reward_trigger"

// Reasons the parser books no withdraw-address transfer for a delegator's
// rewards, because the message's payments do not show where they went.
const (
	// WithdrawRefusedPaidDelegator: the module paid the delegator itself and
	// another account in the same message, so the other payment may be
	// someone else's reward.
	WithdrawRefusedPaidDelegator = "paid_delegator"
	// WithdrawRefusedManyRecipients: the module paid more than one other
	// account, so which one holds this delegator's reward is a guess.
	WithdrawRefusedManyRecipients = "many_recipients"
)

// withdrawAddressPaid returns rewards unchanged and queues, in state, the
// movement of those coins to the delegator's withdraw address. The reward
// rows name the delegator, whose income it is (INF-213), but the chain paid
// the coins elsewhere. Without a transfer the delegator's lots hold coins it
// never had and the withdraw address receives coins from nowhere. The move
// is booked only when the distribution module paid exactly one account in
// the message and that account is not the delegator; it carries the booked
// reward per denom, at most what that account was paid. Anything else is a
// refusal kept in state. The rows wait until the top-level message is done,
// so they follow every existing row, and an authz inner message keeps its
// own delegator.
func withdrawAddressPaid(state *classificationState, log *indexerTxTypes.LogMessage, delegator, trigger string, rewards []TaxableEvent) []TaxableEvent {
	moves, refused := withdrawAddressEvents(log, delegator, trigger, rewards)
	state.paid = append(state.paid, moves...)
	if refused != "" {
		state.refused = append(state.refused, refused)
	}
	return rewards
}

func withdrawAddressEvents(log *indexerTxTypes.LogMessage, delegator, trigger string, rewards []TaxableEvent) ([]TaxableEvent, string) {
	if log == nil || delegator == "" {
		return nil, ""
	}
	booked := map[string]sdkmath.Int{}
	var denoms []string
	for _, r := range rewards {
		if r.Category != string(CategoryReward) || r.ToAddr != delegator {
			continue
		}
		amount, ok := sdkmath.NewIntFromString(r.Amount)
		if !ok || !amount.IsPositive() {
			continue
		}
		if cur, seen := booked[r.Denom]; seen {
			booked[r.Denom] = cur.Add(amount)
		} else {
			booked[r.Denom] = amount
			denoms = append(denoms, r.Denom)
		}
	}
	if len(booked) == 0 {
		return nil, ""
	}
	paidSelf := false
	recipient := ""
	paid := map[string]sdkmath.Int{}
	for _, p := range transferRuns(log, distributionModuleAddress) {
		if !p.coins.IsAllPositive() {
			continue
		}
		if p.recipient == delegator {
			paidSelf = true
			continue
		}
		if recipient != "" && recipient != p.recipient {
			return nil, WithdrawRefusedManyRecipients
		}
		recipient = p.recipient
		for _, c := range p.coins {
			if cur, seen := paid[c.Denom]; seen {
				paid[c.Denom] = cur.Add(c.Amount)
			} else {
				paid[c.Denom] = c.Amount
			}
		}
	}
	switch {
	case recipient == "":
		return nil, ""
	case paidSelf:
		return nil, WithdrawRefusedPaidDelegator
	}
	out := make([]TaxableEvent, 0, len(denoms))
	for _, denom := range denoms {
		got, ok := paid[denom]
		if !ok || !got.IsPositive() {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryTransfer), FromAddr: delegator, ToAddr: recipient,
			Amount: sdkmath.MinInt(got, booked[denom]).String(), Denom: denom, RewardTrigger: TriggerWithdrawAddress,
			Detail: detailJSON(map[string]string{detailProtocol: moduleDistribution, detailRewardTrigger: trigger}),
		})
	}
	return out, ""
}

// WithdrawAddressRefusals classifies a message as the parser does and counts,
// per reason, the delegators whose rewards got no withdraw-address transfer.
func WithdrawAddressRefusals(cosmosMsg sdk.Msg, log *indexerTxTypes.LogMessage) map[string]int {
	state := &classificationState{}
	classifyWithState(cosmosMsg, log, state, 0, "")
	out := map[string]int{}
	for _, r := range state.refused {
		out[r]++
	}
	return out
}
