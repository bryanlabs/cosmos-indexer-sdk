package taxapi

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
)

func TestFlowSelfBondsFromCreateValidator(t *testing.T) {
	op, err := bech32.ConvertAndEncode(flowValoperHRP, bytes.Repeat([]byte{7}, 20))
	require.NoError(t, err)
	acct := operatorAccount(op)
	require.NotEmpty(t, acct)
	k := flowAt(1, 0)
	events := groupLedgerRows([]ledgerAttrRow{
		{EventID: 1, MessageID: 9, Height: k.Height, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "validator", Value: op},
		{EventID: 1, MessageID: 9, Height: k.Height, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "amount", Value: "10000000uatom"},
		// Older SDKs log a bare amount.
		{EventID: 2, MessageID: 11, Height: k.Height + 1, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "validator", Value: op},
		{EventID: 2, MessageID: 11, Height: k.Height + 1, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "amount", Value: "5"},
		{EventID: 3, MessageID: 12, Height: k.Height + 2, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "validator", Value: "not-a-valoper"},
		{EventID: 3, MessageID: 12, Height: k.Height + 2, TimeStamp: k.Time, Phase: 1, Type: "create_validator", Key: "amount", Value: "1uatom"},
	})
	got, err := selfBondsFrom(events, "uatom")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[acct], 2)
	b := got[acct][0]
	require.Equal(t, flowCatDelegate, b.Category)
	require.Equal(t, apiOut, b.Dir)
	require.Equal(t, "10000000", b.Amount.String())
	require.Equal(t, acct, b.From)
	require.Equal(t, op, b.Validator)
	require.Equal(t, uint(9), b.Key.Message)
	require.Equal(t, "5", got[acct][1].Amount.String())
}

// A self-bond the ledger has no delegate row for leaves liquid coin for
// stake, so the wallet's later undelegation overdraws and the trace ends
// holding the bond twice. This is the 10, 1.5 and 13 ATOM drift of three
// commission roots, each from a MsgCreateValidator indexed before the parser
// classified self-bonds.
func TestFlowSelfBondWithoutLedgerRowIsStaked(t *testing.T) {
	r := flowTestReceiver
	bond := flowEvent{Key: flowAt(1, 0), Category: flowCatDelegate, Dir: apiOut, Amount: amt(10), From: r, Validator: flowTestValidator}
	build := func(bonds []flowEvent) *flowTrace {
		l := newFakeFlowLedger()
		l.openings[r] = flowHoldings{Liquid: amt(12)}
		l.commission(flowAt(2, 0), r, 3)
		l.rows[r] = append(l.rows[r],
			flowEvent{Key: flowAt(3, 0), Category: flowCatUndelegate, Dir: "in", Amount: amt(10), To: r, Validator: flowTestValidator},
			flowEvent{Key: flowAt(4, 0), Category: apiFee, Dir: apiOut, Amount: amt(1), From: r},
		)
		l.rows[r] = withSelfBonds(l.rows[r], bonds, flowTestStart, flowTestStart.AddDate(0, 3, 0))
		return runFlowTest(t, l, r)
	}

	// The chain holds 12 - 10 + 3 - 1 = 4 liquid and 10 unbonding.
	tr := build([]flowEvent{bond})
	w := tr.Wallets[r]
	require.Equal(t, "14", w.simulated().String())
	for rule := 0; rule < flowRules; rule++ {
		require.True(t, w.Overdraw[rule].IsZero(), flowRuleNames[rule])
	}

	// Without the bond the trace holds 24 and overdraws the undelegation.
	tr = build(nil)
	require.Equal(t, "24", tr.Wallets[r].simulated().String())
	require.Equal(t, "10", tr.Wallets[r].Overdraw[flowRuleFIFO].String())
}

func TestFlowSelfBondKeepsLedgerRowAndWindow(t *testing.T) {
	r := flowTestReceiver
	k := flowAt(1, 0)
	row := flowEvent{Key: k, Category: flowCatDelegate, Dir: apiOut, Amount: amt(10), From: r, Validator: flowTestValidator}
	fee := flowEvent{Key: flowAt(0, 5), Category: apiFee, Dir: apiOut, Amount: amt(1), From: r}
	bond := row
	early := row
	early.Key = flowKey{Time: flowTestStart.AddDate(0, 0, -1), Height: 1, Message: 3}
	late := row
	late.Key = flowKey{Time: flowTestStart.AddDate(0, 3, 0), Height: 2, Message: 4}
	end := flowTestStart.AddDate(0, 3, 0)

	// The ledger already booked this message's self-bond.
	got := withSelfBonds([]flowEvent{fee, row}, []flowEvent{bond, early, late}, flowTestStart, end)
	require.Equal(t, []flowEvent{fee, row}, got)

	// Missing from the ledger, it is added in key order.
	other := flowEvent{Key: flowAt(2, 0), Category: apiFee, Dir: apiOut, Amount: amt(1), From: r}
	got = withSelfBonds([]flowEvent{fee, other}, []flowEvent{bond, early, late}, flowTestStart, end)
	require.Equal(t, []flowEvent{fee, bond, other}, got)
}
