package taxapi

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

// A reward the chain paid to the delegator's withdraw address reaches the
// exports as two ordinary transfers: the delegator books the income and
// sends the coins on (a disposal at the receipt value, so no gain), and the
// withdraw address receives coins whose basis this report does not know.
// Tagging both sides internal_transfer is the user's choice, as for any
// transfer between their own wallets.
func TestWithdrawAddressMoveExports(t *testing.T) {
	const (
		delegator = "cosmos1delegator"
		withdraw  = "cosmos1withdraw"
		hash      = "REWARDTX"
	)
	paidAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	soldAt := paidAt.AddDate(0, 2, 0)
	detail := `{"protocol":"distribution","reward_trigger":"claim"}`
	reward := Row{Time: paidAt, TxHash: hash, Category: apiReward, Direction: "in", Symbol: apiSymbolATOM, Amount: d(50), PriceUSD: d(10), ValueUSD: d(500), To: delegator, ValidatorAddress: flowTestValidator, RewardTrigger: "claim"}
	moveOut := Row{Time: paidAt, TxHash: hash, Category: apiTransfer, Direction: apiOut, Symbol: apiSymbolATOM, Amount: d(50), PriceUSD: d(10), ValueUSD: d(500), From: delegator, To: withdraw, RewardTrigger: tax.TriggerWithdrawAddress, Detail: detail}
	moveIn := moveOut
	moveIn.Direction = "in"
	sale := Row{Time: soldAt, TxHash: "SALETX", Category: apiTransfer, Direction: apiOut, Symbol: apiSymbolATOM, Amount: d(50), PriceUSD: d(12), ValueUSD: d(600), From: withdraw, To: flowTestExchange}

	// Form 8949, delegator: the move disposes of the reward lot at its own
	// receipt value.
	got := Build8949([]Row{reward, moveOut})
	require.Len(t, got, 1)
	require.False(t, got[0].BasisUnknown)
	require.Equal(t, "500", got[0].Proceeds.String())
	require.Equal(t, "500", got[0].CostBasis.String())
	require.True(t, got[0].GainLoss.IsZero())
	// Form 8949, withdraw address: the received lot has unknown basis.
	got = Build8949([]Row{moveIn, sale})
	require.Len(t, got, 1)
	require.True(t, got[0].BasisUnknown)
	require.Equal(t, "03/01/2025", got[0].DateAcquired)
	require.Equal(t, "600", got[0].GainLoss.String())
	// Tagged internal on both sides, the move is no disposal and the
	// withdraw address has no lot from it: the report is wallet by wallet.
	tagged := func(r Row) Row { r.UserTag = TagInternalTransfer; return r }
	require.Empty(t, Build8949([]Row{reward, tagged(moveOut)}))
	got = Build8949([]Row{tagged(moveIn), sale})
	require.Len(t, got, 1)
	require.True(t, got[0].BasisUnknown)
	require.Equal(t, apiVariousDates, got[0].DateAcquired)

	csvRows := func(format string, rows ...Row) [][]string {
		var b bytes.Buffer
		require.NoError(t, WriteCSV(&b, format, rows))
		records, err := csv.NewReader(&b).ReadAll()
		require.NoError(t, err)
		return records[1:]
	}
	// CryptoTaxCalculator: a send and a receive, transfer-out and
	// transfer-in once tagged internal.
	ctc := csvRows(apiCryptotaxcalculator, reward, moveOut)
	require.Len(t, ctc, 2)
	require.Equal(t, apiStaking, ctc[0][1])
	require.Equal(t, apiSend, ctc[1][1])
	require.Equal(t, delegator, ctc[1][8])
	require.Equal(t, withdraw, ctc[1][9])
	require.Contains(t, ctc[1][12], "source="+tax.TriggerWithdrawAddress)
	require.Equal(t, "receive", csvRows(apiCryptotaxcalculator, moveIn)[0][1])
	require.Equal(t, "transfer-out", csvRows(apiCryptotaxcalculator, tagged(moveOut))[0][1])
	require.Equal(t, "transfer-in", csvRows(apiCryptotaxcalculator, tagged(moveIn))[0][1])
	// Koinly: an untagged send and receive, Koinly's own transfer (no label)
	// once tagged internal.
	koinly := csvRows(apiKoinly, reward, moveOut)
	require.Equal(t, apiStaking, koinly[0][9])
	require.Equal(t, apiSend, koinly[1][9])
	require.Equal(t, "50", koinly[1][1])
	require.Contains(t, koinly[1][10], "source="+tax.TriggerWithdrawAddress)
	require.Equal(t, "receive", csvRows(apiKoinly, moveIn)[0][9])
	require.Equal(t, "", csvRows(apiKoinly, tagged(moveOut))[0][9])
	require.Equal(t, "", csvRows(apiKoinly, tagged(moveIn))[0][9])

	// Journal: income, then the coins leave through transfers-out clearing;
	// the withdraw address receives through transfers-in clearing, and both go
	// through internal clearing when tagged.
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
	require.Equal(t, []string{
		"Dr " + assets + " 500.00", "Cr " + acctStakingIncome + " 500.00",
		"Dr " + acctTransfersOut + " 500.00", "Cr " + assets + " 500.00",
	}, accounts(reward, moveOut))
	require.Equal(t, []string{"Dr " + assets + " 500.00", "Cr " + acctTransfersIn + " 500.00"}, accounts(moveIn))
	require.Equal(t, []string{"Dr " + acctInternal + " 500.00", "Cr " + assets + " 500.00"}, accounts(tagged(moveOut)))
	require.Equal(t, []string{"Dr " + assets + " 500.00", "Cr " + acctInternal + " 500.00"}, accounts(tagged(moveIn)))
	memo := BuildJournal([]Row{moveOut}, noJurisdiction)[0].Memo
	require.True(t, strings.HasPrefix(memo, "transfer "+tax.TriggerWithdrawAddress+" (distribution)"), memo)
}
