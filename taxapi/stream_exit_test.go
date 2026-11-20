package taxapi

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func withDetail(e tax.TaxableEvent, d string) tax.TaxableEvent { e.Detail = d; return e }

// A stream exit gets the leg its subscription paid: the spent amount the
// exit names (v2), or what was left after withdrawals and the exit's own
// refund (v1); the review flag clears once that leg exists.
func TestStreamExitLegsFromLot(t *testing.T) {
	sub := streamRow(3, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, "30000000", "uatom", tax.TriggerStreamSubscribe)
	wd := streamRow(4, 1, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, "10000000", "uatom", tax.TriggerStreamWithdraw)
	refund := streamRow(9, 1, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, "4000000", "uatom", tax.TriggerStreamWithdraw)
	base := `{"contract":"` + flowRowsMarket + `","review_reason":"stream_spend_not_in_message","review_required":"true","stream_id":"2"}`
	v1 := withDetail(streamRow(9, 1, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, "123", "ibc/STREAMED", tax.TriggerStreamExit), base)
	v2 := withDetail(v1, strings.Replace(base, `"stream_id"`, `"spent":"16000000","stream_id"`, 1))

	// v1: 30 subscribed - 10 withdrawn - 4 refunded at exit = 16 spent.
	got := streamExitLegs([]tax.TaxableEvent{refund, v1}, flowRowsBuyer, []tax.TaxableEvent{sub, wd, refund})
	require.Len(t, got, 3)
	require.Equal(t, "", semanticReviewReason(Row{Detail: got[1].Detail}))
	leg := got[2]
	require.Equal(t, string(tax.CategorySwap), leg.Category)
	require.Equal(t, flowRowsBuyer, leg.FromAddr)
	require.Equal(t, flowRowsMarket, leg.ToAddr)
	require.Equal(t, "16000000", leg.Amount)
	require.Equal(t, "uatom", leg.Denom)
	require.Contains(t, leg.Detail, `"lot_basis":"remainder"`)

	// v2 takes the spent amount the exit names.
	got = streamExitLegs([]tax.TaxableEvent{v2}, flowRowsBuyer, []tax.TaxableEvent{sub, wd, refund})
	require.Len(t, got, 2)
	require.Equal(t, "16000000", got[1].Amount)
	require.Contains(t, got[1].Detail, `"lot_basis":"spent"`)

	// A spend past the lot, or no subscription at all, keeps the review.
	over := withDetail(v1, strings.Replace(base, `"stream_id"`, `"spent":"25000000","stream_id"`, 1))
	for name, c := range map[string]struct {
		exit tax.TaxableEvent
		lots []tax.TaxableEvent
	}{"over": {over, []tax.TaxableEvent{sub, wd}}, "no lot": {v1, nil}, "other stream": {v1, []tax.TaxableEvent{withDetail(sub, `{"contract":"`+flowRowsMarket+`","stream_id":"3"}`)}}} {
		got = streamExitLegs([]tax.TaxableEvent{c.exit}, flowRowsBuyer, c.lots)
		require.Len(t, got, 1, name)
		require.Equal(t, tax.ReviewStreamSpend, semanticReviewReason(Row{Detail: got[0].Detail}), name)
	}
}

// The paying leg never reaches the commission flow engine, which already
// swaps the lot at the exit.
func TestFlowRowsStreamExitPayingLegNotSpentTwice(t *testing.T) {
	sub := streamRow(3, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, "30000000", "uatom", tax.TriggerStreamSubscribe)
	exit := streamRow(9, 1, string(tax.CategorySwap), flowRowsMarket, flowRowsBuyer, "123", "ibc/STREAMED", tax.TriggerStreamExit)
	rows := streamExitLegs([]tax.TaxableEvent{sub, exit}, flowRowsBuyer, []tax.TaxableEvent{sub})
	require.Len(t, rows, 3)
	l := bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 30000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "0", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
	require.Equal(t, "30000000", fate(tr, flowRuleFIFO, "2025-01", flowSwapped, apiSwap, false))
}

// The journal takes the exit's paying leg from the protocol account the
// subscription moved the coins into, so both accounts close at zero.
func TestJournalStreamExitPaysFromProtocol(t *testing.T) {
	rows := []Row{
		protoRow(TagProtocolDeposit, apiOut, apiSymbolATOM, 30, 5, "S1"),
		protoRow("protocol_withdrawal", "in", apiSymbolATOM, 14, 5, "W1"),
		protoRow(apiSwap, "in", "STRM", 100, 0, "X1"),
		protoRow(apiSwap, apiOut, apiSymbolATOM, 16, 5, "X1"),
	}
	rows[2].RewardTrigger, rows[3].RewardTrigger = tax.TriggerStreamExit, tax.TriggerStreamExit
	rows[3].Detail = `{"lot_basis":"remainder"}`
	for i := range rows {
		rows[i].Time = rows[0].Time.Add(time.Duration(i) * time.Hour)
	}
	bal := map[string]decimal.Decimal{}
	for _, l := range BuildJournal(rows, undisclosedAlways) {
		d, _ := decimal.NewFromString(firstNonEmptyStr(l.Debit, "0"))
		c, _ := decimal.NewFromString(firstNonEmptyStr(l.Credit, "0"))
		bal[l.Account] = bal[l.Account].Add(d).Sub(c)
	}
	require.True(t, bal["Digital Assets in Protocol: ATOM"].IsZero(), "%v", bal)
	require.Equal(t, "-80", bal["Digital Assets: ATOM"].String(), "%v", bal)
}

func firstNonEmptyStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// A cancelled stream's refund closes the subscription lot and gives the
// coins back as the wallet's own.
func TestFlowRowsStreamRefundClosesLot(t *testing.T) {
	rows := []tax.TaxableEvent{
		streamRow(3, 1, string(tax.CategoryProtocolDeposit), flowRowsBuyer, flowRowsMarket, "30000000", "uatom", tax.TriggerStreamSubscribe),
		streamRow(5, 1, string(tax.CategoryProtocolWithdrawal), flowRowsMarket, flowRowsBuyer, "30000000", "uatom", tax.TriggerStreamRefund),
	}
	l := bidLedger(t, rows)
	l.commission(flowAt(1, 0), flowRowsBuyer, 30000000)
	tr := runFlowTest(t, l, flowRowsBuyer)
	w := tr.Wallets[flowRowsBuyer]
	require.Equal(t, "30000000", w.simulated().String())
	for r := 0; r < flowRules; r++ {
		require.Empty(t, w.pools[r].bids, flowRuleNames[r])
		require.True(t, w.Overdraw[r].IsZero(), flowRuleNames[r])
	}
}
