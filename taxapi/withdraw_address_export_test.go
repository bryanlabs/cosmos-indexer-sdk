package taxapi

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// A reward the chain paid to the delegator's withdraw address is booked as
// the delegator's income, and its coins move on without a disposal: the
// delegator side drops exactly that reward lot, and the withdraw address
// holds it at the reward's basis and receipt time, whoever owns it. Tags never
// reach the move, so a tag on the reward transaction cannot make it income or
// a sale.
const waClaimTrigger = "claim"

func TestWithdrawAddressMoveExports(t *testing.T) {
	const (
		delegator = "cosmos1delegator"
		withdraw  = "cosmos1withdraw"
		hash      = "REWARDTX"
	)
	paidAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	soldAt := paidAt.AddDate(0, 2, 0)
	detail := `{"protocol":"distribution","reward_trigger":"` + waClaimTrigger + `"}`
	reward := Row{Time: paidAt, TxHash: hash, MessageID: 7, Category: apiReward, Direction: "in", Symbol: apiSymbolATOM, Denom: apiUatom, Amount: d(50), PriceUSD: d(10), ValueUSD: d(500), To: delegator, ValidatorAddress: flowTestValidator, RewardTrigger: waClaimTrigger}
	moveOut := Row{Time: paidAt, TxHash: hash, MessageID: 7, Category: apiTransfer, Direction: apiOut, Symbol: apiSymbolATOM, Denom: apiUatom, Amount: d(50), PriceUSD: d(10), ValueUSD: d(500), From: delegator, To: withdraw, RewardTrigger: tax.TriggerWithdrawAddress, Detail: detail}
	moveIn := moveOut
	moveIn.Direction = "in"
	sale := Row{Time: soldAt, TxHash: "SALETX", Category: apiTransfer, Direction: apiOut, Symbol: apiSymbolATOM, Denom: apiUatom, Amount: d(50), PriceUSD: d(12), ValueUSD: d(600), From: withdraw, To: flowTestExchange}

	// Form 8949, delegator: the move is no disposal.
	require.Empty(t, Build8949([]Row{reward, moveOut}))
	// Form 8949, withdraw address: the coins carry the reward's basis and
	// receipt date into the sale.
	got := Build8949([]Row{moveIn, sale})
	require.Len(t, got, 1)
	require.False(t, got[0].BasisUnknown)
	require.Equal(t, "03/01/2025", got[0].DateAcquired)
	require.Equal(t, "500", got[0].CostBasis.String())
	require.Equal(t, "100", got[0].GainLoss.String())
	// A tag on the reward transaction tags the reward, never the move.
	for _, tag := range []string{TagInternalTransfer, TagGift, TagPayment, TagIncome, TagSpam} {
		tagged := ApplyTags([]Row{reward, moveOut, moveIn}, UserTags{hash: tag})
		require.Equal(t, tag, tagged[0].UserTag)
		require.Empty(t, tagged[1].UserTag, tag)
		require.Empty(t, tagged[2].UserTag, tag)
		require.False(t, isIncomeRow(tagged[2]), tag)
	}
	// Even a tag set on the move itself changes nothing.
	tagged := func(r Row) Row { r.UserTag = TagGift; return r }
	require.Empty(t, Build8949([]Row{reward, tagged(moveOut)}))
	require.Equal(t, got, Build8949([]Row{tagged(moveIn), sale}))

	csvRows := func(format string, rows ...Row) [][]string {
		var b bytes.Buffer
		require.NoError(t, WriteCSV(&b, format, rows))
		records, err := csv.NewReader(&b).ReadAll()
		require.NoError(t, err)
		return records[1:]
	}
	// CryptoTaxCalculator: staking income, then a transfer out; the withdraw
	// address receives a transfer in.
	ctc := csvRows(apiCryptotaxcalculator, reward, moveOut)
	require.Len(t, ctc, 2)
	require.Equal(t, apiStaking, ctc[0][1])
	require.Equal(t, "transfer-out", ctc[1][1])
	require.Equal(t, delegator, ctc[1][8])
	require.Equal(t, withdraw, ctc[1][9])
	require.Contains(t, ctc[1][12], "source="+tax.TriggerWithdrawAddress)
	require.Equal(t, "transfer-in", csvRows(apiCryptotaxcalculator, moveIn)[0][1])
	require.Equal(t, "transfer-in", csvRows(apiCryptotaxcalculator, tagged(moveIn))[0][1])
	// Koinly: its own transfer (no label) on both sides.
	koinly := csvRows(apiKoinly, reward, moveOut)
	require.Equal(t, apiStaking, koinly[0][9])
	require.Equal(t, "", koinly[1][9])
	require.Equal(t, "50", koinly[1][1])
	require.Contains(t, koinly[1][10], "source="+tax.TriggerWithdrawAddress)
	require.Equal(t, "", csvRows(apiKoinly, moveIn)[0][9])
	require.Equal(t, "", csvRows(apiKoinly, tagged(moveIn))[0][9])

	// Journal: income, then the coins leave and arrive through internal
	// clearing at the reward's value.
	noJurisdiction := func(string) string { return "" }
	accounts := func(rows ...Row) []string {
		var out []string
		for _, l := range BuildJournal(rows, noJurisdiction) {
			side := "Cr"
			if l.Debit != "" {
				side = "Dr"
			}
			out = append(out, side+" "+l.Account+" "+l.Debit+l.Credit)
		}
		return out
	}
	assets := acctDigitalAssets + ": " + apiSymbolATOM
	wantDelegator := []string{
		"Dr " + assets + " 500.00", "Cr " + acctStakingIncome + " 500.00",
		"Dr " + acctInternal + " 500.00", "Cr " + assets + " 500.00",
	}
	require.Equal(t, wantDelegator, accounts(reward, moveOut))
	// Same instant, move listed first: the journal still books the reward first.
	require.Equal(t, wantDelegator, accounts(moveOut, reward))
	require.Equal(t, []string{"Dr " + assets + " 500.00", "Cr " + acctInternal + " 500.00"}, accounts(moveIn))
	require.Equal(t, []string{"Dr " + assets + " 500.00", "Cr " + acctInternal + " 500.00"}, accounts(tagged(moveIn)))
	memo := BuildJournal([]Row{moveOut}, noJurisdiction)[0].Memo
	require.True(t, strings.HasPrefix(memo, "transfer "+tax.TriggerWithdrawAddress+" (distribution)"), memo)
}

// With several lots of the asset, the move takes the reward lot of its own
// message and denom: not the oldest lot, and not another reward booked at the
// same instant. Sales later use the lots that are left.
func TestWithdrawAddressMoveTakesItsRewardLot(t *testing.T) {
	const delegator, withdraw = "cosmos1delegator", "cosmos1withdraw"
	bought := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	paidAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	soldAt := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	atom := func(r Row) Row {
		r.Symbol, r.Denom = apiSymbolATOM, apiUatom
		r.ValueUSD = r.Amount.Mul(r.PriceUSD)
		return r
	}
	older := atom(Row{Time: bought, TxHash: "OLDREWARD", MessageID: 3, Category: apiReward, Direction: "in", Amount: d(100), PriceUSD: d(5), To: delegator, RewardTrigger: waClaimTrigger})
	// Two rewards in one block, from two messages, the second paid on.
	stays := atom(Row{Time: paidAt, TxHash: "BLOCKTX1", MessageID: 8, Category: apiReward, Direction: "in", Amount: d(20), PriceUSD: d(10), To: delegator, RewardTrigger: waClaimTrigger})
	moved := atom(Row{Time: paidAt, TxHash: "BLOCKTX2", MessageID: 9, Category: apiReward, Direction: "in", Amount: d(30), PriceUSD: d(10), To: delegator, RewardTrigger: waClaimTrigger})
	moveOut := atom(Row{Time: paidAt, TxHash: "BLOCKTX2", MessageID: 9, Category: apiTransfer, Direction: apiOut, Amount: d(30), PriceUSD: d(10), From: delegator, To: withdraw, RewardTrigger: tax.TriggerWithdrawAddress})
	moveIn := moveOut
	moveIn.Direction = "in"
	// The same denom booked in the other message is a different lot.
	foreign := moveOut
	foreign.MessageID = 99

	sell := func(from string, amount, price int64) Row {
		return atom(Row{Time: soldAt, TxHash: "SALE", Category: apiTransfer, Direction: apiOut, Amount: d(amount), PriceUSD: d(price), From: from, To: flowTestExchange})
	}
	asOf := soldAt.AddDate(0, 0, -1)
	noPrice := func(string) (decimal.Decimal, bool) { return decimal.Zero, false }

	// Delegator: the move line sits before its reward in the input, the
	// order the lot engines must not depend on at one instant.
	delegatorRows := []Row{older, moveOut, stays, moved}
	require.Empty(t, Build8949(delegatorRows))
	pos := BuildPositions(delegatorRows, asOf, noPrice)
	require.Len(t, pos, 1)
	require.Equal(t, "120", pos[0].Quantity.String())
	require.Equal(t, "700", pos[0].CostBasisUSD.String())
	require.Equal(t, 2, pos[0].Lots)
	require.True(t, pos[0].BasisUnknownQty.IsZero())
	// Selling everything left: the older lot long term at $5, the reward that
	// stayed short term at $10. Nothing of the moved reward.
	got := Build8949(append(delegatorRows, sell(delegator, 120, 12)))
	require.Len(t, got, 2)
	require.Equal(t, "01/10/2024", got[0].DateAcquired)
	require.Equal(t, "500", got[0].CostBasis.String())
	require.True(t, got[0].LongTerm)
	require.Equal(t, "03/01/2025", got[1].DateAcquired)
	require.Equal(t, "200", got[1].CostBasis.String())
	require.Equal(t, "40", got[1].GainLoss.String())
	require.True(t, got[1].LongTerm)
	// A move naming another message takes nothing here.
	pos = BuildPositions([]Row{older, stays, moved, foreign}, asOf, noPrice)
	require.Equal(t, "150", pos[0].Quantity.String())

	// Withdraw address: known basis at the reward's value, held from the
	// receipt, so a sale 13 months later is long term.
	pos = BuildPositions([]Row{moveIn}, asOf, noPrice)
	require.Len(t, pos, 1)
	require.Equal(t, "30", pos[0].Quantity.String())
	require.Equal(t, "300", pos[0].CostBasisUSD.String())
	require.True(t, pos[0].BasisUnknownQty.IsZero())
	require.Equal(t, "2025-03-01", pos[0].OldestLot)
	got = Build8949([]Row{moveIn, sell(withdraw, 30, 12)})
	require.Len(t, got, 1)
	require.False(t, got[0].BasisUnknown)
	require.Equal(t, "03/01/2025", got[0].DateAcquired)
	require.Equal(t, "300", got[0].CostBasis.String())
	require.Equal(t, "60", got[0].GainLoss.String())
	require.True(t, got[0].LongTerm)
	// The withdraw address's own earlier lot is sold first, FIFO.
	own := atom(Row{Time: bought, TxHash: "OWN", Category: apiReward, MessageID: 2, Direction: "in", Amount: d(10), PriceUSD: d(4), To: withdraw, RewardTrigger: waClaimTrigger})
	got = Build8949([]Row{own, moveIn, sell(withdraw, 25, 12)})
	require.Len(t, got, 2)
	require.Equal(t, "40", got[0].CostBasis.String())
	require.Equal(t, "150", got[1].CostBasis.String())
	require.Equal(t, "03/01/2025", got[1].DateAcquired)
}
