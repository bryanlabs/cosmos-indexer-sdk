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
	TriggerStreamRefund    = "stream_refund"
	TriggerLPProvide       = "lp_provide"
	TriggerLPWithdraw      = "lp_withdraw"
	TriggerLimitOrder      = "limit_order"
	TriggerLimitFill       = "limit_fill"
	TriggerLimitCancel     = "limit_cancel"

	streamExitCancelled = "exit_cancelled"

	triggerNameRegistration = "name_registration"
	triggerDenomCreationFee = "denom_creation_fee"
	triggerListingFee       = "listing_fee"
	cw20DenomPrefix         = "cw20:"
	detailShare             = "share"
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
		case "subscribe", "subscribe_pending", liquidityKindWithdraw, "withdraw_pending", "exit_stream", streamExitCancelled:
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
	case streamExitCancelled:
		// A cancelled stream, or one that missed its threshold, spent
		// nothing: the subscriber takes the whole subscription back.
		coins := legsBetween(legs, contract, sender)
		if len(coins) != 1 || namesAny(known, sender, coins) {
			return nil
		}
		return coinRows(CategoryProtocolWithdrawal, coins, contract, sender, TriggerStreamRefund, contractDetail(contract, detail))
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
// the way Gravity DEX pool movements and Inflow vault shares are recorded,
// as swaps between the coins and the pool's cw20 shares: the coins put into
// the pool for the shares it mints to the receiver, or the shares sent back
// and burned for the coins the pool pays out. The shares are carried at the
// cost of what was given for them. When the log does not show the shares
// reaching the receiver (a pool that locks them), the coins are a deposit
// the wallet still owns in that pool, and a withdrawal brings them back.
func liquidityPoolEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	a, ok := defiEvent(log, func(a map[string]string) bool {
		switch a[attributeAction] {
		case "provide_liquidity":
			return a[detailShare] != ""
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
	if a[attributeAction] == "provide_liquidity" {
		coins := legsBetween(legs, sender, contract)
		if coins.Empty() || namesAny(known, sender, coins) {
			return nil
		}
		receiver := firstNonEmpty(a["receiver"], sender)
		token, ok := cw20Moved(log, "mint", "", receiver, a[detailShare])
		if !ok {
			detail := contractDetail(contract, map[string]string{detailShare: a[detailShare], "pool": contract})
			return coinRows(CategoryProtocolDeposit, coins, sender, contract, TriggerLPProvide, detail)
		}
		detail := contractDetail(contract, map[string]string{detailShare: a[detailShare], "pool_shares": token})
		out := coinRows(CategorySwap, coins, sender, contract, TriggerLPProvide, detail)
		return append(out, TaxableEvent{Category: string(CategorySwap), FromAddr: contract, ToAddr: receiver, Amount: a[detailShare], Denom: token, RewardTrigger: TriggerLPProvide, Detail: detail})
	}
	coins := legsBetween(legs, contract, sender)
	if coins.Empty() || namesAny(known, sender, coins) {
		return nil
	}
	share := a["withdrawn_share"]
	token, sent := cw20Moved(log, "send", sender, contract, share)
	if burned, ok := cw20Moved(log, "burn", "", "", share); !sent || !ok || burned != token {
		detail := contractDetail(contract, map[string]string{detailShare: share, "pool": contract})
		return coinRows(CategoryProtocolWithdrawal, coins, contract, sender, TriggerLPWithdraw, detail)
	}
	detail := contractDetail(contract, map[string]string{detailShare: share, "pool_shares": token})
	out := []TaxableEvent{{Category: string(CategorySwap), FromAddr: sender, ToAddr: contract, Amount: share, Denom: token, RewardTrigger: TriggerLPWithdraw, Detail: detail}}
	return append(out, coinRows(CategorySwap, coins, contract, sender, TriggerLPWithdraw, detail)...)
}

// cw20Moved finds the one cw20 event of the action for exactly amount, from
// and to the addresses given (an empty one is not checked), and returns its
// token as a cw20: denom.
func cw20Moved(log *indexerTxTypes.LogMessage, action, from, to, amount string) (string, bool) {
	if amt, ok := sdkmath.NewIntFromString(amount); !ok || !amt.IsPositive() {
		return "", false
	}
	found := ""
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		if a[attributeAction] != action || a[attributeAmount] != amount || a[attributeContractAddress] == "" ||
			(from != "" && a["from"] != from) || (to != "" && a["to"] != to) {
			continue
		}
		if found != "" {
			return "", false
		}
		found = cw20DenomPrefix + a[attributeContractAddress]
	}
	return found, found != ""
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
