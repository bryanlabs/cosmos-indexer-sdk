package taxapi

import (
	"fmt"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

// The parser now moves a redirected reward from the delegator to its
// withdraw address. The commission trace already takes reward coins from
// what the distribution module paid each wallet, so its output must be the
// same with or without those rows: an operator account that is also a
// delegator paying its reward to a withdraw address, both of which then
// send everything to an exchange.
func TestFlowWithdrawAddressMovesChangeNothing(t *testing.T) {
	const withdraw = "cosmos1withdraw"
	kCommission, kReward, kSend, kWithdrawSend := flowAt(1, 0), flowAt(1, 5), flowAt(2, 0), flowAt(2, 1)
	row := func(k flowKey, sub int, category, amount, from, to, validator, trigger, detail string) tax.TaxableEvent {
		return tax.TaxableEvent{
			Timestamp: k.Time, BlockHeight: k.Height, MessageID: k.Message, SubIndex: sub, TxHash: fmt.Sprintf("TX%d", k.Message),
			Category: category, Amount: amount, Denom: apiUatom, FromAddr: from, ToAddr: to, ValidatorAddress: validator, RewardTrigger: trigger, Detail: detail,
		}
	}
	move := row(kReward, 1, apiTransfer, "50", flowTestReceiver, withdraw, "", tax.TriggerWithdrawAddress, `{"protocol":"distribution","reward_trigger":"claim"}`)
	before := map[string]walletRows{
		flowTestReceiver: {Events: []tax.TaxableEvent{
			row(kCommission, 0, apiCommission, "100", "", flowTestReceiver, flowTestValidator, "commission", ""),
			row(kReward, 0, apiReward, "50", "", flowTestReceiver, flowTestOther, "claim", ""),
			row(kSend, 0, apiTransfer, "100", flowTestReceiver, flowTestExchange, "", "", ""),
		}},
		withdraw: {Events: []tax.TaxableEvent{
			row(kWithdrawSend, 0, apiTransfer, "50", withdraw, flowTestExchange, "", "", ""),
		}},
	}
	after := map[string]walletRows{}
	for addr, rows := range before {
		events := append([]tax.TaxableEvent(nil), rows.Events...)
		after[addr] = walletRows{Events: append(events, move)}
	}
	// What the chain paid: the reward went to the withdraw address.
	paid := map[string]map[uint]flowRewardPayout{
		flowTestReceiver: {kCommission.Message: {Key: kCommission, Amount: amt(100)}},
		withdraw:         {kReward.Message: {Key: kReward, Amount: amt(50)}},
	}
	trace := func(ledger map[string]walletRows) (*flowTrace, map[string][]flowEvent) {
		l := newFakeFlowLedger()
		got := map[string][]flowEvent{}
		for addr, rows := range ledger {
			rows.Events = withoutWithdrawAddressMoves(rows.Events)
			out := commissionFlowEventsFromRows(rows, addr, apiUatom, nil)
			out = reconcileRewardPayouts(addr, out, paid[addr])
			got[addr] = out
			l.rows[addr] = out
		}
		return runFlowTest(t, l, flowTestReceiver, withdraw), got
	}
	trBefore, evBefore := trace(before)
	trAfter, evAfter := trace(after)
	require.Equal(t, evBefore, evAfter, "flow events")
	require.Equal(t, trBefore.Fates, trAfter.Fates, "fates")
	require.Equal(t, trBefore.Commission, trAfter.Commission, "commission")
	require.NotEmpty(t, trAfter.Fates)

	// commissionFlowEventsFromRows skips the rows by itself too.
	for addr := range before {
		require.Equal(t, commissionFlowEventsFromRows(before[addr], addr, apiUatom, nil), commissionFlowEventsFromRows(after[addr], addr, apiUatom, nil), addr)
	}
	// Recorded source-correction fingerprints still match.
	hash := move.TxHash
	require.Equal(t, correctionEventsSHA(before[flowTestReceiver].Events, hash, 0), correctionEventsSHA(after[flowTestReceiver].Events, hash, 0))
	require.Equal(t, correctionEventsSHA(before[flowTestReceiver].Events, hash, kReward.Message), correctionEventsSHA(after[flowTestReceiver].Events, hash, kReward.Message))
	// An ordinary transfer in the same transaction is still fingerprinted.
	plain := move
	plain.RewardTrigger = ""
	require.NotEqual(t, correctionEventsSHA(before[flowTestReceiver].Events, hash, 0), correctionEventsSHA(append(append([]tax.TaxableEvent(nil), before[flowTestReceiver].Events...), plain), hash, 0))
}
