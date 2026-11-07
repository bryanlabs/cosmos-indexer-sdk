package tax

import (
	"sort"
	"strconv"
	"time"

	sdkmath "cosmossdk.io/math"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Block events are coin movements made by the chain itself at the start or
// end of a block, with no transaction behind them: an unbonding that finished,
// a governance deposit refunded or burned when its proposal ended, a community
// pool payout from a passed proposal, an upgrade handler moving funds. Each
// rule reads one block lifecycle (all BeginBlock or all EndBlock events of one
// block) for one wallet. Anything that moves the wallet's coins and matches no
// rule becomes a review row rather than being dropped.

const (
	// govModuleAddress is the Hub's x/gov module account, which holds deposits.
	govModuleAddress = "cosmos10d07y265gmmuvt4z0w9aw880jnsr700j6zn9kn"

	BlockPhaseBegin = "begin_block"
	BlockPhaseEnd   = "end_block"

	// TriggerCommunityPoolSpend names income paid out of the community pool by
	// a passed proposal.
	TriggerCommunityPoolSpend = "community_pool_spend"
	// TriggerBlockWithdrawal names a staking reward the chain withdrew for the
	// delegator outside any transaction.
	TriggerBlockWithdrawal = "block_withdrawal"

	ReviewUnclassifiedBlockMovement = "unclassified_block_movement"
	ReviewGovDepositOutcome         = "gov_deposit_outcome_unverified"

	proposalPassed = "proposal_passed"
)

// BlockEvent is one stored BeginBlock or EndBlock event.
type BlockEvent struct {
	ID         uint
	Height     int64
	Timestamp  time.Time
	Phase      string
	Type       string
	Attributes []txlog.Attribute
}

// GovDeposit is a deposit the wallet made on a proposal, read from its
// gov_deposit rows.
type GovDeposit struct {
	ProposalID string
	Amount     string
	Denom      string
}

type blockCoin struct {
	event  BlockEvent
	amount sdkmath.Int
	denom  string
}

// ClassifyBlockLifecycle classifies one block lifecycle for owner. deposits
// are all of the owner's governance deposits; only those whose proposal ended
// in this lifecycle are settled here. Every row has MessageID 0 and the source
// block event's id as SubIndex.
func ClassifyBlockLifecycle(events []BlockEvent, owner string, deposits []GovDeposit) []TaxableEvent {
	if len(events) == 0 || owner == "" {
		return nil
	}
	var out []TaxableEvent
	classifiedIn := map[string]sdkmath.Int{}
	addIn := func(denom string, amount sdkmath.Int) {
		if v, ok := classifiedIn[denom]; ok {
			classifiedIn[denom] = v.Add(amount)
		} else {
			classifiedIn[denom] = amount
		}
	}

	ended := map[string]string{}
	var endedOrder []string
	govBurn := false
	for _, e := range events {
		a := blockAttrs(e)
		switch e.Type {
		case "active_proposal", "inactive_proposal":
			if id := a[attributeProposalID]; id != "" {
				if _, seen := ended[id]; !seen {
					endedOrder = append(endedOrder, id)
				}
				ended[id] = a["proposal_result"]
			}
		case "burn":
			if a["burner"] == govModuleAddress {
				govBurn = true
			}
		}
	}
	passed := 0
	for _, id := range endedOrder {
		if ended[id] == proposalPassed {
			passed++
		}
	}
	proposalDetail := func(kv map[string]string, onlyPassed bool) {
		var ids []string
		for _, id := range endedOrder {
			if !onlyPassed || ended[id] == proposalPassed {
				ids = append(ids, id)
			}
		}
		if len(ids) == 1 {
			kv[attributeProposalID] = ids[0]
			kv["proposal_result"] = ended[ids[0]]
		}
	}

	var refunds []blockCoin
	for _, e := range events {
		a := blockAttrs(e)
		switch e.Type {
		case "complete_unbonding":
			if a[attributeDelegator] != owner {
				continue
			}
			for _, c := range blockCoins(e, a[attributeAmount]) {
				addIn(c.denom, c.amount)
				out = append(out, blockRow(c, CategoryUnbondingComplete, "", owner, a[attributeValidator], "", nil))
			}
		case "withdraw_rewards":
			if a[attributeDelegator] != owner {
				continue
			}
			for _, c := range blockCoins(e, a[attributeAmount]) {
				addIn(c.denom, c.amount)
				out = append(out, blockRow(c, CategoryReward, "", owner, a[attributeValidator], TriggerBlockWithdrawal, nil))
			}
		case "transfer":
			if a[attributeRecipient] != owner {
				continue
			}
			switch {
			case a[attributeSender] == govModuleAddress && len(endedOrder) > 0:
				refunds = append(refunds, blockCoins(e, a[attributeAmount])...)
			case a[attributeSender] == distributionModuleAddress && e.Phase == BlockPhaseEnd && passed > 0:
				for _, c := range blockCoins(e, a[attributeAmount]) {
					addIn(c.denom, c.amount)
					kv := map[string]string{}
					proposalDetail(kv, true)
					out = append(out, blockRow(c, CategoryIncome, distributionModuleAddress, owner, "", TriggerCommunityPoolSpend, kv))
				}
			}
		}
	}

	refunded := map[string]sdkmath.Int{}
	for _, c := range refunds {
		addIn(c.denom, c.amount)
		if v, ok := refunded[c.denom]; ok {
			refunded[c.denom] = v.Add(c.amount)
		} else {
			refunded[c.denom] = c.amount
		}
		kv := map[string]string{}
		proposalDetail(kv, false)
		out = append(out, blockRow(c, CategoryGovDepositRefund, govModuleAddress, owner, "", "", kv))
	}
	out = append(out, settleGovDeposits(events, owner, deposits, ended, endedOrder, refunded, govBurn)...)
	out = append(out, blockResiduals(events, owner, classifiedIn)...)
	return out
}

// settleGovDeposits closes the owner's deposits on proposals that ended in
// this lifecycle. A deposit is refunded when the gov module paid the owner at
// least the deposited amount, burned when nothing came back and the gov module
// burned coins, and sent to review otherwise.
func settleGovDeposits(events []BlockEvent, owner string, deposits []GovDeposit, ended map[string]string, endedOrder []string, refunded map[string]sdkmath.Int, govBurn bool) []TaxableEvent {
	owed := map[string]sdkmath.Int{}
	var proposals []string
	seen := map[string]bool{}
	for _, d := range deposits {
		if _, ok := ended[d.ProposalID]; !ok {
			continue
		}
		amount, ok := sdkmath.NewIntFromString(d.Amount)
		if !ok || !amount.IsPositive() {
			continue
		}
		if v, ok := owed[d.Denom]; ok {
			owed[d.Denom] = v.Add(amount)
		} else {
			owed[d.Denom] = amount
		}
		if !seen[d.ProposalID] {
			seen[d.ProposalID] = true
			proposals = append(proposals, d.ProposalID)
		}
	}
	if len(owed) == 0 {
		return nil
	}
	anchor := endEvent(events, proposals)
	anyRefund := false
	for _, v := range refunded {
		if v.IsPositive() {
			anyRefund = true
		}
	}
	var out []TaxableEvent
	for _, denom := range sortedDenoms(owed) {
		gap := owed[denom]
		if r, ok := refunded[denom]; ok {
			gap = gap.Sub(r)
		}
		if !gap.IsPositive() {
			continue
		}
		kv := map[string]string{}
		if len(proposals) == 1 {
			kv[attributeProposalID] = proposals[0]
			kv["proposal_result"] = ended[proposals[0]]
		}
		if anyRefund || !govBurn {
			kv["review_required"] = attributeTrue
			kv["review_reason"] = ReviewGovDepositOutcome
		}
		out = append(out, blockRow(blockCoin{event: anchor, amount: gap, denom: denom}, CategoryGovDepositBurn, owner, govModuleAddress, "", "", kv))
	}
	return out
}

// blockResiduals compares what the bank module says the owner received and
// spent in the lifecycle against the classified rows. Chains before SDK 0.46
// emit no coin_received or coin_spent events, so nothing is checked there.
func blockResiduals(events []BlockEvent, owner string, classifiedIn map[string]sdkmath.Int) []TaxableEvent {
	received := map[string][]blockCoin{}
	spent := map[string][]blockCoin{}
	bankEvents := false
	for _, e := range events {
		a := blockAttrs(e)
		switch e.Type {
		case "coin_received":
			bankEvents = true
			if a[attributeReceiver] == owner {
				for _, c := range blockCoins(e, a[attributeAmount]) {
					received[c.denom] = append(received[c.denom], c)
				}
			}
		case "coin_spent":
			bankEvents = true
			if a["spender"] == owner {
				for _, c := range blockCoins(e, a[attributeAmount]) {
					spent[c.denom] = append(spent[c.denom], c)
				}
			}
		}
	}
	if !bankEvents {
		return nil
	}
	review := map[string]string{"review_required": attributeTrue, "review_reason": ReviewUnclassifiedBlockMovement}
	var out []TaxableEvent
	denoms := map[string]bool{}
	for d := range received {
		denoms[d] = true
	}
	for d := range classifiedIn {
		denoms[d] = true
	}
	for _, denom := range sortedDenoms(denoms) {
		total := sdkmath.ZeroInt()
		for _, c := range received[denom] {
			total = total.Add(c.amount)
		}
		classified := sdkmath.ZeroInt()
		if v, ok := classifiedIn[denom]; ok {
			classified = v
		}
		gap := total.Sub(classified)
		if gap.IsZero() {
			continue
		}
		anchor := events[0]
		if len(received[denom]) > 0 {
			anchor = received[denom][0].event
		}
		kv := map[string]string{"review_required": attributeTrue, "review_reason": ReviewUnclassifiedBlockMovement}
		if gap.IsNegative() {
			kv["classified_exceeds_received"] = attributeTrue
			gap = gap.Neg()
		}
		out = append(out, blockRow(blockCoin{event: anchor, amount: gap, denom: denom}, CategoryTransfer, "", owner, "", "", kv))
	}
	for _, denom := range sortedDenoms(spent) {
		for _, c := range spent[denom] {
			out = append(out, blockRow(c, CategoryTransfer, owner, "", "", "", review))
		}
	}
	return out
}

func blockRow(c blockCoin, category Category, from, to, validator, trigger string, kv map[string]string) TaxableEvent {
	detail := map[string]string{
		"source":         "block_event",
		"phase":          c.event.Phase,
		"block_event_id": strconv.FormatUint(uint64(c.event.ID), 10),
		"event_type":     c.event.Type,
	}
	for k, v := range kv {
		detail[k] = v
	}
	return TaxableEvent{
		SubIndex:         int(c.event.ID),
		Category:         string(category),
		Amount:           c.amount.String(),
		Denom:            c.denom,
		FromAddr:         from,
		ToAddr:           to,
		ValidatorAddress: validator,
		RewardTrigger:    trigger,
		Detail:           detailJSON(detail),
		BlockHeight:      c.event.Height,
		Timestamp:        c.event.Timestamp,
	}
}

// blockAttrs keeps the first value of each key, as the tx-side helpers do.
func blockAttrs(e BlockEvent) map[string]string {
	a := make(map[string]string, len(e.Attributes))
	for _, attr := range e.Attributes {
		if _, ok := a[attr.Key]; !ok {
			a[attr.Key] = attr.Value
		}
	}
	return a
}

func blockCoins(e BlockEvent, amount string) []blockCoin {
	if amount == "" {
		return nil
	}
	// An amount the SDK cannot parse yields no row here; the bank residual
	// check still sees the coins the owner received or spent.
	coins, err := sdk.ParseCoinsNormalized(amount)
	if err != nil {
		return nil
	}
	out := make([]blockCoin, 0, len(coins))
	for _, c := range coins {
		if c.Amount.IsPositive() {
			out = append(out, blockCoin{event: e, amount: c.Amount, denom: c.Denom})
		}
	}
	return out
}

// endEvent is the active_proposal or inactive_proposal event that ended the
// first of proposals, so a settled deposit points at where it was decided.
func endEvent(events []BlockEvent, proposals []string) BlockEvent {
	want := map[string]bool{}
	for _, p := range proposals {
		want[p] = true
	}
	for _, e := range events {
		if e.Type == "active_proposal" || e.Type == "inactive_proposal" {
			if want[blockAttrs(e)[attributeProposalID]] {
				return e
			}
		}
	}
	return events[0]
}

func sortedDenoms[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for d := range m {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
