package balancedays

import (
	"math/big"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"
)

// The arithmetic follows the staking module: shares issued at the current
// price, the last share takes every token left, and a slash takes its share
// of entries made at or after the infraction before the validator's tokens.
func TestExactStakingArithmetic(t *testing.T) {
	l := &ledger{wallets: map[string]*wallet{}, exact: true, vals: map[string]*valState{}, reds: map[string][]*redEntry{},
		members: map[string]map[string]bool{}, touched: map[string]bool{}}
	alice, bob := l.at("alice"), l.at("bob")
	require.True(t, l.delegate(alice, "alice", "val", sdkmath.NewInt(3)).Equal(sdkmath.LegacyNewDec(3)))
	l.vals["val"].tokens = sdkmath.NewInt(2)
	// Three shares on two tokens: one token buys one and a half shares.
	require.Equal(t, "1.500000000000000000", l.delegate(bob, "bob", "val", sdkmath.NewInt(1)).String())
	require.Equal(t, "1", l.undelegate(alice, "val", sdkmath.NewInt(1)).String())
	require.Equal(t, "1.500000000000000000", alice.pairs["val"].dec().String())
	// The last shares take what is left.
	require.Equal(t, "1", l.undelegate(alice, "val", sdkmath.NewInt(1)).String())
	require.Equal(t, "1", l.undelegate(bob, "val", sdkmath.NewInt(1)).String())
	require.True(t, l.vals["val"].tokens.IsZero())
	require.True(t, l.vals["val"].shares.IsZero())
	require.Zero(t, l.exactAnomalies)
	// Asking for more than the delegation holds is what the chain refuses.
	l.delegate(bob, "bob", "val", sdkmath.NewInt(10))
	l.undelegate(bob, "val", sdkmath.NewInt(11))
	require.Equal(t, 1, l.exactAnomalies)

	when := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	m := &ledger{wallets: map[string]*wallet{}, exact: true, vals: map[string]*valState{"val": {tokens: sdkmath.NewInt(2_000_000), shares: sdkmath.LegacyNewDec(2_000_000)}},
		reds: map[string][]*redEntry{}, members: map[string]map[string]bool{}, touched: map[string]bool{}}
	carol := m.at("carol")
	carol.entries["val"] = []*entry{
		{balance: big.NewInt(500_000), initial: big.NewInt(500_000), height: 10, completion: when.Add(time.Hour)},
		{balance: big.NewInt(500_000), initial: big.NewInt(500_000), height: 7, completion: when.Add(time.Hour)},
		{balance: big.NewInt(500_000), initial: big.NewInt(500_000), height: 10, completion: when},
	}
	touch := func(address string) *wallet { return m.at(address) }
	m.applySlash(&slash{height: 12, when: when, operator: "val", power: 3, fraction: fractionDowntime, infraction: 10}, touch)
	// 300 to take: 50 from the one entry made at the infraction and not yet
	// paid, 250 from the validator.
	require.Equal(t, []string{"499950", "500000", "500000"},
		[]string{carol.entries["val"][0].balance.String(), carol.entries["val"][1].balance.String(), carol.entries["val"][2].balance.String()})
	require.Equal(t, "1999750", m.vals["val"].tokens.String())
	require.Equal(t, 1, m.slashedEntries)
}

func TestDecFromRat(t *testing.T) {
	d, err := decFromRat(big.NewRat(3, 2))
	require.NoError(t, err)
	require.Equal(t, "1.500000000000000000", d.String())
	_, err = decFromRat(big.NewRat(1, 3))
	require.Error(t, err)
	require.Equal(t, 0, ratFromDec(d).Cmp(big.NewRat(3, 2)))
}
