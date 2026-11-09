package taxapi

import (
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// Heights on cosmoshub-4 where the events the balance ledger reads changed
// shape. Both were read from the chain. Gaia v6 (SDK 0.44) began emitting
// coin_received and coin_spent at block 8695000; block 8694999 has none, so
// before it a wallet's liquid movements show only as transfer events, a
// delegation's bond as the delegate event and nothing at all on a failed
// transaction's fee. Gaia v15 (SDK 0.47) began naming the delegator on
// delegate and unbond events at block 19639600; before it those events and
// the merged message logs of SDK 0.42 to 0.45 do not say whose stake moved.
const (
	hubCoinEventsFrom       = 8695000
	hubStakingDelegatorFrom = 19639600
)

// stakingCategories are the taxable rows that record a wallet's own staking
// principal, written from the message, so they name the delegator on every
// SDK.
var stakingCategories = []string{string(tax.CategoryDelegate), string(tax.CategoryUndelegate), string(tax.CategoryRedelegate), string(tax.CategoryCancelUnbonding)}

// preCoinEventsReason refuses a date whose walk back would cross blocks with
// no coin events. It returns "" when the date can be rebuilt.
func preCoinEventsReason(chain string, height int64) string {
	if chainParam(chain) != apiMainnet || height+1 >= hubCoinEventsFrom {
		return ""
	}
	return fmt.Sprintf("balances before block %d (Gaia v6, 2021-12-14) cannot be rebuilt yet: the chain emitted no coin_received or coin_spent events before then, and rebuilding from its transfer events is not deployed", hubCoinEventsFrom)
}

// unnamedStakingHeights is the ascending heights in (lo, the v15 boundary)
// where the wallet moved its own stake, on chains whose staking events did
// not name the delegator then.
func (s *Server) unnamedStakingHeights(chain, addr string, lo int64) ([]int64, error) {
	if chainParam(chain) != apiMainnet || lo+1 >= hubStakingDelegatorFrom {
		return nil, nil
	}
	var heights []int64
	err := s.db.Raw(`
SELECT DISTINCT block_height FROM (
  SELECT block_height FROM taxable_events WHERE from_addr = @wallet AND category IN @cats AND block_height > @lo AND block_height < @hi
  UNION ALL
  SELECT block_height FROM taxable_events WHERE to_addr = @wallet AND category IN @cats AND block_height > @lo AND block_height < @hi
) s ORDER BY block_height`,
		map[string]any{"wallet": addr, "cats": stakingCategories, "lo": lo, "hi": int64(hubStakingDelegatorFrom)}).
		Scan(&heights).Error
	return heights, err
}
