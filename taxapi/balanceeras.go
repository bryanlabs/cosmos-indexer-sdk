package taxapi

import (
	"fmt"
)

// Heights on cosmoshub-4 where the events the balance ledger reads changed
// shape. Both were read from the chain. Gaia v6 (SDK 0.44) began emitting
// coin_received and coin_spent at block 8695000; block 8694999 has none, so
// before it a wallet's liquid movements show only as transfer events, a
// delegation's bond as the delegate event and nothing at all on a failed
// transaction's fee. Gaia v15 (SDK 0.47) began naming the delegator on
// delegate and unbond events at block 19639600; before it the ledger reads
// whose stake moved from the same message (attributeRedelegations).
const hubCoinEventsFrom = 8695000

// preCoinEventsReason refuses a date whose walk back would cross blocks with
// no coin events. It returns "" when the date can be rebuilt.
func preCoinEventsReason(chain string, height int64) string {
	if chainParam(chain) != apiMainnet || height+1 >= hubCoinEventsFrom {
		return ""
	}
	return fmt.Sprintf("balances before block %d (Gaia v6, 2021-12-14) cannot be rebuilt yet: the chain emitted no coin_received or coin_spent events before then, and rebuilding from its transfer events is not deployed", hubCoinEventsFrom)
}
