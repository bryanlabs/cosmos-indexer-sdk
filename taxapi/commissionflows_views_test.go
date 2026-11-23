package taxapi

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func positive[K comparable](m map[K]decimal.Decimal) map[K]string {
	out := map[K]string{}
	for k, v := range m {
		if !v.IsZero() {
			out[k] = v.String()
		}
	}
	return out
}

// Every view a trace marks exact matches a separate trace at that limit,
// rows and stops, over random ledgers.
func TestFlowHopViewsMatchSeparateTraces(t *testing.T) {
	exact, inexact, differs := 0, 0, 0
	for _, c := range []struct {
		hops  int
		views []int
	}{{8, []int{3, 4, 5, 6, 7}}, {10, []int{6, 7, 8}}} {
		for seed := int64(1); seed <= 40; seed++ {
			base, group := randomFlowLedger(seed, 300)
			asOf := flowTestStart.AddDate(0, 3, 0)
			tr := newFlowTrace(flowTestValidator, flowTestStart, asOf, base)
			tr.maxHops = c.hops
			tr.withViews(c.views)
			require.NoError(t, tr.run(group))

			plain, _ := randomFlowLedger(seed, 300)
			top := newFlowTrace(flowTestValidator, flowTestStart, asOf, plain)
			top.maxHops = c.hops
			require.NoError(t, top.run(group))
			require.Equal(t, flowTraceDigest(top), flowTraceDigest(tr), "views leave the trace's own result unchanged, seed %d", seed)

			for _, v := range tr.views {
				sep, _ := randomFlowLedger(seed, 300)
				at := newFlowTrace(flowTestValidator, flowTestStart, asOf, sep)
				at.maxHops = v.Hops
				require.NoError(t, at.run(group))
				same := assertEqualMaps(positive(at.Fates), positive(v.Fates)) && assertEqualMaps(positive(at.Stops), positive(v.Stops))
				if v.Conflicts == 0 {
					exact++
					require.Equal(t, positive(at.Fates), positive(v.Fates), "hops %d view %d seed %d rows", c.hops, v.Hops, seed)
					require.Equal(t, positive(at.Stops), positive(v.Stops), "hops %d view %d seed %d stops", c.hops, v.Hops, seed)
				} else {
					inexact++
					if !same {
						differs++
					}
				}
			}
		}
	}
	t.Logf("exact views %d, views with conflicts %d (%d of them differ from a separate run)", exact, inexact, differs)
	require.Greater(t, exact, 0)
}

func assertEqualMaps[K comparable](a, b map[K]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A chain past the view's limit becomes one hop-limit stop at it.
func TestFlowHopViewStopsChainAtItsLimit(t *testing.T) {
	l, w := hopChainLedger()
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	tr.maxHops = 8
	tr.withViews([]int{3, 5})
	require.NoError(t, tr.run([]string{w[0]}))
	for _, v := range tr.views {
		l2, _ := hopChainLedger()
		at := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l2)
		at.maxHops = v.Hops
		require.NoError(t, at.run([]string{w[0]}))
		require.Zero(t, v.Conflicts)
		require.Equal(t, positive(at.Fates), positive(v.Fates))
		require.Equal(t, positive(at.Stops), positive(v.Stops))
	}
	require.Equal(t, "100", tr.views[0].Stops[FlowStopKey{Rule: flowRuleFIFO, Class: flowBeyondHops, From: w[3], To: w[4], Hop: 3}].String())
}

// Coins sent from past a view's limit back to a wallet inside it make the
// view inexact.
func TestFlowHopViewCountsReturnAcrossLimit(t *testing.T) {
	l, w := hopChainLedger()
	l.send(flowAt(20, 0), w[6], w[2], 50)
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), l)
	tr.maxHops = 8
	tr.withViews([]int{3, 7})
	require.NoError(t, tr.run([]string{w[0]}))
	require.Positive(t, tr.viewAt(3).Conflicts)
	require.Zero(t, tr.viewAt(7).Conflicts)
}
