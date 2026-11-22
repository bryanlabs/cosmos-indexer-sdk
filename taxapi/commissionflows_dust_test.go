package taxapi

import (
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestProRataRetainsSubDustCommissionRemainder(t *testing.T) {
	p := &flowPool{rule: flowRuleProRata, parts: []flowPart{
		{Origin: "2025-01", Amount: decimal.RequireFromString("1.0000001")},
		{Origin: "2025-02", Amount: decimal.RequireFromString("1.0000001")},
	}}
	before := p.total()
	out, short := p.take(decimal.NewFromInt(2))
	require.True(t, short.IsZero())
	sum := p.total()
	for _, part := range out {
		sum = sum.Add(part.Amount)
	}
	require.True(t, sum.Equal(before), "taking coins must preserve even sub-dust tagged remainder")
	require.Len(t, p.parts, 2)
	require.True(t, p.parts[0].Amount.Equal(decimal.RequireFromString("0.0000001")))
}

func TestProRataRepeatedSmallRemaindersConserveEveryOrigin(t *testing.T) {
	p := &flowPool{rule: flowRuleProRata}
	expected := map[string]decimal.Decimal{}
	actual := map[string]decimal.Decimal{}
	for i, origin := range []string{"2025-01", "2025-02", "2025-03"} {
		amount := decimal.NewFromInt(int64(i + 1)).Add(decimal.RequireFromString("0.0000001"))
		expected[origin] = amount
		p.add([]flowPart{{Origin: origin, Amount: amount}})
	}
	for i := 0; i < 100; i++ {
		out, short := p.take(decimal.RequireFromString("0.05999999"))
		require.True(t, short.IsZero())
		for _, part := range out {
			actual[part.Origin] = actual[part.Origin].Add(part.Amount)
		}
	}
	for _, part := range p.parts {
		actual[part.Origin] = actual[part.Origin].Add(part.Amount)
	}
	for origin, amount := range expected {
		require.True(t, actual[origin].Equal(amount), "origin must be conserved exactly")
	}
}
