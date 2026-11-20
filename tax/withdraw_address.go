package tax

import (
	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// TriggerWithdrawAddress marks the transfer from a delegator to the withdraw
// address the distribution module paid its reward to.
const TriggerWithdrawAddress = "withdraw_address"

const detailRewardTrigger = "reward_trigger"

// withdrawAddressPaid returns rewards unchanged and queues, in state, the
// movement of those coins to the delegator's withdraw address. The reward
// rows name the delegator, whose income it is (INF-213), but the chain paid
// the coins elsewhere. Without a transfer the delegator's lots hold coins it
// never had and the withdraw address receives coins from nowhere. Each
// distribution payment to an account other than the delegator moves up to
// the reward booked for that denom; payments beyond it are not this
// delegator's reward. The rows wait until the top-level message is done, so
// they follow every existing row, and an authz inner message keeps its own
// delegator.
func withdrawAddressPaid(state *classificationState, log *indexerTxTypes.LogMessage, delegator, trigger string, rewards []TaxableEvent) []TaxableEvent {
	state.paid = append(state.paid, withdrawAddressEvents(log, delegator, trigger, rewards)...)
	return rewards
}

func withdrawAddressEvents(log *indexerTxTypes.LogMessage, delegator, trigger string, rewards []TaxableEvent) []TaxableEvent {
	if log == nil || delegator == "" {
		return nil
	}
	booked := map[string]sdkmath.Int{}
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
		}
	}
	if len(booked) == 0 {
		return nil
	}
	type paidKey struct{ to, denom string }
	moved := map[paidKey]sdkmath.Int{}
	var order []paidKey
	for _, p := range transferRuns(log, distributionModuleAddress) {
		if p.recipient == delegator {
			continue
		}
		for _, c := range p.coins {
			left, ok := booked[c.Denom]
			if !ok || !left.IsPositive() || !c.IsPositive() {
				continue
			}
			take := sdkmath.MinInt(c.Amount, left)
			booked[c.Denom] = left.Sub(take)
			k := paidKey{p.recipient, c.Denom}
			if cur, seen := moved[k]; seen {
				moved[k] = cur.Add(take)
			} else {
				moved[k] = take
				order = append(order, k)
			}
		}
	}
	out := make([]TaxableEvent, 0, len(order))
	for _, k := range order {
		out = append(out, TaxableEvent{
			Category: string(CategoryTransfer), FromAddr: delegator, ToAddr: k.to,
			Amount: moved[k].String(), Denom: k.denom, RewardTrigger: TriggerWithdrawAddress,
			Detail: detailJSON(map[string]string{detailProtocol: moduleDistribution, detailRewardTrigger: trigger}),
		})
	}
	return out
}
