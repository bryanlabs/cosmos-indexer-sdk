package taxapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Block 8694999 is the last without coin events, so a date ending there
// still undoes only blocks that have them.
func TestPreCoinEventsReason(t *testing.T) {
	require.Empty(t, preCoinEventsReason("", hubCoinEventsFrom-1))
	require.Empty(t, preCoinEventsReason(apiMainnet, 30000000))
	require.Contains(t, preCoinEventsReason("", hubCoinEventsFrom-2), "8695000")
	require.Contains(t, preCoinEventsReason("cosmoshub-4", 5200791), "Gaia v6")
	require.Empty(t, preCoinEventsReason(apiTestnet, 100))
}
