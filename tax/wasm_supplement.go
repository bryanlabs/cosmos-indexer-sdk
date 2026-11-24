package tax

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/address"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

// Triggers and detail the wasm supplement writes.
const (
	attrCollection     = "collection"
	attrTokenID        = "token_id"
	moduleDistribution = "distribution"
)

const (
	triggerBidFilled  = "bid_filled"
	saleCollectionBid = "collection_bid"
	detailFallback    = "fallback"
	fallbackBank      = "bank"
)

// Stargaze marketplace bid events. A bid escrows the bidder's coins in the
// marketplace until it is removed, replaced or filled.
const (
	eventSetBid              = "wasm-set-bid"
	eventSetAsk              = "wasm-set-ask"
	actionTransferNFT        = "transfer_nft"
	eventUpdateBid           = "wasm-update-bid"
	eventRemoveBid           = "wasm-remove-bid"
	eventSetCollectionBid    = "wasm-set-collection-bid"
	eventUpdateCollectionBid = "wasm-update-collection-bid"
	eventRemoveCollectionBid = "wasm-remove-collection-bid"
	eventFinalizeSale        = "wasm-finalize-sale"
)

// wasmSupplementEvents are the rows a contract message adds after every row
// the known patterns wrote for it: marketplace bid escrow, top-level name
// registration, a tokenfactory fee a contract forwards, streamswap
// subscriptions, pool deposits and withdrawals, limit orders, and last the native
// coin any wallet gained or lost that no row explains. They come after the
// known rows so those keep their positions in the message.
func wasmSupplementEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if log == nil || len(log.Events) == 0 {
		return nil
	}
	// A flattened log merges every event of a type into one, so the order
	// of bank legs and contract events and the signer are lost. Only the
	// order-free fallback runs there, and only for 20-byte wallets.
	var out []TaxableEvent
	settled := lsmSettlementEvents(lsmRecordSettlements(log, sender))
	if flattenedLog(log) {
		return append(settled, bankFallbackEvents(log, "", known)...)
	}
	out = append(out, marketplaceBidEvents(log, sender, known)...)
	out = append(out, askSaleEvents(log, known)...)
	out = append(out, askListingFeeEvents(log, sender, known)...)
	out = append(out, matchListingFeeEvents(log, sender, known)...)
	out = append(out, tldRegistrationEvents(log, sender, known)...)
	out = append(out, forwardedDenomFeeEvents(log, sender, known)...)
	out = append(out, streamswapEvents(log, sender, known)...)
	out = append(out, liquidityPoolEvents(log, sender, known)...)
	out = append(out, limitOrderEvents(log, sender, known)...)
	all := make([]TaxableEvent, 0, len(known)+len(out))
	all = append(all, known...)
	all = append(all, out...)
	// Settlement rows are not in all: a payee's other coins in the message
	// still reach the fallback, which only loses the settled legs.
	out = append(out, settled...)
	out = append(out, bankFallbackEvents(log, sender, all)...)
	return out
}

// WasmSupplementForLog is the supplement rows of one contract message's log
// as wasmSupplementEvents writes them after that message's known rows.
func WasmSupplementForLog(log *indexerTxTypes.LogMessage, sender string) []TaxableEvent {
	if log == nil {
		return nil
	}
	return wasmSupplementEvents(log, sender, wasmExecuteEvents(log, sender))
}

// AuthzWasmSupplementForLog is the supplement rows the indexer writes for
// the contract messages inside a MsgExec, from its stored log alone: each
// inner message's events, in inner message order, as classifyAuthz splits
// them. The granter is not in the log, so no rule that needs the signer
// runs, the same as in the indexer. A log without authz_msg_index cannot be
// split without the transaction body and gets none.
func AuthzWasmSupplementForLog(log *indexerTxTypes.LogMessage) []TaxableEvent {
	return authzWasmSupplement(log, 0)
}

func authzWasmSupplement(log *indexerTxTypes.LogMessage, depth int) []TaxableEvent {
	if log == nil || depth >= maxAuthzDepth || !authzIndexed(log) {
		return nil
	}
	seen := map[int]bool{}
	var idx []int
	for _, ev := range log.Events {
		for _, r := range authzRecords(ev.Attributes) {
			i, err := strconv.Atoi(r.index)
			if err == nil && !seen[i] {
				seen[i] = true
				idx = append(idx, i)
			}
		}
	}
	sort.Ints(idx)
	var out []TaxableEvent
	for _, i := range idx {
		sub := authzSubLog(log, i)
		switch {
		case authzIndexed(sub):
			out = append(out, authzWasmSupplement(sub, depth+1)...)
		case contractMessageLog(sub):
			out = append(out, WasmSupplementForLog(sub, "")...)
		}
	}
	return out
}

// contractMessageLog reports the events of a contract execute, instantiate
// or migrate message.
func contractMessageLog(log *indexerTxTypes.LogMessage) bool {
	for _, ev := range log.Events {
		switch ev.Type {
		case "execute", "instantiate", "migrate":
			return true
		}
	}
	return false
}

// SupplementRow reports a row only the wasm supplement writes, and whether
// it is a bank fallback row: the fallback marker, a marketplace bid row
// named by its bid_id, an accepted ask, or a streamswap, pool, limit order
// or forwarded denom fee row.
func SupplementRow(e TaxableEvent) (supplement, fallback bool) {
	var d map[string]any
	if strings.HasPrefix(e.Detail, "{") {
		_ = json.Unmarshal([]byte(e.Detail), &d)
	}
	if d[detailFallback] == fallbackBank {
		return true, true
	}
	switch e.RewardTrigger {
	case TriggerStreamSubscribe, TriggerStreamWithdraw, TriggerStreamExit, TriggerStreamRefund, TriggerLPProvide, TriggerLPWithdraw,
		TriggerLimitOrder, TriggerLimitFill, TriggerLimitCancel, TriggerLimitFee, triggerListingFee:
		return true, false
	case triggerDenomCreationFee:
		// A MsgCreateDenom signed directly writes the same trigger with no
		// detail; only the fee a contract forwarded names its contract.
		_, ok := d["contract"]
		return ok, false
	case flowTriggerAuctionBid, flowTriggerAuctionRefund, triggerBidFilled:
		_, ok := d["bid_id"]
		return ok, false
	}
	return e.Category == string(CategoryNFTSale) && d[detailSale] == "ask", false
}

// flattenedLog reports a log in the pre-0.50 shape, where each event type
// appears once with the attributes of every event of that type: one event
// naming several contracts, a transfer event with several recipients or a
// message event with several senders.
func flattenedLog(log *indexerTxTypes.LogMessage) bool {
	for _, ev := range log.Events {
		counts := map[string]int{}
		for _, a := range ev.Attributes {
			counts[a.Key]++
		}
		if counts[attributeContractAddress] > 1 {
			return true
		}
		switch ev.Type {
		case string(CategoryTransfer):
			if counts[attributeRecipient] > 1 {
				return true
			}
		case eventMessage:
			if counts[attributeSender] > 1 {
				return true
			}
		}
	}
	return false
}

// logItem is one bank leg or one marketplace event, in the order the chain
// emitted them.
type logItem struct {
	event string
	attrs map[string]string
	leg   *coinTransfer
}

func logItems(log *indexerTxTypes.LogMessage) []logItem {
	var out []logItem
	for _, ev := range log.Events {
		if ev.Type == string(CategoryTransfer) {
			for _, a := range ibcAttributeGroups(ev, attributeSender, attributeRecipient, attributeAmount) {
				coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
				if err == nil && a[attributeRecipient] != "" && !coins.Empty() {
					leg := coinTransfer{from: a[attributeSender], to: a[attributeRecipient], coins: coins}
					out = append(out, logItem{leg: &leg})
				}
			}
			continue
		}
		switch ev.Type {
		case eventSetBid, eventUpdateBid, eventRemoveBid, eventSetCollectionBid, eventUpdateCollectionBid, eventRemoveCollectionBid, eventFinalizeSale:
			out = append(out, logItem{event: ev.Type, attrs: attrMap(ev)})
		}
	}
	return out
}

// marketplaceBidEvents records Stargaze marketplace bid escrow. Placing or
// raising a bid deposits the bid price, a removal or replacement returns the
// old escrow, and an accepted bid releases the escrow to the buyer, who pays
// the seller with it in the existing nft_sale row. Each row's amount is a
// bank leg the message shows: a deposit is written only when the bidder's
// transfer to the marketplace equals the bid price, and a release only when
// the marketplace's payouts equal the sale price. Anything else is left to
// the bank fallback. An accepted bid always releases the escrow at the sale
// price, since the buyer's nft_sale row pays with it; payouts that do not add
// up to the price are noted on the row.
func marketplaceBidEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	items := logItems(log)
	var out []TaxableEvent
	for i, it := range items {
		if it.leg != nil {
			continue
		}
		contract := it.attrs[attributeContractAddress]
		if contract == "" || contractKind(contract) != contractRoleMarketplace {
			continue
		}
		// Legs that belong to this event: the bidder's payment comes just
		// before it and the marketplace's payouts just after, up to the
		// neighbouring marketplace event.
		before, after := eventLegs(items, i)
		sale := "bid"
		if strings.Contains(it.event, attrCollection) {
			sale = saleCollectionBid
		}
		detail := func(id string, extra ...string) string {
			m := map[string]string{"bid_id": id, attrCollection: it.attrs[attrCollection], attrTokenID: it.attrs[attrTokenID], "sale": sale}
			for k := 0; k+1 < len(extra); k += 2 {
				m[extra[k]] = extra[k+1]
			}
			return contractDetail(contract, m)
		}
		switch it.event {
		case eventSetBid, eventSetCollectionBid:
			if dep, price, ok := bidDeposit(it.attrs, contract, before); ok {
				out = append(out, coinRows(CategoryProtocolDeposit, dep, it.attrs["creator"], contract, flowTriggerAuctionBid, depositDetail(detail, it.attrs["id"], price))...)
			}
		case eventUpdateBid, eventUpdateCollectionBid:
			dep, price, ok := bidDeposit(it.attrs, contract, before)
			if !ok {
				continue
			}
			refund := legsBetween(after, contract, it.attrs["creator"])
			out = append(out, coinRows(CategoryProtocolWithdrawal, refund, contract, it.attrs["creator"], flowTriggerAuctionRefund, detail(it.attrs["id"]))...)
			out = append(out, coinRows(CategoryProtocolDeposit, dep, it.attrs["creator"], contract, flowTriggerAuctionBid, depositDetail(detail, it.attrs["id"], price))...)
		case eventRemoveBid, eventRemoveCollectionBid:
			// The event does not name the bidder; a bidder removing their
			// own bid signs the message, and only the payout to them is the
			// returned escrow. Any other payout is left to the bank fallback.
			if it.attrs["id"] == "" || sender == "" {
				continue
			}
			for _, leg := range after {
				if leg.from == contract && leg.to == sender {
					out = append(out, coinRows(CategoryProtocolWithdrawal, leg.coins, contract, leg.to, flowTriggerAuctionRefund, detail(it.attrs["id"]))...)
				}
			}
		case eventFinalizeSale:
			// A sale paid from a bid's escrow: the seller accepted the bid,
			// or set or updated an ask that matched it at once.
			id, s := it.attrs["bid"], "bid"
			switch it.attrs["marketplace_action"] {
			case "accept-bid":
			case "accept-collection-bid":
				id, s = it.attrs[saleCollectionBid], saleCollectionBid
			case "set-ask-match", "update-ask":
				if it.attrs[saleCollectionBid] != "" {
					id, s = it.attrs[saleCollectionBid], saleCollectionBid
				}
			default:
				continue
			}
			sale = s
			buyer, denom := it.attrs["nft_recipient"], it.attrs[attributeDenom]
			price, ok := sdkmath.NewIntFromString(it.attrs["price"])
			if id == "" || buyer == "" || denom == "" || !ok || !price.IsPositive() {
				continue
			}
			paid := sdkmath.ZeroInt()
			for _, leg := range after {
				if leg.from == contract {
					paid = paid.Add(leg.coins.AmountOf(denom))
				}
			}
			// The forward of a matched ask's listing fee is the seller's
			// expense, not a payout of the sale.
			for _, fee := range matchListingFeeEvents(log, sender, known) {
				if fee.Denom == denom {
					if amt, ok := sdkmath.NewIntFromString(fee.Amount); ok {
						paid = paid.Sub(amt)
					}
				}
			}
			d := detail(id)
			if !paid.Equal(price) {
				d = detail(id, "payouts", paid.String()+denom)
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryProtocolWithdrawal), FromAddr: contract, ToAddr: buyer,
				Amount: price.String(), Denom: denom, RewardTrigger: triggerBidFilled, Detail: d,
			})
		}
	}
	return out
}

// askSaleEvents books a listed NFT bought at its asking price on a
// marketplace whose accept_ask event names the seller, buyer, price and
// payouts, as the nft_sale the Stargaze marketplace sale rule writes. It
// holds only when the buyer paid the contract the price in the message, the
// contract paid the seller the proceeds, the payouts add up to the price and
// the collection moved the token to the buyer. The escrow release the known
// rows already record stays, as a record-only nft_transfer.
func askSaleEvents(log *indexerTxTypes.LogMessage, known []TaxableEvent) []TaxableEvent {
	sold := map[string]bool{}
	for _, e := range known {
		if e.Category == string(CategoryNFTSale) {
			sold[e.Asset] = true
		}
	}
	legs := transfersIn(log)
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		contract, seller, buyer, denom := a[attributeContractAddress], a["seller"], a["buyer"], a["price_denom"]
		asset := a[attrCollection] + "/" + a[attrTokenID]
		if a[attributeAction] != "accept_ask" || contract == "" || seller == "" || buyer == "" || denom == "" || a[attrCollection] == "" || a[attrTokenID] == "" || sold[asset] {
			continue
		}
		price, ok := sdkmath.NewIntFromString(a["price_amount"])
		if !ok || !price.IsPositive() {
			continue
		}
		parts := sdkmath.ZeroInt()
		for _, k := range []string{"fee_amount", "seller_proceeds", "royalty_amount"} {
			v, ok := sdkmath.NewIntFromString(firstNonEmpty(a[k], "0"))
			if !ok || v.IsNegative() {
				parts = sdkmath.ZeroInt()
				break
			}
			parts = parts.Add(v)
		}
		proceeds, _ := sdkmath.NewIntFromString(firstNonEmpty(a["seller_proceeds"], "0"))
		if !parts.Equal(price) || !coinsEqual(legsBetween(legs, buyer, contract), sdk.Coins{sdk.NewCoin(denom, price)}) ||
			!legsBetween(legs, contract, seller).AmountOf(denom).Equal(proceeds) || !nftMovedTo(log, a[attrCollection], a[attrTokenID], buyer) {
			continue
		}
		sold[asset] = true
		out = append(out, TaxableEvent{
			Category: string(CategoryNFTSale), FromAddr: seller, ToAddr: buyer,
			Amount: price.String(), Denom: denom, Asset: asset,
			Detail: contractDetail(contract, map[string]string{detailSale: "ask"}),
		})
	}
	return out
}

// matchListingFeeEvents books the listing fee of an ask that matched a bid
// as soon as it was set: the signer, who is the seller the sale names, pays
// the marketplace one coin before the sale, and the marketplace's first
// payout forwards exactly that coin to another address. The sale itself is
// paid from the bid's escrow, so no other coin leaves the signer.
func matchListingFeeEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	if sender == "" {
		return nil
	}
	var sale map[string]string
	for _, ev := range log.Events {
		switch ev.Type {
		case eventSetAsk:
			return nil
		case eventFinalizeSale:
			if sale != nil {
				return nil
			}
			sale = attrMap(ev)
		}
	}
	if sale == nil || sale["marketplace_action"] != "set-ask-match" || sale["seller_recipient"] != sender {
		return nil
	}
	contract := sale[attributeContractAddress]
	legs := transfersIn(log)
	// The sale's own nft_sale row names the seller and the sale coin; any
	// other known row naming them already accounts for the coin.
	var others []TaxableEvent
	for _, e := range known {
		if e.Category != string(CategoryNFTSale) {
			others = append(others, e)
		}
	}
	if contract == "" || len(legs) < 2 || legs[0].from != sender || legs[0].to != contract || len(legs[0].coins) != 1 ||
		!legs[0].coins.IsAllPositive() || namesAny(others, sender, legs[0].coins) {
		return nil
	}
	for _, l := range legs[1:] {
		if l.from == sender {
			return nil
		}
	}
	fwd := legs[1]
	if fwd.from != contract || fwd.to == sender || fwd.to == contract || !coinsEqual(legs[0].coins, fwd.coins) {
		return nil
	}
	return coinRows(CategoryExpense, legs[0].coins, sender, fwd.to, triggerListingFee, contractDetail(contract, map[string]string{"ask_id": sale["ask"]}))
}

// askListingFeeEvents books the fee a marketplace charges to list an NFT:
// the ask's creator pays the marketplace, which forwards exactly that coin
// to one other address in the same message, and no other coins move. It is
// an expense, as a name registration is. The creator is read from the ask
// event, so the rule needs no signer and runs inside a MsgExec too.
func askListingFeeEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	var ask map[string]string
	for _, ev := range log.Events {
		if ev.Type != eventSetAsk {
			continue
		}
		if ask != nil {
			return nil
		}
		ask = attrMap(ev)
	}
	if ask == nil {
		return nil
	}
	contract, creator := ask[attributeContractAddress], ask["creator"]
	if contract == "" || creator == "" || ask["id"] == "" || (sender != "" && sender != creator) {
		return nil
	}
	legs := transfersIn(log)
	if len(legs) != 2 || legs[0].from != creator || legs[0].to != contract || legs[1].from != contract ||
		legs[1].to == creator || legs[1].to == contract || len(legs[0].coins) != 1 || !coinsEqual(legs[0].coins, legs[1].coins) ||
		!legs[0].coins.IsAllPositive() || namesAny(known, creator, legs[0].coins) {
		return nil
	}
	return coinRows(CategoryExpense, legs[0].coins, creator, legs[1].to, triggerListingFee, contractDetail(contract, map[string]string{"ask_id": ask["id"]}))
}

// nftMovedTo reports the collection's transfer of the token to addr.
func nftMovedTo(log *indexerTxTypes.LogMessage, collection, tokenID, addr string) bool {
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		if a[attributeContractAddress] == collection && a[attrTokenID] == tokenID && a[attributeRecipient] == addr &&
			(a[attributeAction] == actionTransferNFT || a[attributeAction] == "send_nft") {
			return true
		}
	}
	return false
}

// depositDetail is a bid deposit's detail, naming the bid price when the
// coins paid differ from it.
func depositDetail(detail func(string, ...string) string, id, price string) string {
	if price == "" {
		return detail(id)
	}
	return detail(id, "price", price)
}

// Triggers shared with the reserve auction rows.
const (
	flowTriggerAuctionBid    = "auction_bid"
	flowTriggerAuctionRefund = "auction_refund"
)

// eventLegs returns the bank legs directly before and after item i, each
// side stopping at the neighbouring marketplace event.
func eventLegs(items []logItem, i int) (before, after []coinTransfer) {
	for j := i - 1; j >= 0 && items[j].leg != nil; j-- {
		before = append([]coinTransfer{*items[j].leg}, before...)
	}
	for j := i + 1; j < len(items) && items[j].leg != nil; j++ {
		after = append(after, *items[j].leg)
	}
	return before, after
}

// bidDeposit is what the bidder paid the marketplace just before the bid
// event: the escrow the bid holds. When it is not the bid price, the price
// is returned for the row's detail, and the row still holds the coins paid,
// so a later fill or refund releases the escrow instead of liquid coins.
func bidDeposit(attrs map[string]string, contract string, before []coinTransfer) (sdk.Coins, string, bool) {
	creator := attrs["creator"]
	if creator == "" || attrs["id"] == "" {
		return nil, "", false
	}
	paid := legsBetween(before, creator, contract)
	if len(paid) != 1 || !paid[0].IsPositive() {
		return nil, "", false
	}
	price, err := sdk.ParseCoinsNormalized(attrs["price"])
	if err != nil || !coinsEqual(paid, price) {
		return paid, attrs["price"], true
	}
	return paid, "", true
}

func legsBetween(legs []coinTransfer, from, to string) sdk.Coins {
	total := sdk.Coins{}
	for _, l := range legs {
		if l.from == from && l.to == to {
			total = total.Add(l.coins...)
		}
	}
	return total
}

// tldRegistrationEvents books an atom.names top-level domain registration
// as the name_registration expense the registry's other purchases use: what
// the signer paid the TLD contract in the message, unless a row already
// names that coin for the signer.
func tldRegistrationEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	for _, e := range known {
		if e.RewardTrigger == triggerNameRegistration {
			return nil
		}
	}
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		contract := a[attributeContractAddress]
		if a[attributeAction] != "register_tld" || contractKind(contract) != protocolNames || sender == "" {
			continue
		}
		coins := legsBetween(transfersIn(log), sender, contract)
		if coins.Empty() || namesAny(known, sender, coins) {
			return nil
		}
		out = append(out, coinRows(CategoryExpense, coins, sender, contract, triggerNameRegistration, contractDetail(contract, map[string]string{"name": a["tld"]}))...)
		break
	}
	return out
}

// forwardedDenomFeeEvents books the tokenfactory denom creation fee a wallet
// pays through a contract: the wallet sends the contract the fee and the
// contract, which creates the denom in the same message, forwards exactly
// that amount to the distribution module.
func forwardedDenomFeeEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	creators := map[string]string{}
	for _, ev := range log.Events {
		if ev.Type == "create_denom" {
			a := attrMap(ev)
			if a["creator"] != "" {
				creators[a["creator"]] = a["new_token_denom"]
			}
		}
	}
	if len(creators) == 0 || sender == "" {
		return nil
	}
	legs := transfersIn(log)
	var out []TaxableEvent
	for _, l := range legs {
		denom, ok := creators[l.from]
		if !ok || l.to != distributionModuleAddress {
			continue
		}
		fee := legsBetween(legs, sender, l.from)
		if fee.Empty() || !coinsEqual(fee, l.coins) || namesAny(known, sender, fee) {
			continue
		}
		out = append(out, coinRows(CategoryExpense, fee, sender, distributionModuleAddress, triggerDenomCreationFee, contractDetail(l.from, map[string]string{"denom": denom}))...)
		delete(creators, l.from)
	}
	return out
}

func namesAny(rows []TaxableEvent, addr string, coins sdk.Coins) bool {
	for _, c := range coins {
		for _, e := range rows {
			if e.Denom == c.Denom && (e.FromAddr == addr || e.ToAddr == addr) {
				return true
			}
		}
	}
	return false
}

// ReviewUnclassifiedContractMovement marks a bank fallback row for a coin
// other than the native one whose meaning no rule decides: the coin a wallet
// paid a contract or got from one, recorded as a transfer for the user to
// review, never dropped and never guessed as income.
const ReviewUnclassifiedContractMovement = "unclassified_contract_movement"

// fallbackFlow is one wallet's net movement of one denom in a contract
// message that no other row of the message names for that denom.
type fallbackFlow struct {
	wallet, denom string
	net           sdkmath.Int
	topIn, topOut sdkmath.Int
	from, to      string
	firstLegPos   int
}

type flowKey struct{ wallet, denom string }

// fallbackFlows nets each wallet's legs of the denoms keep accepts, in leg
// order. A leg names a wallet only for a denom no known row names it for.
func fallbackFlows(legs []coinTransfer, keep func(string) bool, wallet func(string) bool, named map[flowKey]bool) (map[flowKey]*fallbackFlow, []flowKey) {
	flows := map[flowKey]*fallbackFlow{}
	var order []flowKey
	for pos, l := range legs {
		if feeCollectors[l.from] || feeCollectors[l.to] || l.from == l.to {
			continue
		}
		for _, c := range l.coins {
			if !keep(c.Denom) || !c.Amount.IsPositive() {
				continue
			}
			for _, side := range []string{l.from, l.to} {
				k := flowKey{side, c.Denom}
				if !wallet(side) || named[k] {
					continue
				}
				f := flows[k]
				if f == nil {
					f = &fallbackFlow{wallet: side, denom: c.Denom, net: sdkmath.ZeroInt(), topIn: sdkmath.ZeroInt(), topOut: sdkmath.ZeroInt(), firstLegPos: pos}
					flows[k] = f
					order = append(order, k)
				}
				if side == l.to {
					f.net = f.net.Add(c.Amount)
					if c.Amount.GT(f.topIn) {
						f.topIn, f.from = c.Amount, l.from
					}
				} else {
					f.net = f.net.Sub(c.Amount)
					if c.Amount.GT(f.topOut) {
						f.topOut, f.to = c.Amount, l.to
					}
				}
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return flows[order[i]].firstLegPos < flows[order[j]].firstLegPos })
	return flows, order
}

// fallbackRow is the plain transfer of one flow: out to the address the
// wallet paid most, or in from the address that paid it most. A counterparty
// that is itself a wallet gets its own row, or is already named by one, so
// the row names the contract instead and no wallet sees the coin twice. It
// returns the row and the contract it is recorded against.
func fallbackRow(log *indexerTxTypes.LogMessage, f *fallbackFlow, top string, ownFlow func(string) bool) (TaxableEvent, string) {
	from, to, cp := f.from, f.wallet, f.from
	amt := f.net
	if f.net.IsNegative() {
		from, to, cp = f.wallet, f.to, f.to
		amt = f.net.Neg()
	}
	if ownFlow(cp) {
		if from == cp {
			from = top
		} else {
			to = top
		}
	}
	contract := top
	if excludedContract(log, cp) {
		contract = cp
	}
	return TaxableEvent{Category: string(CategoryTransfer), FromAddr: from, ToAddr: to, Amount: amt.String(), Denom: f.denom}, contract
}

// fallbackEvidence is what our own node's contract info says each
// registered contract is (code id and label). Only these contracts' registry
// kinds decide a fallback row's category; any other contract's rows are
// flagged for review, whatever kind the registry gives it.
var fallbackEvidence = map[string]struct {
	codeID uint64
	label  string
}{
	"cosmos1clswlqlfm8gpn7n5wu0ypu0ugaj36urlhj7yz30hn7v7mkcm2tuqy9f8s5": {127, "Skip Swap Entry Point"},
	"cosmos1lqu9662kd4my6dww4gzp3730vew0gkwe0nl9ztjh0n5da0a8zc4swsvd22": {126, "Skip Swap IBC Transfer Adapter"},
	"cosmos1r9695xq7y0fd2nk06rv6llykay7ny27jrpq4de2w6eqja5p2mlvqfv8aa9": {187, "DAO Rewards Distributor"},
	"cosmos19fprlcmzt2axrukx3xacsdpykv40nrdsv9l7jgnfrpwukgteu3zq3t2q9f": {187, "DAO Rewards Distributor"},
	"cosmos15nafa2hazwmjs8yz844h5ry5yrzvz6q96a0300ge26vm8hcsrd4s7zxhyj": {192, "dao-voting-token-staked"},
	"cosmos1690hpscut3ltfkcm997fw2jws3tk0wkh7tnlntlavqkpg56hpdzsrjhy5r": {752, "Inflow ATOM Vault"},
	"cosmos1d2hrvjvrvqvzdcffy2cy0p2gnytj48e2yg0lvnxk6knhmnt7cqvqetf09u": {752, "Inflow ATOM Vault"},
	"cosmos1nhu3aqrlf3adlffvx3z5pyxntgulgj9dhfskwlp8fksx5mf7v9qqfjpgpd": {691, "Governance Score Rewards"},
	"cosmos10c2h0v8p5szs43fu6fm5n4t3ddtcly72tw3663l9tgqaxzw96lxsj2pcfj": {192, "dao-voting-token-staked"},
	"cosmos10ugv48wsjp024fuj0ntd45qjzll5468a3z7q829pvn9ksse7w29qf0gtq9": {192, "dao-voting-token-staked"},
	"cosmos19v9yjq7swms9cs8haeklh8fvmayw6d8tu5hufegz63pmscu7n2vsz6zdej": {192, "dao-voting-token-staked"},
	"cosmos1d0flhtmllz6lvfsrjw544crg7ah02fj7xwxeergt05lmugh9rftqyapmya": {192, "dao-voting-token-staked"},
	"cosmos1kdt9aaju9zyp65dr7ztr58tmxh9cejw6mh8tywdujwwvzy66d0tq88ly5m": {192, "dao-voting-token-staked"},
	"cosmos1pzurh5vm7sl597u7gwpkfxjgw0d58ry78yewedv9sn4kcuyacwrqkc3dal": {36, "dao-voting-token-staked"},
	"cosmos172x6q0v849r2rsa8kqnrkvuza4lw8tjd77acc4gkzues4vkjlzjsve2pf8": {689, "Inflow Shares Converter"},
}

// fallbackCodeBuilds are the published release builds whose sha256 equals
// our own node's checksum for a code that fallbackEvidence pins. A pin on one
// of these codes is backed by the project's verified source, not by its
// on-chain label.
var fallbackCodeBuilds = map[uint64]struct {
	checksum string
	build    string
}{
	192: {"1441f2bc94d70c7cd781a32c85bfa1576108a3b27efef12f0d223b02c9d04a94", "DA0-DA0/dao-contracts v2.7.1 dao_voting_token_staked-default.wasm"},
	36:  {"c61a851c3561f4683812d43f7ccb7c8e85eb60de7fcb8b7d2f38727fa63adfdb", "DA0-DA0/dao-contracts v2.6.0 dao_voting_token_staked.wasm"},
	689: {"115f4df4bb8d0f66eaa0081bef89c8f916e4f08d17afb0afdb37690ca158088c", "informalsystems/hydro v3.6.12 shares_converter.wasm"},
}

// bankFallbackEvents records the coin a wallet gained or lost in a contract
// message that no other row of the message names, so every coin that moves
// between a wallet and a contract reaches the wallet's balances.
//
// The native coin is recorded exactly as it always has been: one plain
// transfer per wallet, out to the address it paid most or in from the
// address that paid it most. Every other coin follows all native rows, one
// row per wallet and denom against the contract, in the order of the
// wallet's first such leg. A wallet whose only unexplained coins are not the
// native one gets the category its contracts and movement decide:
//   - coins out and other coins back through a router or vault are a swap,
//     the way DEX trades and vault shares are recorded;
//   - tokens into a DAO staking module are a deposit the wallet still owns,
//     and tokens out of one are its withdrawal;
//   - coins only received from a rewards distributor are income.
//
// Only contracts in fallbackEvidence decide. Anything else, and the other
// coins of a wallet that also has a native fallback row (whose plain
// transfer cannot be one leg of a swap), is a transfer flagged
// ReviewUnclassifiedContractMovement: never dropped and never guessed as
// income.
//
// Contracts that ran in the message, module accounts and IBC escrow accounts
// are never the wallet side, and a 32-byte address is the wallet side only as
// the signer. Fee legs never reach a message's events, and a leg to a fee
// collector is skipped in case one does.
func bankFallbackEvents(log *indexerTxTypes.LogMessage, sender string, known []TaxableEvent) []TaxableEvent {
	excluded := fallbackExcluded(log)
	wallet := func(addr string) bool {
		if addr == "" || excluded[addr] {
			return false
		}
		if addr == sender {
			return true
		}
		_, bz, err := bech32.DecodeAndConvert(addr)
		return err == nil && len(bz) == 20
	}
	nativeNamed := map[flowKey]bool{}
	named := map[flowKey]bool{}
	for _, e := range known {
		for _, a := range []string{e.FromAddr, e.ToAddr} {
			named[flowKey{a, e.Denom}] = true
		}
		if e.Denom == fixtureNativeDenom {
			nativeNamed[flowKey{e.FromAddr, fixtureNativeDenom}] = true
			nativeNamed[flowKey{e.ToAddr, fixtureNativeDenom}] = true
		}
	}
	legs := withoutSettlementLegs(transfersIn(log), lsmRecordSettlements(log, sender))
	native, nativeOrder := fallbackFlows(legs, func(d string) bool { return d == fixtureNativeDenom }, wallet, nativeNamed)
	otherWallet := func(addr string) bool { return wallet(addr) && !transferEscrows()[addr] }
	other, otherOrder := fallbackFlows(legs, func(d string) bool { return d != fixtureNativeDenom }, otherWallet, named)
	top := firstContract(log)
	var out []TaxableEvent
	nativeRow := map[string]bool{}
	// nativeDAO marks a wallet whose native fallback row is a payout of a
	// pinned DAO proposal; its other coins from the same payout follow it.
	nativeDAO := map[string]bool{}
	for _, k := range nativeOrder {
		f := native[k]
		if f.net.IsZero() {
			continue
		}
		nativeRow[k.wallet] = true
		e, contract := fallbackRow(log, f, top, func(cp string) bool {
			_, ok := native[flowKey{cp, fixtureNativeDenom}]
			return ok || (nativeNamed[flowKey{cp, fixtureNativeDenom}] && wallet(cp))
		})
		e.Detail = contractDetail(contract, map[string]string{detailFallback: fallbackBank})
		if trigger, extra, ok := daoProposalClass(log, []*fallbackFlow{f}, []string{contract}); ok {
			e.Category, e.RewardTrigger = string(CategoryIncome), trigger
			e.Detail = contractDetail(contract, extra)
			nativeDAO[k.wallet] = true
		}
		out = append(out, e)
	}
	seen := map[string]bool{}
	// Decided fees follow every other fallback row, so the rows before them
	// keep the positions an earlier repair gave them.
	var fees []TaxableEvent
	for _, k0 := range otherOrder {
		w := k0.wallet
		if seen[w] || other[k0].net.IsZero() {
			continue
		}
		seen[w] = true
		var flows []*fallbackFlow
		var rows []TaxableEvent
		var contracts []string
		for _, k := range otherOrder {
			f := other[k]
			if k.wallet != w || f.net.IsZero() {
				continue
			}
			e, contract := fallbackRow(log, f, top, func(cp string) bool {
				_, ok := other[flowKey{cp, k.denom}]
				return ok || (named[flowKey{cp, k.denom}] && otherWallet(cp))
			})
			flows, rows, contracts = append(flows, f), append(rows, e), append(contracts, contract)
		}
		if !nativeRow[w] {
			if row, fee, ok := skipBridgeRows(log, sender, w, flows, rows, contracts); ok {
				out = append(out, row)
				if fee != nil {
					fees = append(fees, *fee)
				}
				continue
			}
		}
		if !nativeRow[w] || nativeDAO[w] {
			if trigger, extra, ok := daoProposalClass(log, flows, contracts); ok {
				for i := range rows {
					rows[i].Category, rows[i].RewardTrigger = string(CategoryIncome), trigger
					rows[i].Detail = contractDetail(contracts[i], extra)
				}
				out = append(out, rows...)
				continue
			}
		}
		category, review := CategoryTransfer, true
		if !nativeRow[w] {
			category, review = fallbackClass(flows, contracts)
		}
		for i := range rows {
			extra := map[string]string{detailFallback: fallbackBank}
			if review {
				extra[detailReviewRequired] = attributeTrue
				extra[detailReviewReason] = ReviewUnclassifiedContractMovement
			}
			rows[i].Category = string(category)
			rows[i].Detail = contractDetail(contracts[i], extra)
		}
		out = append(out, rows...)
	}
	return append(out, fees...)
}

// fallbackClass decides one wallet's fallback rows in a message from the
// registry kinds of the contracts they are recorded against, when our node's
// evidence backs every one of them, and from whether the wallet paid,
// received or both. It reports whether the rows need review.
func fallbackClass(flows []*fallbackFlow, contracts []string) (Category, bool) {
	var paid, got bool
	for _, f := range flows {
		if f.net.IsNegative() {
			paid = true
		} else {
			got = true
		}
	}
	kinds := map[string]bool{}
	for _, c := range contracts {
		if _, ok := fallbackEvidence[c]; !ok {
			return CategoryTransfer, true
		}
		kinds[contractKind(c)] = true
	}
	only := func(allowed ...string) bool {
		for k := range kinds {
			ok := false
			for _, a := range allowed {
				ok = ok || k == a
			}
			if !ok {
				return false
			}
		}
		return len(kinds) > 0
	}
	switch {
	case paid && got && only(contractRoleRouter, contractRoleVault):
		return CategorySwap, false
	case paid && !got && only(contractRoleDAOStaking):
		return CategoryProtocolDeposit, false
	case got && !paid && only(contractRoleDAOStaking):
		return CategoryProtocolWithdrawal, false
	case got && !paid && only(attributeRewards):
		return CategoryIncome, false
	}
	return CategoryTransfer, true
}

// feeCollectors are the module accounts transaction fees are paid to.
var feeCollectors = map[string]bool{moduleAddress("fee_collector"): true, moduleAddress("feemarket-fee-collector"): true}

// fallbackModules are the module accounts that can appear in a contract
// message's bank legs. None is ever a wallet.
var fallbackModules = []string{
	"fee_collector", moduleDistribution, "bonded_tokens_pool", "not_bonded_tokens_pool", "gov", minttypes.ModuleName,
	transfertypes.ModuleName, "tokenfactory", "interchainaccounts", "icahost", "provider", "consumer_rewards_pool",
	"feeibc", "liquid", "feemarket-fee-collector", "wasm", "ibc",
}

var fallbackModuleSet = func() map[string]bool {
	out := map[string]bool{}
	for _, m := range fallbackModules {
		out[moduleAddress(m)] = true
	}
	return out
}()

func moduleAddress(name string) string {
	s, err := bech32.ConvertAndEncode("cosmos", address.Module(name))
	if err != nil {
		panic(err)
	}
	return s
}

// fallbackExcluded is every address in the message that is not a wallet:
// the contracts that ran, the module accounts and the IBC escrow accounts of
// the channels the message sent packets on.
func fallbackExcluded(log *indexerTxTypes.LogMessage) map[string]bool {
	out := map[string]bool{}
	for m := range fallbackModuleSet {
		out[m] = true
	}
	for _, ev := range log.Events {
		for _, at := range ev.Attributes {
			if at.Key == attributeContractAddress && at.Value != "" {
				out[at.Value] = true
			}
		}
		a := attrMap(ev)
		if ev.Type == "send_packet" {
			if ch := a["packet_src_channel"]; ch != "" {
				port := a["packet_src_port"]
				if port == "" {
					port = transfertypes.PortID
				}
				out[escrowAddress(port, ch)] = true
			}
			if client := a["packet_source_client"]; client != "" {
				out[escrowAddress(transfertypes.PortID, client)] = true
			}
		}
	}
	return out
}

// transferEscrowChannels bounds the transfer channels whose escrow accounts
// are known up front. Hub channel numbers are far below it.
const transferEscrowChannels = 10000

var (
	transferEscrowOnce sync.Once
	transferEscrowSet  map[string]bool
)

// transferEscrows are the escrow accounts of transfer/channel-0 onward. An
// escrow is never the wallet side of a contract's other coins, even in a
// message without the send_packet that would name its channel.
func transferEscrows() map[string]bool {
	transferEscrowOnce.Do(func() {
		transferEscrowSet = make(map[string]bool, transferEscrowChannels)
		for n := 0; n < transferEscrowChannels; n++ {
			transferEscrowSet[escrowAddress(transfertypes.PortID, fmt.Sprintf("channel-%d", n))] = true
		}
	})
	return transferEscrowSet
}

func escrowAddress(port, channel string) string {
	s, err := bech32.ConvertAndEncode("cosmos", transfertypes.GetEscrowAddress(port, channel))
	if err != nil {
		return ""
	}
	return s
}

func excludedContract(log *indexerTxTypes.LogMessage, addr string) bool {
	for _, ev := range log.Events {
		for _, a := range ev.Attributes {
			if a.Key == attributeContractAddress && a.Value == addr {
				return true
			}
		}
	}
	return false
}

// firstContract is the first contract that ran in the message.
func firstContract(log *indexerTxTypes.LogMessage) string {
	for _, ev := range log.Events {
		for _, a := range ev.Attributes {
			if a.Key == attributeContractAddress && a.Value != "" {
				return a.Value
			}
		}
	}
	return ""
}

// coinsEqual compares two coin sets without the panic sdk.Coin.IsEqual has
// on a denom mismatch.
func coinsEqual(a, b sdk.Coins) bool {
	if len(a) != len(b) {
		return false
	}
	for _, c := range a {
		if !b.AmountOf(c.Denom).Equal(c.Amount) {
			return false
		}
	}
	return true
}
