package books

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Made-up wallets in the shape of a Cosmos Hub address.
func wallet(n int) string {
	return "cosmos1" + strings.Repeat("q", 37) + string("pzry9x8gf2tvdw0s"[n])
}

func donor(name string, source, dest string, amount string) string {
	return fmt.Sprintf(`
func %s() FundRecoveryMessage {
	sourceAddress, _ := sdk.AccAddressFromBech32("%s")
	destAddress, _ := sdk.AccAddressFromBech32("%s")
	return FundRecoveryMessage{
		signedMessage: "x",
		sourceAddress: sourceAddress,
		destAddress:   destAddress,
		destBalance:   sdk.Coins{sdk.NewInt64Coin("uatom", %s)},
	}
}
`, name, source, dest, amount)
}

const fileHead = `package app

type FundRecoveryMessage struct {
	signedMessage string
}
`

func TestTheGenesisChangeListIsReadOnlyFromAFileItFullyUnderstands(t *testing.T) {
	good := fileHead + donor("btcDonor1", wallet(0), wallet(1), "900") + donor("ethDonor1", wallet(2), wallet(3), "468")
	list, err := parseRecoveries([]byte(good))
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, wallet(0), list[0].Source)
	require.Equal(t, wallet(1), list[0].Recovery)
	require.Equal(t, "900", list[0].Amount.String())
	require.Equal(t, wallet(3), list[1].Recovery)
	require.Equal(t, "468", list[1].Amount.String())

	_, err = LoadProp29([]byte(good))
	require.ErrorContains(t, err, "recorded sha256", "a file that is not the pinned one is refused whatever it lists")

	for name, text := range map[string]string{
		"a wallet listed twice":             fileHead + donor("btcDonor1", wallet(0), wallet(1), "900") + donor("ethDonor1", wallet(2), wallet(0), "468"),
		"a wallet that is its own recovery": fileHead + donor("btcDonor1", wallet(0), wallet(0), "900"),
		"an amount of zero":                 fileHead + donor("btcDonor1", wallet(0), wallet(1), "0"),
		"an address of another chain":       fileHead + donor("btcDonor1", "osmo1"+strings.Repeat("q", 38), wallet(1), "900"),
		"a recovery under another name":     fileHead + donor("btcDonor1", wallet(0), wallet(1), "900") + donor("ltcDonor1", wallet(2), wallet(3), "468"),
		"two amounts in one recovery": fileHead + strings.Replace(donor("btcDonor1", wallet(0), wallet(1), "900"),
			"\t}\n}", "\t}\n\t_ = sdk.Coins{sdk.NewInt64Coin(\"uatom\", 5)}\n}", 1),
		"no recovery wallet": fileHead + strings.Replace(donor("btcDonor1", wallet(0), wallet(1), "900"), "\tdestAddress, _ :=", "\t_, _ =", 1),
	} {
		_, err := parseRecoveries([]byte(text))
		require.Error(t, err, name)
	}
}

// With LEGACY_PROP29_FILE naming a copy of the public file, the pinned file
// gives seven recoveries over fourteen distinct wallets.
func TestThePinnedProposal29FileGivesSevenRecoveries(t *testing.T) {
	path := os.Getenv("LEGACY_PROP29_FILE")
	if path == "" {
		t.Skip("LEGACY_PROP29_FILE is not set")
	}
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	change, err := LoadProp29(source)
	require.NoError(t, err)
	require.Len(t, change.Recoveries, 7)
	seen := map[string]bool{}
	for _, entry := range change.Recoveries {
		seen[entry.Source], seen[entry.Recovery] = true, true
		require.Positive(t, entry.Amount.Sign())
	}
	require.Len(t, seen, 14)
	_, err = LoadProp29(append(source, '\n'))
	require.ErrorContains(t, err, "recorded sha256")
}
