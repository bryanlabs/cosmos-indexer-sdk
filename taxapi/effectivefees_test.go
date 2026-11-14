package taxapi

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestEffectiveChargedCoins(t *testing.T) {
	for _, raw := range []string{"-1uatom", "1.5uatom", "1", "uatom", "1uatom,", "1uatom, 2uatom", " 1uatom", "1x"} {
		_, err := effectiveChargedCoins(raw)
		require.Error(t, err)
	}
	got, err := effectiveChargedCoins("12uatom,3voucher,4uatom")
	require.NoError(t, err)
	require.Equal(t, []feeRec{{Amount: "16", Denom: "uatom"}, {Amount: "3", Denom: "voucher"}}, got)
	got, err = effectiveChargedCoins("")
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = effectiveChargedCoins("999999999999999999999999999999999999999999uatom")
	require.NoError(t, err)
	require.Equal(t, "999999999999999999999999999999999999999999", got[0].Amount)
}
