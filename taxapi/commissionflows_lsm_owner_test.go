package taxapi

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Share-record ownership is separate from the delegation being tokenized.
func TestFlowRowsTokenizeOnlySpendsDelegatorsStake(t *testing.T) {
	const validator = "cosmosvaloper1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
	row := flowRow(2, 0, string(tax.CategoryTokenizeShares), flowRowsSeller, flowRowsHolder, "3000000", validator+"/1", `{"bonded_amount":"3000000uatom"}`)
	row.ValidatorAddress = validator
	for _, sameOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_record_owner", true: "same_record_owner"}[sameOwner], func(t *testing.T) {
			e := row
			if sameOwner {
				e.ToAddr = e.FromAddr
			}
			l := ledgerFromRows(t, map[string][]tax.TaxableEvent{flowRowsSeller: {e}, flowRowsHolder: {e}})
			l.openings[flowRowsSeller] = flowHoldings{Staked: map[string]decimal.Decimal{validator: base("3000000")}}
			l.openings[flowRowsHolder] = flowHoldings{Liquid: base("7000000")}
			l.commission(flowAt(1, 0), flowRowsSeller, 1000000)
			tr := runFlowTest(t, l, flowRowsSeller, flowRowsHolder)
			require.Equal(t, "1000000", tr.Wallets[flowRowsSeller].simulated().String())
			require.Equal(t, "7000000", tr.Wallets[flowRowsHolder].simulated().String())
			for _, w := range tr.order {
				for r := 0; r < flowRules; r++ {
					require.True(t, w.Overdraw[r].IsZero())
				}
			}
		})
	}
}

func TestFlowRowsShareTransfersNeverBecomeNativeATOM(t *testing.T) {
	e := flowRow(2, 0, apiTransfer, flowRowsSeller, flowRowsHolder, "3000000", "cosmosvaloper1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq/1", "")
	for _, addr := range []string{flowRowsSeller, flowRowsHolder} {
		_, ok := flowEventFromRow(e, addr, "uatom")
		require.False(t, ok)
	}
}
