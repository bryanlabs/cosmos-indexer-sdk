package tax

import (
	"sort"
	"strings"

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
	triggerBidFilled = "bid_filled"
	detailFallback   = "fallback"
	fallbackBank     = "bank"
)

// Stargaze marketplace bid events. A bid escrows the bidder's coins in the
// marketplace until it is removed, replaced or filled.
const (
	eventSetBid              = "wasm-set-bid"
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
	if flattenedLog(log) {
		return bankFallbackEvents(log, "", known)
	}
	out = append(out, marketplaceBidEvents(log, sender, known)...)
	out = append(out, tldRegistrationEvents(log, sender, known)...)
	out = append(out, forwardedDenomFeeEvents(log, sender, known)...)
	out = append(out, streamswapEvents(log, sender, known)...)
	out = append(out, liquidityPoolEvents(log, sender, known)...)
	out = append(out, limitOrderEvents(log, sender, known)...)
	all := make([]TaxableEvent, 0, len(known)+len(out))
	all = append(all, known...)
	all = append(all, out...)
	out = append(out, bankFallbackEvents(log, sender, all)...)
	return out
}

// WasmSupplementForLog is the supplement rows of one contract message's log
// as wasmSupplementEvents writes them after that message's known rows. The
// repair uses it for contract messages inside a MsgExec, whose granter the
// stored log does not name, so it passes no sender.
func WasmSupplementForLog(log *indexerTxTypes.LogMessage, sender string) []TaxableEvent {
	if log == nil {
		return nil
	}
	return wasmSupplementEvents(log, sender, wasmExecuteEvents(log, sender))
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
			sale = "collection_bid"
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
			if dep, ok := bidDeposit(it.attrs, contract, before); ok {
				out = append(out, coinRows(CategoryProtocolDeposit, dep, it.attrs["creator"], contract, flowTriggerAuctionBid, detail(it.attrs["id"]))...)
			}
		case eventUpdateBid, eventUpdateCollectionBid:
			dep, ok := bidDeposit(it.attrs, contract, before)
			if !ok {
				continue
			}
			refund := legsBetween(after, contract, it.attrs["creator"])
			out = append(out, coinRows(CategoryProtocolWithdrawal, refund, contract, it.attrs["creator"], flowTriggerAuctionRefund, detail(it.attrs["id"]))...)
			out = append(out, coinRows(CategoryProtocolDeposit, dep, it.attrs["creator"], contract, flowTriggerAuctionBid, detail(it.attrs["id"]))...)
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
			id, s := it.attrs["bid"], "bid"
			switch it.attrs["marketplace_action"] {
			case "accept-bid":
			case "accept-collection-bid":
				id, s = it.attrs["collection_bid"], "collection_bid"
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

// bidDeposit is the bid price when the bidder's transfer to the marketplace
// just before the bid event is exactly that price.
func bidDeposit(attrs map[string]string, contract string, before []coinTransfer) (sdk.Coins, bool) {
	creator := attrs["creator"]
	if creator == "" || attrs["id"] == "" {
		return nil, false
	}
	price, err := sdk.ParseCoinsNormalized(attrs["price"])
	if err != nil || len(price) != 1 || !price[0].IsPositive() {
		return nil, false
	}
	paid := legsBetween(before, creator, contract)
	if !coinsEqual(paid, price) {
		return nil, false
	}
	return price, true
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

// bankFallbackEvents records the native coin a wallet gained or lost in a
// contract message that no other row of the message names, as a plain
// transfer: out to the address it paid most, or in from the address that
// paid it most. Contracts that ran in the message, module accounts and IBC
// escrow accounts are never the wallet side, and a 32-byte address is the
// wallet side only as the signer. Fee legs never reach a message's events,
// and a leg to a fee collector is skipped in case one does.
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
	named := map[string]bool{}
	for _, e := range known {
		if e.Denom == fixtureNativeDenom {
			named[e.FromAddr] = true
			named[e.ToAddr] = true
		}
	}
	type flow struct {
		net         sdkmath.Int
		topIn       sdkmath.Int
		topOut      sdkmath.Int
		from, to    string
		firstLegPos int
	}
	flows := map[string]*flow{}
	var order []string
	legs := transfersIn(log)
	for pos, l := range legs {
		amt := l.coins.AmountOf(fixtureNativeDenom)
		if !amt.IsPositive() || feeCollectors[l.from] || feeCollectors[l.to] || l.from == l.to {
			continue
		}
		for _, side := range []string{l.from, l.to} {
			if !wallet(side) || named[side] {
				continue
			}
			f := flows[side]
			if f == nil {
				f = &flow{net: sdkmath.ZeroInt(), topIn: sdkmath.ZeroInt(), topOut: sdkmath.ZeroInt(), firstLegPos: pos}
				flows[side] = f
				order = append(order, side)
			}
			if side == l.to {
				f.net = f.net.Add(amt)
				if amt.GT(f.topIn) {
					f.topIn, f.from = amt, l.from
				}
			} else {
				f.net = f.net.Sub(amt)
				if amt.GT(f.topOut) {
					f.topOut, f.to = amt, l.to
				}
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return flows[order[i]].firstLegPos < flows[order[j]].firstLegPos })
	top := firstContract(log)
	var out []TaxableEvent
	for _, addr := range order {
		f := flows[addr]
		if f.net.IsZero() {
			continue
		}
		from, to, cp := f.from, addr, f.from
		amt := f.net
		if f.net.IsNegative() {
			from, to, cp = addr, f.to, f.to
			amt = f.net.Neg()
		}
		// A counterparty that is itself a wallet gets its own row, or is
		// already named by one, so this row names the contract instead and
		// no wallet sees the coin twice.
		if _, ok := flows[cp]; ok || (named[cp] && wallet(cp)) {
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
		out = append(out, TaxableEvent{
			Category: string(CategoryTransfer), FromAddr: from, ToAddr: to,
			Amount: amt.String(), Denom: fixtureNativeDenom,
			Detail: contractDetail(contract, map[string]string{detailFallback: fallbackBank}),
		})
	}
	return out
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
