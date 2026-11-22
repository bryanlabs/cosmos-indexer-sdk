package taxapi

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

const (
	routeRouter = "cosmos1router"
	routeUser   = "cosmos1user"
	routeSender = "cosmos1origin"
	// A Skip Go forward of IBC Eureka WETH through the v2 client 08-wasm-1369,
	// the shape of the routed sends whose error acknowledgement the router's
	// source callback paid straight on to the user.
	routeTrace   = "transfer/08-wasm-1369/0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"
	routePacket  = `{"ibc_generation":"v2","packet_sequence":"41","packet_source_client":"08-wasm-1369","packet_destination_client":"client-2"}`
	routeOnward  = `{"ibc_generation":"v2","packet_sequence":"41","packet_source_client":"08-wasm-1369","packet_destination_client":"client-2","refund_passed_through":"true","principal_return":"false"}`
	routeAmount  = "120000000000000000"
	routeForward = `{"protocol":"skip","routed_from_hub":"true"}`
)

// The router received the coins, forwarded them, got them back on the error
// acknowledgement and paid them to the user in the same message. Its forward
// is undone like any refunded send; the transfer to the user is untouched.
func passThroughEvents() []tax.TaxableEvent {
	recv := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	return []tax.TaxableEvent{
		{ID: 1, MessageID: 10, Category: string(tax.CategoryIBCIn), FromAddr: routeSender, ToAddr: routeRouter, Amount: routeAmount, Denom: ibcRefundDenom(routeTrace), Timestamp: recv},
		{ID: 2, MessageID: 10, Category: string(tax.CategoryIBCOut), FromAddr: routeRouter, Amount: routeAmount, Denom: routeTrace, Detail: routePacket, Timestamp: recv},
		{ID: 3, MessageID: 20, Category: string(tax.CategoryTransfer), FromAddr: routeRouter, ToAddr: routeUser, Amount: routeAmount, Denom: ibcRefundDenom(routeTrace), Detail: routeOnward, Timestamp: recv.Add(30 * time.Second)},
	}
}

func TestPassedThroughRefundUndoesTheRoutersSend(t *testing.T) {
	events := passThroughEvents()
	end := events[0].Timestamp.AddDate(1, 0, 0)
	pairs := matchIBCRefunds(events)
	require.Equal(t, map[uint]string{2: ibcRefundRoleSend, 3: ibcRefundRolePassedThrough}, pairs)
	require.True(t, ibcBasisReadiness(events, end).ExportReady)

	reconcileIBCRefunds(events, pairs)
	require.Equal(t, string(tax.CategoryIBCIn), events[0].Category)
	require.Equal(t, string(tax.CategoryProtocolDeposit), events[1].Category)
	require.Equal(t, ibcRefundedSend, events[1].RewardTrigger)
	want := passThroughEvents()[2]
	require.Equal(t, want, events[2], "the transfer to the user keeps its category, parties and amount")
}

// The user's own report never loads the router's send, so the transfer in is
// neither matched nor refused there.
func TestPassedThroughRefundLeavesTheRecipientAlone(t *testing.T) {
	events := passThroughEvents()[2:]
	require.Empty(t, matchIBCRefunds(events))
	require.True(t, ibcBasisReadiness(events, events[0].Timestamp.AddDate(1, 0, 0)).ExportReady)
}

// When the user started the send through an entry point, the send is booked to
// the user and the router owns no send for the packet: the transfer alone is
// the return, nothing to undo and nothing to refuse.
func TestPassedThroughRefundWithoutARouterSend(t *testing.T) {
	events := passThroughEvents()
	events = append(events[:1], events[2:]...)
	require.Empty(t, matchIBCRefunds(events))
	require.True(t, ibcBasisReadiness(events, events[0].Timestamp.AddDate(1, 0, 0)).ExportReady)
}

// A router send for the packet that cannot be paired exactly is refused, never
// netted against the transfer.
func TestPassedThroughRefundRefusesAnInexactPair(t *testing.T) {
	for name, change := range map[string]func([]tax.TaxableEvent) []tax.TaxableEvent{
		"another amount": func(e []tax.TaxableEvent) []tax.TaxableEvent { e[2].Amount = "119000000000000000"; return e },
		"different coin": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			e[2].Denom = ibcRefundDenom("transfer/08-wasm-1370/0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2")
			return e
		},
		"returned before the send": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			e[2].Timestamp = e[1].Timestamp.Add(-time.Second)
			return e
		},
		"second send for the packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			extra := e[1]
			extra.ID = 4
			return append(e, extra)
		},
		"second pass-through for the packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			extra := e[2]
			extra.ID = 5
			return append(e, extra)
		},
		"a refund row for the same packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			return append(e, tax.TaxableEvent{ID: 6, Category: string(tax.CategoryProtocolWithdrawal), ToAddr: routeRouter, Amount: routeAmount, Denom: e[2].Denom, Detail: routePacket, RewardTrigger: ibcErrorAckRefund, Timestamp: e[2].Timestamp})
		},
		"transfer without a row id": func(e []tax.TaxableEvent) []tax.TaxableEvent { e[2].ID = 0; return e },
	} {
		t.Run(name, func(t *testing.T) {
			events := change(passThroughEvents())
			require.Empty(t, matchIBCRefunds(events))
			r := ibcBasisReadiness(events, events[0].Timestamp.AddDate(1, 0, 0))
			require.False(t, r.ExportReady)
			require.Contains(t, r.ReviewRequired, ibcRefundBasisRequired)
		})
	}
}

// A router send whose packet identity could not be recovered may be the one
// the transfer returned, so it blocks instead of being guessed from amounts.
func TestPassedThroughRefundRefusesAnUnidentifiedRouterSend(t *testing.T) {
	events := passThroughEvents()
	events[1].Detail = routeForward
	require.Empty(t, matchIBCRefunds(events))
	r := ibcBasisReadiness(events, events[0].Timestamp.AddDate(1, 0, 0))
	require.False(t, r.ExportReady)
	require.Contains(t, r.ReviewRequired, ibcPacketIdentityRequired)
}

// An ordinary transfer is never read as a pass-through, whatever its detail
// text happens to contain.
func TestOnlyMarkedTransfersArePassThroughs(t *testing.T) {
	for _, e := range []tax.TaxableEvent{
		{Category: string(tax.CategoryTransfer), Detail: `{"memo":"refund_passed_through"}`},
		{Category: string(tax.CategoryTransfer), Detail: `{"refund_passed_through":"false"}`},
		{Category: string(tax.CategoryIBCIn), Detail: routeOnward},
	} {
		require.False(t, isIBCRefundPassThrough(e))
	}
	require.True(t, isIBCRefundPassThrough(passThroughEvents()[2]))
}
