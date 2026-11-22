package taxapi

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func flowSlashWallet(t *testing.T, l *fakeFlowLedger) *flowTrace {
	t.Helper()
	tr := runFlowTest(t, l, flowTestReceiver)
	require.Len(t, tr.order, 1)
	return tr
}

func flowDelegate(k flowKey, validator string, v int64) flowEvent {
	return flowEvent{Key: k, Category: flowCatDelegate, Dir: apiOut, Amount: amt(v), From: flowTestReceiver, Validator: validator}
}

func TestFlowSlashBurnsDelegatedStake(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.openings[r] = flowHoldings{Liquid: decimal.Zero, Staked: map[string]decimal.Decimal{flowTestValidator: amt(1000000)}}
	l.commission(flowAt(1, 0), r, 1000000)
	l.rows[r] = append(l.rows[r], flowDelegate(flowAt(1, 5), flowTestValidator, 1000000))
	l.slash(flowAt(2, 0), flowTestValidator, "0.0001")
	tr := flowSlashWallet(t, l)

	w := tr.order[0]
	require.Equal(t, "1999800", w.simulated().String(), "the slash takes 0.0001 of the 2,000,000 bonded")
	for _, rule := range []int{flowRuleFIFO, flowRuleProRata} {
		require.Equal(t, "100", fate(tr, rule, "2025-01", flowBurned, flowSlashDetail, false), "the commission half of the stake loses its share under either rule")
		require.Equal(t, "999900", fate(tr, rule, "2025-01", flowRestakedOwn, "", false))
	}
	for k := range tr.Fates {
		require.NotContains(t, []string{flowSpent, flowSwapped, flowExchange}, k.Class, "a slash is neither a sale nor a spend")
	}
	require.Equal(t, "1000000", tr.Commission["2025-01"].String(), "a slash adds no income")
	require.True(t, w.Overdraw[flowRuleFIFO].IsZero())
}

func TestFlowTwoSlashesCompound(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 1000000)
	l.rows[r] = append(l.rows[r], flowDelegate(flowAt(1, 5), flowTestValidator, 1000000))
	l.slash(flowAt(2, 0), flowTestValidator, "0.0001")
	l.slash(flowAt(9, 0), flowTestValidator, "0.05")
	tr := flowSlashWallet(t, l)

	// 1,000,000 x 0.9999 = 999,900, then x 0.95 = 949,905.
	require.Equal(t, "949905", tr.order[0].simulated().String())
	require.Equal(t, "50095", fate(tr, flowRuleFIFO, "2025-01", flowBurned, flowSlashDetail, false))
	require.Equal(t, "949905", fate(tr, flowRuleProRata, "2025-01", flowRestakedOwn, "", false))
}

func TestFlowSlashMissesStakeUnbondedBefore(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 1000000)
	l.rows[r] = append(l.rows[r],
		flowDelegate(flowAt(1, 5), flowTestValidator, 1000000),
		flowEvent{Key: flowAt(2, 0), Category: flowCatUndelegate, Dir: "in", Amount: amt(1000000), To: r, Validator: flowTestValidator},
	)
	// Long after the unbonding began: the entry is past the infraction.
	l.slash(flowAt(3, 0), flowTestValidator, "0.0001")
	tr := flowSlashWallet(t, l)

	require.Equal(t, "1000000", tr.order[0].simulated().String())
	require.Equal(t, "0", fate(tr, flowRuleFIFO, "2025-01", flowBurned, flowSlashDetail, false))
	require.Equal(t, "1000000", fate(tr, flowRuleFIFO, "2025-01", flowUnbonding, "", false))
}

func TestFlowSlashReachesEntryBegunAfterInfraction(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 1000000)
	unbond := flowAt(2, 0)
	l.rows[r] = append(l.rows[r],
		flowDelegate(flowAt(1, 5), flowTestValidator, 1000000),
		flowEvent{Key: unbond, Category: flowCatUndelegate, Dir: "in", Amount: amt(400000), To: r, Validator: flowTestValidator},
		flowEvent{Key: flowAt(2, 1), Category: flowCatRedelegate, Dir: apiOut, Amount: amt(100000), From: r, Validator: flowTestOther, SrcValidator: flowTestValidator},
	)
	// The slash lands in the block after both entries began, so its downtime
	// infraction two blocks back reaches them.
	k := unbond
	k.Height += 2
	k.Time = k.Time.Add(2 * 60e9)
	l.slash(k, flowTestValidator, "0.0001")
	tr := flowSlashWallet(t, l)

	// 500,000 still bonded loses 50, the unbonding entry 40, the redelegation 10.
	require.Equal(t, "999900", tr.order[0].simulated().String())
	require.Equal(t, "100", fate(tr, flowRuleFIFO, "2025-01", flowBurned, flowSlashDetail, false))
	require.Equal(t, "399960", fate(tr, flowRuleFIFO, "2025-01", flowUnbonding, "", false))
	require.Equal(t, "99990", fate(tr, flowRuleFIFO, "2025-01", flowRestakedOther, flowTestOther, false))
}

func TestFlowDoubleSignReachesEntriesWithinUnbondingPeriod(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 1000000)
	l.rows[r] = append(l.rows[r],
		flowDelegate(flowAt(1, 5), flowTestValidator, 1000000),
		flowEvent{Key: flowAt(2, 0), Category: flowCatUndelegate, Dir: "in", Amount: amt(200000), To: r, Validator: flowTestValidator},
	)
	l.slash(flowAt(5, 0), flowTestValidator, "0.05")
	l.slashed[0].DoubleSign = true
	tr := flowSlashWallet(t, l)

	// 800,000 bonded loses 40,000 and the three-day-old entry 10,000.
	require.Equal(t, "950000", tr.order[0].simulated().String())
	require.Equal(t, "190000", fate(tr, flowRuleFIFO, "2025-01", flowUnbonding, "", false))
}

func TestFlowSlashOfValidatorOutsideTheSetChangesNothing(t *testing.T) {
	l := newFakeFlowLedger()
	r := flowTestReceiver
	l.commission(flowAt(1, 0), r, 1000000)
	l.rows[r] = append(l.rows[r], flowDelegate(flowAt(1, 5), flowTestValidator, 1000000))
	l.slash(flowAt(2, 0), "cosmosvaloper1elsewhere", "0.05")
	tr := flowSlashWallet(t, l)

	require.Equal(t, "1000000", tr.order[0].simulated().String())
	require.Equal(t, "1000000", fate(tr, flowRuleFIFO, "2025-01", flowRestakedOwn, "", false))
	for k := range tr.Fates {
		require.NotEqual(t, flowBurned, k.Class)
	}
}

func TestFlowPoolBurnKeepsTotals(t *testing.T) {
	for _, rule := range []int{flowRuleFIFO, flowRuleProRata} {
		p := &flowPool{rule: rule}
		p.add([]flowPart{{Origin: "a", Amount: amt(3)}, {Origin: "", Amount: amt(7)}})
		out := p.burnAmount(amt(1))
		sum := decimal.Zero
		for _, x := range out {
			sum = sum.Add(x.Amount)
		}
		require.Equal(t, "1", sum.String())
		require.Equal(t, "9", p.total().String())
		require.Equal(t, "10", p.total().Add(sum).String())
	}
}
