package taxapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestPrivateOpeningSourceBelongsToTheTraceRead(t *testing.T) {
	ledger := newFakeFlowLedger()
	ledger.openings[flowTestReceiver] = flowHoldings{Liquid: decimal.NewFromInt(10), Source: CommissionFlowHoldingSource{Kind: "ledger_exact", Height: 123}}
	first := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 0, 1), ledger)
	require.NoError(t, first.discover(flowTestReceiver, 0, flowTestStart, nil))
	ledger.openings[flowTestReceiver] = flowHoldings{Liquid: decimal.NewFromInt(11), Source: CommissionFlowHoldingSource{Kind: "archive_exact", Height: 123, ReceiptHashes: []string{"second-read"}}}
	second := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 0, 1), ledger)
	require.NoError(t, second.discover(flowTestReceiver, 0, flowTestStart, nil))
	require.Equal(t, "ledger_exact", first.Wallets[flowTestReceiver].OpeningSource.Kind)
	require.Equal(t, "10", first.Wallets[flowTestReceiver].simulated().String())
	require.Equal(t, "archive_exact", second.Wallets[flowTestReceiver].OpeningSource.Kind)
	require.Equal(t, "11", second.Wallets[flowTestReceiver].simulated().String())
}

func TestPrivateWalletEvidenceCopiesAllAccountingFields(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	closing := since.AddDate(0, 0, 10)
	w := &flowWallet{Addr: flowTestReceiver, Hop: 2, Since: since, Known: true}
	for r := 0; r < flowRules; r++ {
		w.pools[r] = newFlowPools(r)
		w.Overdraw[r] = decimal.NewFromInt(int64(r + 1))
	}
	w.pools[0].liquid.add([]flowPart{{Amount: decimal.NewFromInt(12)}})
	actual := flowHoldings{Liquid: decimal.NewFromInt(10), Staked: map[string]decimal.Decimal{"v": decimal.NewFromInt(3)}, Unbonding: map[string]decimal.Decimal{"v": decimal.NewFromInt(4)}}
	e := commissionFlowWalletEvidence(w, actual, closing)
	require.Equal(t, w.Addr, e.Address)
	require.Equal(t, 2, e.Hop)
	require.Equal(t, since, e.Since)
	require.True(t, e.OpeningKnown)
	require.Equal(t, closing, e.ClosingDay)
	require.Equal(t, "10", e.ActualLiquidBase)
	require.Equal(t, map[string]string{"v": "3"}, e.ActualStakedBase)
	require.Equal(t, map[string]string{"v": "4"}, e.ActualUnbondingBase)
	require.Equal(t, "12", e.SimulatedBase)
	require.Equal(t, "-5", e.DriftBase)
	require.Equal(t, map[string]string{"fifo": "1", "pro_rata": "2"}, e.Overdraw)
	actual.Staked["v"] = decimal.NewFromInt(99)
	w.Overdraw[0] = decimal.Zero
	require.Equal(t, "3", e.ActualStakedBase["v"])
	require.Equal(t, "1", e.Overdraw["fifo"])
	before := CommissionFlowEvidence{Wallets: []CommissionFlowWalletEvidence{e}}
	raw, err := json.Marshal(before)
	require.NoError(t, err)
	var after CommissionFlowEvidence
	require.NoError(t, json.Unmarshal(raw, &after))
	require.Equal(t, before, after)
}
