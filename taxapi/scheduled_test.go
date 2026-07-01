package taxapi

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestBuildScheduleD(t *testing.T) {
	rows := []Form8949Row{
		{Proceeds: d(12), CostBasis: d(8), GainLoss: d(4), LongTerm: false},
		{Proceeds: d(5), CostBasis: d(9), GainLoss: d(-4), LongTerm: false}, // short-term loss
		{Proceeds: d(20), CostBasis: d(10), GainLoss: d(10), LongTerm: true},
	}
	sd := BuildScheduleD(rows)

	if !sd.ShortTermGainLoss.Equal(decimal.Zero) { // 4 + (-4)
		t.Fatalf("short-term gain wrong: %v", sd.ShortTermGainLoss)
	}
	if !sd.ShortTermProceeds.Equal(d(17)) || !sd.ShortTermCostBasis.Equal(d(17)) {
		t.Fatalf("short-term totals wrong: %v / %v", sd.ShortTermProceeds, sd.ShortTermCostBasis)
	}
	if !sd.LongTermGainLoss.Equal(d(10)) {
		t.Fatalf("long-term gain wrong: %v", sd.LongTermGainLoss)
	}
	if !sd.NetGainLoss.Equal(d(10)) { // 0 + 10
		t.Fatalf("net gain wrong: %v", sd.NetGainLoss)
	}
}
