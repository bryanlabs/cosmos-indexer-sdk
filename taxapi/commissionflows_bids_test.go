package taxapi

import (
	"os"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func marketBidDetail(id string) string {
	return `{"bid_id":"` + id + `","collection":"` + flowRowsCollection + `","contract":"` + flowRowsMarket + `","name":"Stargaze marketplace","protocol":"Stargaze","sale":"collection_bid"}`
}

func marketBid(day, minute int, amount, id string) tax.TaxableEvent {
	return withTrigger(flowRow(day, minute, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, amount, "uatom", marketBidDetail(id)), flowAuctionBid)
}

func marketRefund(day, minute int, amount, id string) tax.TaxableEvent {
	return withTrigger(flowRow(day, minute, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, amount, "uatom", marketBidDetail(id)), flowAuctionRefund)
}

// marketFill is an accepted bid: the buyer's nft_sale and the bid_filled
// release the parser writes after it in the same message.
func marketFill(day, minute int, amount, id string) []tax.TaxableEvent {
	sale := flowRow(day, minute, apiNFTSale, flowRowsSeller, flowRowsBuyer, amount, "uatom", "")
	sale.Asset = flowRowsCollection + "/9"
	fill := withTrigger(flowRow(day, minute, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, amount, "uatom", marketBidDetail(id)), flowBidFilled)
	fill.SubIndex = 1
	return []tax.TaxableEvent{sale, fill}
}

// bidLedger reads rows the way flowEvents does, including the bid fill
// pairing.
func bidLedger(t *testing.T, rows []tax.TaxableEvent) *fakeFlowLedger {
	t.Helper()
	l := newFakeFlowLedger()
	var events []flowEvent
	for _, e := range rows {
		if fe, ok := flowEventFromRow(e, flowRowsBuyer, "uatom"); ok {
			events = append(events, fe)
		}
	}
	l.rows[flowRowsBuyer] = payFilledBids(events, flowRowsBuyer)
	return l
}

// A marketplace bid escrows the commission it was placed with; a raised bid
// returns the old escrow and escrows the new price; the accepted bid pays
// for the NFT from that escrow, so the buyer is charged once and the
// commission is what bought the NFT.
func TestFlowRowsMarketplaceBidFillChargesOnce(t *testing.T) {
	rows := []tax.TaxableEvent{
		flowRow(2, 1, string(tax.CategoryIBCIn), "osmo1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsender", flowRowsBuyer, "60000000", "uatom", ""),
		marketBid(3, 2, "30000000", "b1"),
		marketRefund(4, 3, "30000000", "b1"),
		marketBid(4, 4, "40000000", "b1"),
	}
	rows = append(rows, marketFill(5, 5, "40000000", "b1")...)
	l := bidLedger(t, rows)
	for _, e := range l.rows[flowRowsBuyer] {
		require.NotEqual(t, flowBidFilled, e.Trigger, "the release is folded into the sale")
	}
	l.commission(flowAt(1, 0), flowRowsBuyer, 40000000)
	tr := runFlowTest(t, l, flowRowsBuyer)

	// Chain: 40 commission + 60 other - 40 paid for the NFT = 60 ATOM.
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "60000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	// The raised bid escrowed 10 of the commission (the 30 refunded went
	// to the back of the wallet's queue), and that bid paid for the NFT.
	require.Equal(t, "10000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))

	// Without the raise, the whole commission bid and bought the NFT,
	// while the 60 of other origin stays in the wallet.
	rows = []tax.TaxableEvent{
		flowRow(2, 1, string(tax.CategoryIBCIn), "osmo1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsender", flowRowsBuyer, "60000000", "uatom", ""),
		marketBid(3, 2, "40000000", "b1"),
	}
	rows = append(rows, marketFill(5, 5, "40000000", "b1")...)
	l = bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 40000000)
	tr = runFlowTest(t, l, flowRowsBuyer)
	require.Equal(t, "60000000", tr.Wallets[flowRowsBuyer].simulated().String())
	require.Equal(t, "40000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
}

// A removed bid returns its escrow to the wallet.
func TestFlowRowsMarketplaceBidRemoved(t *testing.T) {
	l := bidLedger(t, []tax.TaxableEvent{marketBid(2, 1, "25000000", "b2"), marketRefund(3, 2, "25000000", "b2")})
	l.commission(flowAt(1, 0), flowRowsBuyer, 25000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "25000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
	}
	require.Equal(t, "25000000", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
}

// A release with no purchase in its message returns the escrow as a plain
// withdrawal.
func TestFlowRowsMarketplaceFillWithoutSale(t *testing.T) {
	fill := marketFill(3, 2, "25000000", "b3")[1]
	l := bidLedger(t, []tax.TaxableEvent{marketBid(2, 1, "25000000", "b3"), fill})
	l.commission(flowAt(1, 0), flowRowsBuyer, 25000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "25000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
	}
}

func streamRow(day, minute int, category, from, to, amount, denom, trigger string) tax.TaxableEvent {
	detail := `{"contract":"` + flowRowsMarket + `","stream_id":"2"}`
	return withTrigger(flowRow(day, minute, category, from, to, amount, denom, detail), trigger)
}

// A streamswap subscription waits in the stream as the subscriber's coins:
// a withdrawal brings the unspent part back, and the exit that pays out the
// streamed token, in another denom, swaps whatever was left in the stream.
func TestFlowRowsStreamswapSubscriptionSwapsAtExit(t *testing.T) {
	rows := []tax.TaxableEvent{
		flowRow(2, 1, string(tax.CategoryIBCIn), "osmo1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsender", flowRowsBuyer, "5000000", "uatom", ""),
		streamRow(3, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, "30000000", "uatom", tax.TriggerStreamSubscribe),
		streamRow(4, 1, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, "10000000", "uatom", tax.TriggerStreamWithdraw),
		streamRow(9, 1, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, "123456789", "ibc/STREAMED", tax.TriggerStreamExit),
	}
	l := bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 30000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	// Chain: 30 commission + 5 other - 30 subscribed + 10 withdrawn = 15.
	require.Equal(t, "15000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "20000000", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowSpent, "", false))

	// Without the exit the subscription is still the wallet's, waiting in
	// the stream, and nothing was spent.
	l = bidLedger(t, rows[:3])
	l.commission(flowAt(1, 0), flowRowsBuyer, 30000000)
	tr = runFlowTest(t, l, flowRowsBuyer)
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
	require.NotEmpty(t, tr.Wallets[flowRowsBuyer].pools[flowRuleFIFO].bids)
}

// An exit of a stream the wallet never subscribed to moves nothing.
func TestFlowRowsStreamswapExitWithoutSubscription(t *testing.T) {
	l := bidLedger(t, []tax.TaxableEvent{streamRow(9, 1, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, "123", "ibc/STREAMED", tax.TriggerStreamExit)})
	l.commission(flowAt(1, 0), flowRowsBuyer, 1000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	require.Equal(t, "1000000", tr.Wallets[flowRowsBuyer].simulated().String())
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
}

// goldenRows reads a reviewed parser golden (tax/testdata/<name>.golden):
// the rows the parser writes for one exported production message.
func goldenRows(t *testing.T, name string, day int) []tax.TaxableEvent {
	t.Helper()
	data, err := os.ReadFile("../tax/testdata/" + name + ".golden")
	require.NoError(t, err)
	var out []tax.TaxableEvent
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.SplitN(line, "|", 9)
		require.Len(t, f, 9, line)
		e := flowRow(day, 0, f[0], f[4], f[5], f[1], f[2], f[8])
		e.Asset, e.ValidatorAddress, e.RewardTrigger, e.SubIndex = f[3], f[6], f[7], i
		out = append(out, e)
	}
	return out
}

// Three production messages of one collection bid: the bid, a raise that
// refunds it and escrows the new price, and the accepted bid that releases
// that escrow, joined by their bid_id. The buyer pays for the NFT once,
// from the escrow, and the marketplace holds nothing of theirs afterwards.
func TestFlowRowsMarketplaceProductionBidChain(t *testing.T) {
	var rows []tax.TaxableEvent
	for i, n := range []string{"wasm-bidchain-1-set", "wasm-bidchain-2-update", "wasm-bidchain-3-accept"} {
		rows = append(rows, goldenRows(t, n, i+2)...)
	}
	buyer := rows[0].FromAddr
	lots := map[string]bool{}
	for _, e := range rows {
		if e.RewardTrigger == flowAuctionBid || e.RewardTrigger == flowAuctionRefund || e.RewardTrigger == flowBidFilled {
			lots[flowBidLot(e)] = true
		}
	}
	require.Len(t, lots, 1, "every escrow row names the same bid")

	l := newFakeFlowLedger()
	var events []flowEvent
	for _, e := range rows {
		if e.FromAddr != buyer && e.ToAddr != buyer {
			continue
		}
		if fe, ok := flowEventFromRow(e, buyer, "uatom"); ok {
			events = append(events, fe)
		}
	}
	l.rows[buyer] = payFilledBids(events, buyer)
	l.commission(flowAt(1, 0), buyer, 10000000)
	tr := runFlowTest(t, l, buyer)
	w := tr.Wallets[buyer]
	// 10 commission - 5.326923 paid for the NFT.
	require.Equal(t, "4673077", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "5326923", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))
}

// An accepted bid whose marketplace payouts do not add up to the price
// still releases the escrow at the price; the payouts are a diagnostic in
// the row's detail and move nothing.
func TestFlowRowsMarketplaceFillWithPayoutMismatch(t *testing.T) {
	fill := marketFill(5, 5, "40000000", "b4")
	fill[1].Detail = strings.TrimSuffix(fill[1].Detail, "}") + `,"payouts":"39000000uatom"}`
	l := bidLedger(t, append([]tax.TaxableEvent{marketBid(3, 2, "40000000", "b4")}, fill...))
	l.commission(flowAt(1, 0), flowRowsBuyer, 40000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "0", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "40000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))
}

// A limit order swaps the part matched at once, keeps the rest in the book
// as the wallet's coins, and a cancel brings that rest back.
func TestFlowRowsLimitOrderDepositAndCancel(t *testing.T) {
	detail := `{"contract":"` + flowRowsMarket + `","order_id":"7"}`
	rows := []tax.TaxableEvent{
		withTrigger(flowRow(3, 1, string(tax.CategorySwap), flowRowsBuyer, flowRowsMarket, "5000000", "uatom", detail), tax.TriggerLimitFill),
		withTrigger(flowRow(3, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, "15000000", "uatom", detail), tax.TriggerLimitOrder),
		withTrigger(flowRow(3, 1, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, "99", "ibc/QUOTE", detail), tax.TriggerLimitFill),
		withTrigger(flowRow(6, 1, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, "15000000", "uatom", detail), tax.TriggerLimitCancel),
	}
	for i := range rows {
		rows[i].SubIndex = i
	}
	l := bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 20000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "15000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "5000000", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
	require.Equal(t, "15000000", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
}

// A listed NFT bought at its asking price keeps the escrow release as a
// record-only nft_transfer next to the nft_sale. The buyer pays the price
// once, the seller is credited what the chain paid, and the transfer moves
// no coin and is no disposal.
func TestFlowRowsAcceptAskTransferAndSale(t *testing.T) {
	rows := goldenRows(t, "wasm-accept-ask-other-market", 2)
	require.Equal(t, apiNFTTransfer, rows[0].Category)
	require.Equal(t, apiNFTSale, rows[1].Category)
	require.Equal(t, rows[0].Asset, rows[1].Asset)
	require.False(t, Row{Category: rows[0].Category}.isTaxable())
	seller, buyer := rows[1].FromAddr, rows[1].ToAddr

	l := newFakeFlowLedger()
	var events []flowEvent
	for _, e := range rows {
		if e.FromAddr != buyer && e.ToAddr != buyer {
			continue
		}
		if fe, ok := flowEventFromRow(e, buyer, "uatom"); ok {
			events = append(events, fe)
		}
	}
	require.Len(t, events, 1, "only the sale moves the buyer's coin")
	l.rows[buyer] = events
	l.commission(flowAt(1, 0), buyer, 30000000)
	tr := runFlowTest(t, l, buyer)
	require.Equal(t, "10000000", tr.Wallets[buyer].simulated().String())
	require.Equal(t, "20000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))

	events = nil
	for _, e := range rows {
		if e.FromAddr != seller && e.ToAddr != seller {
			continue
		}
		if fe, ok := flowEventFromRow(e, seller, "uatom"); ok {
			events = append(events, fe)
		}
	}
	require.Len(t, events, 1)
	events = netSaleProceeds(events, seller, map[uint]decimal.Decimal{events[0].Key.Message: decimal.NewFromInt(19600000)})
	require.Equal(t, "19600000", events[0].Amount.String())
}

// A bid escrowed at a leg that differs from its event price is filled from
// that escrow, never from the bidder's liquid coins a second time.
func TestFlowRowsBidPriceMismatchFillsFromEscrow(t *testing.T) {
	bid := marketBid(3, 2, "15000000", "b5")
	bid.Detail = strings.TrimSuffix(bid.Detail, "}") + `,"price":"14000000uatom"}`
	l := bidLedger(t, append([]tax.TaxableEvent{bid}, marketFill(5, 5, "15000000", "b5")...))
	l.commission(flowAt(1, 0), flowRowsBuyer, 15000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "0", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "15000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTSale, false))
}
