package tax

import (
	"strings"

	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Triggers of the streamswap, liquidity pool and limit order supplement rows.
const (
	TriggerStreamSubscribe = "stream_subscribe"
	TriggerStreamWithdraw  = "stream_withdraw"
	TriggerStreamExit      = "stream_exit"
	TriggerLPProvide       = "lp_provide"
	TriggerLPWithdraw      = "lp_withdraw"
	TriggerLimitOrder      = "limit_order"
	TriggerLimitFill       = "limit_fill"
	TriggerLimitCancel     = "limit_cancel"

	triggerNameRegistration = "name_registration"
	triggerDenomCreationFee = "denom_creation_fee"
	detailReviewRequired    = "review_required"
	detailReviewReason      = "review_reason"

	// ReviewStreamSpend marks a stream exit: the message names what the
	// subscriber bought but not the coin it was paid with.
	ReviewStreamSpend = "stream_spend_not_in_message"
)

// defiEvent is the one contract event a supplement rule reads: the wasm
// event of the action it handles, alone in its message.
func defiEvent(log *indexerTxTypes.LogMessage, match func(map[string]string) bool) (map[string]string, bool) {
	var found map[string]string
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		if !match(a) {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = a
	}
	return found, found != nil
}

// streamswapEvents records a streamswap subscription as coins the
// subscriber still owns until the stream ends: subscribing deposits them,
// withdrawing takes the unspent part back, and exiting a finished stream
// pays out what they bought, which the subscription paid for. Every row is
// a bank leg between the signer and the stream contract in the message.
func streamswapEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	a, ok := defiEvent(log, func(a map[string]string) bool {
		if a["stream_id"] == "" {
			return false
		}
		switch a[attributeAction] {
		case "subscribe", "subscribe_pending", liquidityKindWithdraw, "withdraw_pending", "exit_stream":
			return true
		}
		return false
	})
	if !ok {
		return nil
	}
	contract := a[attributeContractAddress]
	legs := transfersIn(log)
	detail := map[string]string{"stream_id": a["stream_id"]}
	switch a[attributeAction] {
	case "subscribe", "subscribe_pending":
		coins := legsBetween(legs, sender, contract)
		if len(coins) != 1 || namesAny(known, sender, coins) {
			return nil
		}
		return coinRows(CategoryProtocolDeposit, coins, sender, contract, TriggerStreamSubscribe, contractDetail(contract, detail))
	case liquidityKindWithdraw, "withdraw_pending":
		coins := legsBetween(legs, contract, sender)
		if len(coins) != 1 || namesAny(known, sender, coins) {
			return nil
		}
		return coinRows(CategoryProtocolWithdrawal, coins, contract, sender, TriggerStreamWithdraw, contractDetail(contract, detail))
	}
	paid := legsBetween(legs, contract, sender)
	if paid.Empty() || namesAny(known, sender, paid) {
		return nil
	}
	bought, refund := paid, sdk.Coins{}
	if p := a["purchased"]; p != "" {
		amt, ok := sdkmath.NewIntFromString(p)
		if !ok {
			return nil
		}
		bought = sdk.Coins{}
		for _, c := range paid {
			if c.Amount.Equal(amt) && bought.Empty() {
				bought = bought.Add(c)
			} else {
				refund = refund.Add(c)
			}
		}
		if bought.Empty() {
			return nil
		}
	} else if len(paid) != 1 {
		return nil
	}
	out := coinRows(CategoryProtocolWithdrawal, refund, contract, sender, TriggerStreamWithdraw, contractDetail(contract, detail))
	exit := map[string]string{"stream_id": a["stream_id"], "spent": a["spent"], detailReviewRequired: attributeTrue, detailReviewReason: ReviewStreamSpend}
	return append(out, coinRows(CategorySwap, bought, contract, sender, TriggerStreamExit, contractDetail(contract, exit))...)
}

// liquidityPoolEvents records a constant-product pool deposit or withdrawal
// the way Gravity DEX pool movements are recorded, as swaps: the coins the
// signer put into the pool for its pool shares, or the coins the pool paid
// the signer for burned shares.
func liquidityPoolEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	a, ok := defiEvent(log, func(a map[string]string) bool {
		switch a[attributeAction] {
		case "provide_liquidity":
			return a["share"] != ""
		case "withdraw_liquidity":
			return a["withdrawn_share"] != ""
		}
		return false
	})
	if !ok || a[attributeSender] != sender {
		return nil
	}
	contract := a[attributeContractAddress]
	legs := transfersIn(log)
	detail := contractDetail(contract, map[string]string{"share": firstNonEmpty(a["share"], a["withdrawn_share"])})
	if a[attributeAction] == "provide_liquidity" {
		coins := legsBetween(legs, sender, contract)
		if coins.Empty() || namesAny(known, sender, coins) {
			return nil
		}
		return coinRows(CategorySwap, coins, sender, contract, TriggerLPProvide, detail)
	}
	coins := legsBetween(legs, contract, sender)
	if coins.Empty() || namesAny(known, sender, coins) {
		return nil
	}
	return coinRows(CategorySwap, coins, contract, sender, TriggerLPWithdraw, detail)
}

// limitOrderEvents records an order book limit order: the part matched at
// once is a swap, the rest waits in the book as a deposit, and a cancel
// returns what was left of it.
func limitOrderEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	a, ok := defiEvent(log, func(a map[string]string) bool {
		if a["order_id"] == "" {
			return false
		}
		switch a[attributeAction] {
		case "place_limit":
			return a["side"] != "" && a["matched_base"] != "" && a["matched_quote"] != ""
		case "cancel":
			return a["refund_qty"] != ""
		}
		return false
	})
	if !ok {
		return nil
	}
	contract := a[attributeContractAddress]
	legs := transfersIn(log)
	detail := contractDetail(contract, map[string]string{"order_id": a["order_id"]})
	back := legsBetween(legs, contract, sender)
	if a[attributeAction] == "cancel" {
		if back.Empty() || namesAny(known, sender, back) {
			return nil
		}
		return coinRows(CategoryProtocolWithdrawal, back, contract, sender, TriggerLimitCancel, detail)
	}
	paid := legsBetween(legs, sender, contract)
	if len(paid) != 1 || namesAny(known, sender, paid) || namesAny(known, sender, back) {
		return nil
	}
	matched := a["matched_quote"]
	if a["side"] == "sell" {
		matched = a["matched_base"]
	}
	spent, ok := sdkmath.NewIntFromString(matched)
	if !ok || spent.IsNegative() || spent.GT(paid[0].Amount) {
		return nil
	}
	if spent.IsPositive() && back.Empty() {
		return nil
	}
	denom := paid[0].Denom
	out := coinRows(CategorySwap, sdk.Coins{sdk.NewCoin(denom, spent)}, sender, contract, TriggerLimitFill, detail)
	out = append(out, coinRows(CategoryProtocolDeposit, sdk.Coins{sdk.NewCoin(denom, paid[0].Amount.Sub(spent))}, sender, contract, TriggerLimitOrder, detail)...)
	return append(out, coinRows(CategorySwap, back, contract, sender, TriggerLimitFill, detail)...)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
