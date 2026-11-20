package taxapi

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
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
