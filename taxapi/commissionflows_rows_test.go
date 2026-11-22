package taxapi

import (
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Synthetic addresses with the length and charset of real Hub accounts.
const (
	flowRowsBuyer   = "cosmos1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqbuyer"
	flowRowsSeller  = "cosmos1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzse11er"
	flowRowsHolder  = "cosmos1ppppppppppppppppppppppppppppppppho1der"
	flowRowsAuction = "cosmos1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaauct"
	flowRowsMarket  = "cosmos1mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmarket"
)

const flowRowsAuctionDetail = `{"contract":"` + flowRowsAuction + `","name":"Stargaze reserve auction","protocol":"Stargaze","sale":"auction"}`

func flowRow(day, minute int, category, from, to, amount, denom, detail string) tax.TaxableEvent {
	k := flowAt(day, minute)
	return tax.TaxableEvent{
		Category: category, FromAddr: from, ToAddr: to, Amount: amount, Denom: denom, Detail: detail,
		Timestamp: k.Time, BlockHeight: k.Height, MessageID: k.Message,
	}
}

// ledgerFromRows reads each wallet's ledger rows the way flowEvents does.
func ledgerFromRows(t *testing.T, rows map[string][]tax.TaxableEvent) *fakeFlowLedger {
	t.Helper()
	l := newFakeFlowLedger()
	for addr, list := range rows {
		for _, e := range list {
			if fe, ok := flowEventFromRow(e, addr, "uatom"); ok {
				l.rows[addr] = append(l.rows[addr], fe)
			}
		}
	}
	return l
}

func base(v string) decimal.Decimal { return decimal.RequireFromString(v) }

// A wallet that buys three NFTs through a marketplace pays the three prices;
// the rows name it as the buyer (To) with the price as Amount.
func TestFlowRowsNFTPurchaseSpendsThePrice(t *testing.T) {
	buy := func(day int, price string) tax.TaxableEvent {
		return flowRow(day, 0, apiNFTSale, flowRowsSeller, flowRowsBuyer, price, "uatom", "")
	}
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsBuyer: {
		buy(2, "74180000"), buy(3, "179000000"), buy(4, "78000000"),
	}})
	l.openings[flowRowsBuyer] = flowHoldings{Liquid: base("400000000")}
	l.commission(flowAt(1, 0), flowRowsBuyer, 100000000)
	tr := runFlowTest(t, l, flowRowsBuyer)

	// Chain: 400 + 100 - 331.18 = 168.82 ATOM left.
	require.Equal(t, "168820000", tr.Wallets[flowRowsBuyer].simulated().String())
	for _, e := range l.rows[flowRowsBuyer] {
		if e.Category == apiNFTSale {
			require.Equal(t, apiOut, e.Dir)
		}
	}
	// FIFO spends the 400 held first, so the commission is untouched.
	require.Equal(t, "100000000", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
}

// The seller side of the same row receives the price.
func TestFlowRowsNFTSaleReceivesThePrice(t *testing.T) {
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsSeller: {
		flowRow(2, 0, apiNFTSale, flowRowsSeller, flowRowsBuyer, "74180000", "uatom", ""),
		flowRow(3, 0, apiNFTSale, flowRowsSeller, flowRowsSeller, "5000000", "uatom", ""),
	}})
	l.commission(flowAt(1, 0), flowRowsSeller, 10000000)
	tr := runFlowTest(t, l, flowRowsSeller)
	require.Equal(t, "84180000", tr.Wallets[flowRowsSeller].simulated().String())
}

// A mint row names the owner as To and carries what the owner paid.
func TestFlowRowsNFTMintSpendsTheCost(t *testing.T) {
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsHolder: {
		flowRow(2, 0, apiNFTMint, "", flowRowsHolder, "5000000", "uatom", ""),
		flowRow(2, 1, apiNFTMint, "", flowRowsHolder, "", "", ""),
	}})
	l.commission(flowAt(1, 0), flowRowsHolder, 10000000)
	tr := runFlowTest(t, l, flowRowsHolder)
	require.Equal(t, "5000000", tr.Wallets[flowRowsHolder].simulated().String())
	require.Equal(t, "5000000", fate(tr, flowRuleFIFO, "2025-01", flowSpent, apiNFTMint, false))
}

// ATOM coming home over one hop arrives as its sender's trace and is released
// from escrow as native coin. A longer trace is a voucher, not native coin.
func TestFlowRowsReturningIBCIsNative(t *testing.T) {
	in := func(day int, amount, denom string) tax.TaxableEvent {
		return flowRow(day, 0, string(tax.CategoryIBCIn), "osmo1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsender", flowRowsHolder, amount, denom, "")
	}
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsHolder: {
		in(2, "99031000", "transfer/channel-0/uatom"),
		in(3, "100088000", "transfer/channel-141/uatom"),
		in(4, "500000000", "transfer/channel-0/transfer/channel-141/uatom"),
		in(5, "7000000", "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2"),
	}})
	l.openings[flowRowsHolder] = flowHoldings{Liquid: base("1000000")}
	tr := runFlowTest(t, l, flowRowsHolder)
	require.Equal(t, "200119000", tr.Wallets[flowRowsHolder].simulated().String())
}

func withTrigger(e tax.TaxableEvent, trigger string) tax.TaxableEvent {
	e.RewardTrigger = trigger
	return e
}

const flowRowsCollection = "stars1cccccccccccccccccccccccccccccccccccccccccccccccccccccccollect"

// auctionBid and auctionRefund are the protocol rows the parser writes for a
// reserve auction bid on a token and an outbid refund of it.
func auctionBid(day, minute int, wallet, amount, token string) tax.TaxableEvent {
	e := flowRow(day, minute, string(tax.CategoryProtocolDeposit), wallet, flowRowsAuction, amount, "uatom", auctionLotDetail(token))
	return withTrigger(e, flowAuctionBid)
}

func auctionRefund(day, minute int, wallet, amount, token string) tax.TaxableEvent {
	e := flowRow(day, minute, string(tax.CategoryProtocolWithdrawal), flowRowsAuction, wallet, amount, "uatom", auctionLotDetail(token))
	return withTrigger(e, flowAuctionRefund)
}

func auctionLotDetail(token string) string {
	return `{"collection":"` + flowRowsCollection + `","contract":"` + flowRowsAuction + `","name":"Stargaze reserve auction","protocol":"Stargaze","token_id":"` + token + `"}`
}

// auctionSale is the nft_sale row for a token sold through the auction
// contract, by settlement or buy-now.
func auctionSale(day, minute int, buyer, amount, token string) tax.TaxableEvent {
	e := flowRow(day, minute, apiNFTSale, flowRowsSeller, buyer, amount, "uatom", flowRowsAuctionDetail)
	e.Asset = flowRowsCollection + "/" + token
	return e
}

// A reserve auction winner's bid is escrowed with the auction contract as a
// protocol_deposit when it is placed, so settlement pays from that bid; an
// outbid refund leaves the escrow; a buy-now pays from the wallet, never from
// coins locked elsewhere (a governance deposit here).
func TestFlowRowsAuctionSettlesFromEscrowedBid(t *testing.T) {
	gov := "cosmos1" + strings.Repeat("g", 35) + "govmod"
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsBuyer: {
		flowRow(2, 0, string(tax.CategoryGovDeposit), flowRowsBuyer, gov, "15000000", "uatom", ""),
		auctionBid(2, 1, flowRowsBuyer, "10000000", "1"),
		auctionRefund(3, 0, flowRowsBuyer, "10000000", "1"),
		auctionBid(3, 1, flowRowsBuyer, "30000000", "2"),
		auctionSale(4, 0, flowRowsBuyer, "30000000", "2"),
		auctionSale(5, 0, flowRowsBuyer, "20000000", "3"),
	}})
	l.commission(flowAt(1, 0), flowRowsBuyer, 100000000)
	tr := runFlowTest(t, l, flowRowsBuyer)

	// Chain: 100 - 15 gov deposit - 30 winning bid - 20 buy-now = 35 ATOM
	// in the bank, 15 still in the governance deposit, no open bid.
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "35000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.Equal(t, "15000000", w.pools[r].locked.total().String(), flowRuleNames[r])
		require.Equal(t, "50000000", fate(tr, r, "2025-01", flowSpent, apiNFTSale, false))
		require.True(t, w.Overdraw[r].IsZero())
	}
}

// A buy-now on one token through the same contract that holds an open bid
// on another token pays from the wallet and leaves that bid open.
func TestFlowRowsAuctionBuyNowLeavesOtherBidOpen(t *testing.T) {
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsBuyer: {
		auctionBid(2, 0, flowRowsBuyer, "10000000", "1"),
		auctionSale(3, 0, flowRowsBuyer, "10000000", "2"),
	}})
	l.commission(flowAt(1, 0), flowRowsBuyer, 50000000)
	tr := runFlowTest(t, l, flowRowsBuyer)

	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "30000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Len(t, w.pools[r].bids, 1, flowRuleNames[r])
		require.Equal(t, "10000000", fate(tr, r, "2025-01", flowLocked, "", false))
	}
}

// A settlement pays with the coins its own bid escrowed, not with commission
// that sits in an unrelated governance deposit.
func TestFlowRowsAuctionSettlementKeepsBidOrigin(t *testing.T) {
	gov := "cosmos1" + strings.Repeat("g", 35) + "govmod"
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsBuyer: {
		flowRow(2, 0, string(tax.CategoryGovDeposit), flowRowsBuyer, gov, "10000000", "uatom", ""),
		flowRow(3, 0, string(tax.CategoryIBCIn), "osmo1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsender", flowRowsBuyer, "10000000", "uatom", ""),
		auctionBid(4, 0, flowRowsBuyer, "10000000", "1"),
		auctionSale(5, 0, flowRowsBuyer, "10000000", "1"),
	}})
	l.commission(flowAt(1, 0), flowRowsBuyer, 10000000)
	tr := runFlowTest(t, l, flowRowsBuyer)

	for r := 0; r < flowRules; r++ {
		require.Equal(t, "10000000", fate(tr, r, "2025-01", flowLocked, "", false), flowRuleNames[r])
		require.Equal(t, "0", base(fate(tr, r, "2025-01", flowSpent, apiNFTSale, false)).String(), flowRuleNames[r])
	}
}

// Coins deposited before tracing started come back as other coins: the
// wallet's balance rises, and none of it counts as commission.
func TestFlowRowsPreWindowDepositReturnsAsOtherCoins(t *testing.T) {
	l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsHolder: {
		flowRow(2, 0, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsHolder, "20000000", "uatom", ""),
		flowRow(3, 0, apiTransfer, flowRowsHolder, flowTestExchange, "25000000", "uatom", ""),
	}})
	l.commission(flowAt(1, 0), flowRowsHolder, 10000000)
	tr := runFlowTest(t, l, flowRowsHolder)

	require.Equal(t, "5000000", tr.Wallets[flowRowsHolder].simulated().String())
	// FIFO: the 10 commission came first and leaves first.
	require.Equal(t, "10000000", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	// Pro-rata: commission is a third of the 30 held, so a third of the 25.
	ex := base(fate(tr, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	held := base(fate(tr, flowRuleProRata, "2025-01", flowHeld, "", false))
	require.True(t, ex.Add(held).Equal(base("10000000")))
	require.Equal(t, "10000000", tr.Commission["2025-01"].String())
}
