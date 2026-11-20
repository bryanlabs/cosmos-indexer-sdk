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
	actionMint             = "mint"
	TriggerLPWithdraw      = "lp_withdraw"
	TriggerLimitOrder      = "limit_order"
	TriggerLimitFill       = "limit_fill"
	TriggerLimitCancel     = "limit_cancel"
	TriggerLimitFee        = "limit_fee"

	streamExitCancelled = "exit_cancelled"

	triggerNameRegistration = "name_registration"
	triggerDenomCreationFee = "denom_creation_fee"
	triggerListingFee       = "listing_fee"
	cw20DenomPrefix         = "cw20:"
	detailShare             = "share"
	detailOrderID           = "order_id"
	detailSide              = "side"
	detailOpenQty           = "open_qty"
	detailPrice             = "price"
	detailReviewRequired    = "review_required"
	detailReviewReason      = "review_reason"

	// ReviewStreamSpend marks a stream exit: the message names what the
	// subscriber bought but not the coin it was paid with.
	ReviewStreamSpend = "stream_spend_not_in_message"
	// ReviewLimitMakerSpend marks what a resting limit order's owner received
	// when another order filled it: the message names the payout but not the
	// order it came from.
	ReviewLimitMakerSpend = "limit_maker_spend_not_in_message"
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
// the pool for the shares it mints (a transfer follows when it mints them
// to a receiver other than the sender), or the shares sent back
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
		token, ok := cw20Moved(log, actionMint, "", receiver, a[detailShare])
		if !ok {
			detail := contractDetail(contract, map[string]string{detailShare: a[detailShare], "pool": contract})
			return coinRows(CategoryProtocolDeposit, coins, sender, contract, TriggerLPProvide, detail)
		}
		detail := contractDetail(contract, map[string]string{detailShare: a[detailShare], "pool_shares": token})
		out := coinRows(CategorySwap, coins, sender, contract, TriggerLPProvide, detail)
		out = append(out, TaxableEvent{Category: string(CategorySwap), FromAddr: contract, ToAddr: sender, Amount: a[detailShare], Denom: token, RewardTrigger: TriggerLPProvide, Detail: detail})
		if receiver != sender {
			// The sender bought the shares and the pool minted them
			// straight to another address: a swap to the sender, then the
			// sender's transfer of the shares to the receiver.
			out = append(out, TaxableEvent{Category: string(CategoryTransfer), FromAddr: sender, ToAddr: receiver, Amount: a[detailShare], Denom: token, RewardTrigger: TriggerLPProvide, Detail: detail})
		}
		return out
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
// once is a swap, the rest waits in the book as a deposit keyed by contract
// and order id, and a cancel returns what was left of it. Coins of the paid
// denom the book sends back at once (a better price than the limit) only
// shrink the deposit. A trading fee belongs to the trade it paid for: what
// the book kept of a buy beyond its matched quote (the taker fee and the
// fee reserve of the matched part) is part of the swap's cost, as a sell's
// fee already is of its net quote. A buy's deposit is the open quantity at
// the limit price plus that quantity's share of the reserve the order
// paid. The deposit names the side, the limit price and the base quantity
// left open, which the report reads to pair later maker fills with the lot.
func limitOrderEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	a, ok := defiEvent(log, func(a map[string]string) bool {
		if a[detailOrderID] == "" {
			return false
		}
		switch a[attributeAction] {
		case "place_limit":
			return a[detailSide] != "" && a["qty"] != "" && a[detailPrice] != "" && a["matched_base"] != "" && a["matched_quote"] != ""
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
	back := legsBetween(legs, contract, sender)
	if a[attributeAction] == "cancel" {
		if len(back) != 1 || namesAny(known, sender, back) {
			return nil
		}
		detail := contractDetail(contract, map[string]string{detailOrderID: a[detailOrderID], "refund_qty": a["refund_qty"]})
		return coinRows(CategoryProtocolWithdrawal, back, contract, sender, TriggerLimitCancel, detail)
	}
	paid := legsBetween(legs, sender, contract)
	if len(paid) != 1 || namesAny(known, sender, paid) || namesAny(known, sender, back) {
		return nil
	}
	side := a[detailSide]
	matched := a["matched_quote"]
	switch side {
	case "sell":
		matched = a["matched_base"]
	case "buy":
	default:
		return nil
	}
	qty, ok1 := sdkmath.NewIntFromString(a["qty"])
	base, ok2 := sdkmath.NewIntFromString(a["matched_base"])
	spent, ok3 := sdkmath.NewIntFromString(matched)
	if !ok1 || !ok2 || !ok3 || spent.IsNegative() || base.IsNegative() || base.GT(qty) {
		return nil
	}
	denom := paid[0].Denom
	improved := back.AmountOf(denom)
	got := back.Sub(sdk.NewCoin(denom, improved))
	left := paid[0].Amount.Sub(spent).Sub(improved)
	if left.IsNegative() || (spent.IsPositive() && got.Empty()) {
		return nil
	}
	open := qty.Sub(base)
	deposit := left
	if side == "buy" {
		var ok bool
		if deposit, ok = limitBuyDeposit(paid[0].Amount, qty, open, a[detailPrice]); !ok || deposit.GT(left) {
			return nil
		}
	} else if open.IsZero() && left.IsPositive() {
		return nil
	}
	cost := spent.Add(left).Sub(deposit)
	detail := contractDetail(contract, map[string]string{detailOrderID: a[detailOrderID]})
	out := coinRows(CategorySwap, sdk.Coins{sdk.NewCoin(denom, cost)}, sender, contract, TriggerLimitFill, detail)
	if open.IsPositive() {
		lot := contractDetail(contract, map[string]string{detailOrderID: a[detailOrderID], detailSide: side, detailPrice: a[detailPrice], detailOpenQty: open.String()})
		out = append(out, coinRows(CategoryProtocolDeposit, sdk.Coins{sdk.NewCoin(denom, deposit)}, sender, contract, TriggerLimitOrder, lot)...)
	}
	out = append(out, coinRows(CategorySwap, got, contract, sender, TriggerLimitFill, detail)...)
	return append(out, limitMakerRows(legs, contract, sender, denom, spent)...)
}

// limitBuyDeposit is what a buy order leaves in the book for its open base
// quantity: that quantity at the limit price, plus its share of the reserve
// the order paid beyond its whole quantity at the limit price.
func limitBuyDeposit(paid, qty, open sdkmath.Int, price string) (sdkmath.Int, bool) {
	p, ok := sdkmath.NewIntFromString(price)
	if !ok || !p.IsPositive() || !qty.IsPositive() {
		return sdkmath.Int{}, false
	}
	scale := sdkmath.NewIntWithDecimal(1, 12)
	reserve := paid.Sub(qty.Mul(p).Quo(scale))
	if reserve.IsNegative() {
		return sdkmath.Int{}, false
	}
	return open.Mul(p).Quo(scale).Add(reserve.Mul(open).Quo(qty)), true
}

// limitMakerRows records what the resting orders this order matched paid
// their owners: the coins of the paid denom the book sent to other wallets
// in the order it sent them, until they add up to what the order spent. A
// fee the book takes in the same denom comes after them and is not a maker.
// The maker's own order and price are not in the message, so each row asks
// for review until the report pairs it with the maker's open order.
func limitMakerRows(legs []coinTransfer, contract, sender, denom string, spent sdkmath.Int) []TaxableEvent {
	if !spent.IsPositive() {
		return nil
	}
	paid := map[string]sdkmath.Int{}
	var makers []string
	acc := sdkmath.ZeroInt()
	for _, l := range legs {
		amt := l.coins.AmountOf(denom)
		if acc.Equal(spent) {
			break
		}
		if l.from != contract || l.to == sender || l.to == contract || !amt.IsPositive() {
			continue
		}
		if _, seen := paid[l.to]; !seen {
			paid[l.to] = sdkmath.ZeroInt()
			makers = append(makers, l.to)
		}
		paid[l.to] = paid[l.to].Add(amt)
		acc = acc.Add(amt)
	}
	if !acc.Equal(spent) {
		return nil
	}
	detail := contractDetail(contract, map[string]string{"maker": attributeTrue, detailReviewRequired: attributeTrue, detailReviewReason: ReviewLimitMakerSpend})
	var out []TaxableEvent
	for _, m := range makers {
		out = append(out, coinRows(CategorySwap, sdk.Coins{sdk.NewCoin(denom, paid[m])}, contract, m, TriggerLimitFill, detail)...)
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
