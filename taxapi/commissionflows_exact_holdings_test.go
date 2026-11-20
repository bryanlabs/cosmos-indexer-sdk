package taxapi

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrivateHistoricalHoldingsRejectApproximateSource(t *testing.T) {
	b := HistoricalBalance{Status: balanceApproximate, Height: 123, Lines: []BalanceLine{{Denom: apiUatom, Bucket: apiLiquid, AmountBase: "10"}}}
	_, source, err := flowHistoricalHoldings(b, apiUatom, true)
	require.NoError(t, err)
	require.Empty(t, source.Kind, "strict loader must fall through to exact archive")
	h, source, err := flowHistoricalHoldings(b, apiUatom, false)
	require.NoError(t, err)
	require.Equal(t, "ledger_approximate", source.Kind, "legacy behaviour is unchanged")
	require.Equal(t, "10", h.Liquid.String())
	b.Status = balanceExact
	h, source, err = flowHistoricalHoldings(b, apiUatom, true)
	require.NoError(t, err)
	require.Equal(t, "ledger_exact", source.Kind)
	require.Equal(t, int64(123), source.Height)
	require.Equal(t, "10", h.Liquid.String())
	b.Lines[0].Approximate = true
	_, source, err = flowHistoricalHoldings(b, apiUatom, true)
	require.NoError(t, err)
	require.Empty(t, source.Kind, "line-level approximation cannot pass an exact envelope")
	b.Lines[0].Approximate = false
	b.Height = 0
	_, source, err = flowHistoricalHoldings(b, apiUatom, true)
	require.NoError(t, err)
	require.Empty(t, source.Kind, "an unbound holding height is not exact evidence")
}
