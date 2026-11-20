package taxapi

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const limitQuote = "ibc/QUOTE"

func limitDeposit(day int, id, side, price, open, held, denom string) tax.TaxableEvent {
	d := `{"contract":"` + flowRowsMarket + `","open_qty":"` + open + `","order_id":"` + id + `","price":"` + price + `","side":"` + side + `"}`
	return withTrigger(flowRow(day, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, held, denom, d), tax.TriggerLimitOrder)
}

func limitFill(day, minute int, amount, denom string) tax.TaxableEvent {
	d := `{"contract":"` + flowRowsMarket + `","maker":"true","review_reason":"` + tax.ReviewLimitMakerSpend + `","review_required":"true"}`
	return withTrigger(flowRow(day, minute, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, amount, denom, d), tax.TriggerLimitFill)
}

func limitCancelRow(day int, id, refundQty, amount, denom string) tax.TaxableEvent {
	d := `{"contract":"` + flowRowsMarket + `","order_id":"` + id + `","refund_qty":"` + refundQty + `"}`
	return withTrigger(flowRow(day, 3, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, amount, denom, d), tax.TriggerLimitCancel)
}

// A resting buy order filled later by another wallet's sell: the base the
// maker received is a swap paid from the order's quote at its own price, and
// the fee reserve the order paid goes into that swap's cost, since it paid
// for this trade. The fill that closes the order spends all it still holds.
func TestLimitMakerFillPaysFromOrder(t *testing.T) {
	dep := limitDeposit(3, "48", "buy", "2082676512967", "500000", "1043941", limitQuote)
	fill := limitFill(9, 2, "500000", "uatom")
	got := limitMakerLegs([]tax.TaxableEvent{fill}, flowRowsBuyer, []tax.TaxableEvent{dep, fill})
	require.Len(t, got, 2)
	require.Equal(t, "", semanticReviewReason(Row{Detail: got[0].Detail}))
	require.Equal(t, "48", detailValue(got[0].Detail, "order_id"))
	leg := got[1]
	require.Equal(t, string(tax.CategorySwap), leg.Category)
	require.Equal(t, tax.TriggerLimitFill, leg.RewardTrigger)
	require.Equal(t, flowRowsBuyer, leg.FromAddr)
	require.Equal(t, flowRowsMarket, leg.ToAddr)
	require.Equal(t, "1043941", leg.Amount)
	require.Equal(t, limitQuote, leg.Denom)
	require.Equal(t, limitBasisMakerFill, detailValue(leg.Detail, "lot_basis"))
	require.Equal(t, fill.MessageID, leg.MessageID)
	require.Negative(t, leg.SubIndex)

	// Two fills split the reserve by the quantity each filled: 200000 at the
	// limit price (416535) plus 2/5 of the 2603 reserve (1041), then the
	// closing fill spends the rest.
	first, second := limitFill(9, 2, "200000", "uatom"), limitFill(10, 4, "300000", "uatom")
	got = limitMakerLegs([]tax.TaxableEvent{first, second}, flowRowsBuyer, []tax.TaxableEvent{dep, first, second})
	require.Len(t, got, 4)
	require.Equal(t, "417576", got[1].Amount)
	require.Equal(t, "626365", got[3].Amount)
	for _, e := range got {
		require.NotEqual(t, string(tax.CategoryExpense), e.Category)
	}
}

// A sell order partly filled by a later buy and then cancelled: the fill
// releases the base its quote bought at the order's price, and the cancel
// refunds exactly the base left open, so it adds nothing.
func TestLimitCancelAfterPartialMakerFill(t *testing.T) {
	dep := limitDeposit(3, "51", "sell", "2000000000000", "1000000", "1000000", "uatom")
	fill := limitFill(5, 2, "1000000", limitQuote)
	cancel := limitCancelRow(7, "51", "500000", "500000", "uatom")
	got := limitMakerLegs([]tax.TaxableEvent{fill, cancel}, flowRowsBuyer, []tax.TaxableEvent{dep, fill, cancel})
	require.Len(t, got, 3)
	require.Equal(t, "500000", got[1].Amount)
	require.Equal(t, "uatom", got[1].Denom)
	require.Equal(t, cancel, got[2])

	// A cancel refunding more base than the replay left open is flagged.
	bad := limitCancelRow(7, "51", "600000", "600000", "uatom")
	got = limitMakerLegs([]tax.TaxableEvent{bad}, flowRowsBuyer, []tax.TaxableEvent{dep, fill, bad})
	require.Len(t, got, 1)
	require.Equal(t, reviewLimitLot, semanticReviewReason(Row{Detail: got[0].Detail}))

	// A buy cancelled after a partial fill: the fill spends its share of
	// the fee reserve (2000 of 5000), and the cancel gives up the reserve
	// of the cancelled quantity, which no trade paid for, as a fee.
	buy := limitDeposit(3, "79", "buy", "2000000000000", "1000000", "2005000", limitQuote)
	bfill := limitFill(5, 2, "400000", "uatom")
	bcancel := limitCancelRow(7, "79", "600000", "1200000", limitQuote)
	got = limitMakerLegs([]tax.TaxableEvent{bfill, bcancel}, flowRowsBuyer, []tax.TaxableEvent{buy, bfill, bcancel})
	require.Len(t, got, 4)
	require.Equal(t, "802000", got[1].Amount)
	require.Equal(t, string(tax.CategoryExpense), got[3].Category)
	require.Equal(t, tax.TriggerLimitFee, got[3].RewardTrigger)
	require.Equal(t, "3000", got[3].Amount)
}

// One payout can fill several of the wallet's orders: the best price goes
// first, then the oldest, as the book matches them.
func TestLimitMakerFillWalksPriceThenTime(t *testing.T) {
	low := limitDeposit(2, "10", "sell", "2000000000000", "300000", "300000", "uatom")
	best := limitDeposit(3, "11", "sell", "1900000000000", "200000", "200000", "uatom")
	same := limitDeposit(4, "12", "sell", "2000000000000", "300000", "300000", "uatom")
	// 200000 at 1.9 (380000) and 100000 at 2.0 (200000) from the older order.
	fill := limitFill(9, 2, "580000", limitQuote)
	got := limitMakerLegs([]tax.TaxableEvent{fill}, flowRowsBuyer, []tax.TaxableEvent{low, best, same, fill})
	require.Len(t, got, 3)
	require.Equal(t, "11,10", detailValue(got[0].Detail, "order_id"))
	require.Equal(t, "200000", got[1].Amount)
	require.Equal(t, "11", detailValue(got[1].Detail, "order_id"))
	require.Equal(t, "100000", got[2].Amount)
	require.Equal(t, "10", detailValue(got[2].Detail, "order_id"))

	// A payout the open orders do not cover keeps its review flag.
	big := limitFill(9, 2, "5000000", limitQuote)
	got = limitMakerLegs([]tax.TaxableEvent{big}, flowRowsBuyer, []tax.TaxableEvent{low, big})
	require.Len(t, got, 1)
	require.Equal(t, tax.ReviewLimitMakerSpend, semanticReviewReason(Row{Detail: got[0].Detail}))
	got = limitMakerLegs([]tax.TaxableEvent{fill}, flowRowsBuyer, []tax.TaxableEvent{fill})
	require.Len(t, got, 1)
	require.Equal(t, tax.ReviewLimitMakerSpend, semanticReviewReason(Row{Detail: got[0].Detail}))
}

// In the commission flow a sell order's resting ATOM waits in its lot; the
// maker fill swaps that part out of the lot and the cancel returns the rest.
func TestFlowRowsLimitMakerFillSpendsLot(t *testing.T) {
	dep := limitDeposit(3, "51", "sell", "2000000000000", "30000000", "30000000", "uatom")
	fill := limitFill(5, 2, "20000000", limitQuote)
	cancel := limitCancelRow(7, "51", "20000000", "20000000", "uatom")
	rows := limitMakerLegs([]tax.TaxableEvent{dep, fill, cancel}, flowRowsBuyer, []tax.TaxableEvent{dep, fill, cancel})
	require.Len(t, rows, 4)
	l := bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 30000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "20000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "10000000", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
}

// The journal pays the maker fill and the reserve a cancel gave up from the
// protocol account the order moved the quote into, so that account closes
// at zero.
func TestJournalLimitMakerFillPaysFromProtocol(t *testing.T) {
	rows := []Row{
		protoRow(TagProtocolDeposit, apiOut, "USDC", 2005000, 1, "D1"),
		protoRow(apiSwap, "in", apiSymbolATOM, 400000, 2, "F1"),
		protoRow(apiSwap, apiOut, "USDC", 802000, 1, "F1"),
		protoRow(string(tax.CategoryProtocolWithdrawal), "in", "USDC", 1200000, 1, "C1"),
		protoRow(TagExpense, apiOut, "USDC", 3000, 1, "C1"),
	}
	rows[2].Detail = `{"lot_basis":"maker_fill"}`
	rows[4].Detail = `{"lot_basis":"fee_reserve"}`
	rows[4].RewardTrigger = tax.TriggerLimitFee
	for i := range rows {
		rows[i].Time = rows[0].Time.Add(time.Duration(i) * time.Hour)
	}
	bal := map[string]decimal.Decimal{}
	for _, l := range BuildJournal(rows, undisclosedAlways) {
		d, _ := decimal.NewFromString(firstNonEmptyStr(l.Debit, "0"))
		c, _ := decimal.NewFromString(firstNonEmptyStr(l.Credit, "0"))
		bal[l.Account] = bal[l.Account].Add(d).Sub(c)
	}
	require.True(t, bal["Digital Assets in Protocol: USDC"].IsZero(), "%v", bal)
	require.Equal(t, "800000", bal["Digital Assets: ATOM"].String(), "%v", bal)
}

// A stream exit's leg and a limit fill's legs in one message never share a
// sub index.
func TestReportLegSubIndexRangesApart(t *testing.T) {
	for sub := 0; sub < 64; sub++ {
		stream := streamLegBase - sub
		for o := 0; o < 64; o++ {
			for i := 0; i < 4; i++ {
				require.NotEqual(t, stream, limitLegBase-(o*limitLegSpan+i))
			}
		}
	}
}
