package core

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stake "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

// A Hub 1 undelegation names shares. Its row takes the token amount from
// the recorded state change and the share evidence moves nothing.
func TestReplayHub1ShareValuedPrincipalComesFromState(t *testing.T) {
	a, _, v, w := replayAddresses()
	evidence := func(kind, label, validator, amount string) rr.Record {
		return rr.Record{Kind: kind, Phase: "message", Status: "committed", Label: label, Address: a, Delegator: a, Validator: validator, Amount: amount}
	}
	principal := rr.Record{Kind: "principal", Phase: "message", Status: "committed", Label: "undelegate", Address: a, Delegator: a, Validator: v, Denom: "uatom", Amount: "9"}
	marker := rr.Record{Kind: "accounting", Status: "committed", Label: "undelegate", Delegator: a, Validator: v}
	m := legacy.Message{AminoName: "cosmos-sdk/MsgUndelegate", Derived: &stake.MsgUndelegate{DelegatorAddress: a, ValidatorAddress: v}}
	rows, err := replayMessage(m, []rr.Record{evidence("share_request", "undelegate:requested_source_shares", v, "9.5"), evidence("staking_shares", "undelegate:source", v, "-9.5"), principal, marker})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "undelegate", rows[0].Category)
	require.Equal(t, "9", rows[0].Amount)
	require.Equal(t, "state", rows[0].Source)

	// The same evidence under another delegation or an unknown label stops.
	_, err = replayMessage(m, []rr.Record{evidence("staking_shares", "undelegate:source", w, "-9.5"), principal, marker})
	require.ErrorContains(t, err, `kind "staking_shares"`)
	_, err = replayMessage(m, []rr.Record{evidence("staking_shares", "undelegate:pool", v, "-9.5"), principal, marker})
	require.ErrorContains(t, err, `label "undelegate:pool"`)
	// Without the recorded principal there is no amount to give the row.
	_, err = replayMessage(m, []rr.Record{evidence("share_request", "undelegate:requested_source_shares", v, "9.5"), marker})
	require.Error(t, err)

	redelegate := legacy.Message{AminoName: "cosmos-sdk/MsgBeginRedelegate", Derived: &stake.MsgBeginRedelegate{DelegatorAddress: a, ValidatorSrcAddress: v, ValidatorDstAddress: w}}
	principal.Label, marker.Label = "redelegate", "redelegate"
	rows, err = replayMessage(redelegate, []rr.Record{evidence("staking_shares", "redelegate:source", v, "-9"), evidence("staking_shares", "redelegate:destination", w, "9"), principal, marker})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, w, rows[0].Validator)
}

func TestReplayHub1BondRequestMustMatchSignedAmount(t *testing.T) {
	a, _, v, _ := replayAddresses()
	coin := sdk.NewInt64Coin("uatom", 11)
	m := legacy.Message{AminoName: "cosmos-sdk/MsgDelegate", Derived: &stake.MsgDelegate{DelegatorAddress: a, ValidatorAddress: v, Amount: coin}}
	request := rr.Record{Kind: "principal_request", Phase: "message", Status: "committed", Label: "delegate", Address: a, Delegator: a, Validator: v, Denom: "uatom", Amount: "11"}
	debit := rr.Record{Kind: "movement", Phase: "message", Status: "committed", Label: "staking/delegate/bank.delegate", Address: a, Delegator: a, Validator: v, Denom: "uatom", Amount: "-11"}
	marker := rr.Record{Kind: "accounting", Status: "committed", Label: "delegate"}
	rows, err := replayMessage(m, []rr.Record{request, debit, marker})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	request.Amount = "12"
	_, err = replayMessage(m, []rr.Record{request, debit, marker})
	require.ErrorContains(t, err, "bond request differs")
}

// A movement the ledger has no label for stops the height and the error
// names the kind and label, with no address or amount in it.
func TestReplayUnlabelledMovementIsNamed(t *testing.T) {
	a, _, v, _ := replayAddresses()
	m := legacy.Message{AminoName: "cosmos-sdk/MsgUndelegate", Derived: &stake.MsgUndelegate{DelegatorAddress: a, ValidatorAddress: v}}
	_, err := replayMessage(m, []rr.Record{{Kind: "movement", Height: 5, Phase: "message", Status: "committed", Label: "staking/slash/bank.burn", Address: a, Denom: "uatom", Amount: "-4"}})
	require.ErrorIs(t, err, replayHeld)
	require.ErrorContains(t, err, `kind "movement" label "staking/slash/bank.burn"`)
	require.NotContains(t, err.Error(), a)

	h, items, r := replayFixture(t, 0, true)
	extra := rr.Record{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed", Label: "slashing/slash/bank.burn", Address: a, Denom: "uatom", Amount: "-4"}
	r.Records = append([]rr.Record{r.Records[0], extra}, r.Records[1:]...)
	_, err = LegacyReplayBlock(h, items, r)
	require.ErrorContains(t, err, `label "slashing/slash/bank.burn"`)
	require.NotContains(t, err.Error(), a)
}

func TestReplayHub1BlockIsAdmittedAndProvesItsRows(t *testing.T) {
	h, items, r := replayFixture(t, 0, true)
	h.Block.Block.ChainID = "cosmoshub-1"
	b, err := LegacyReplayBlock(h, items, r)
	require.NoError(t, err)
	require.Equal(t, "cosmoshub-1", b.Chain)
	want, err := LegacyReplayRecorded(r)
	require.NoError(t, err)
	got, err := LegacyReplayLiquid(b)
	require.NoError(t, err)
	require.Len(t, want, 2)
	require.True(t, got.Equal(want))

	// One uatom less on a row no longer explains the recording.
	b.Rows[0].Amount = "52"
	got, err = LegacyReplayLiquid(b)
	require.NoError(t, err)
	require.False(t, got.Equal(want))
	// A category with no balance rule is refused by name.
	b.Rows[0].Category = "slash"
	_, err = LegacyReplayLiquid(b)
	require.ErrorContains(t, err, `category "slash"`)
}

func TestReplayLiquidRulesAndExportOnce(t *testing.T) {
	a, b, v, _ := replayAddresses()
	block := ledger.Block{Chain: "cosmoshub-1", Height: 9, Rows: []ledger.Row{
		{Category: "gov_deposit", From: a, Amount: "5", Denom: "uatom"},
		{Category: "gov_deposit_refund", To: a, Amount: "2", Denom: "uatom"},
		{Category: "gov_deposit_burn", From: a, Amount: "3", Denom: "uatom"},
		{Category: "commission", To: a, Validator: v, Amount: "7", Denom: "uatom", Detail: replayPaidTo(b, "")},
		{Category: "undelegate", To: a, Validator: v, Amount: "100", Denom: "uatom"},
		{Category: "unbonding_complete", To: a, Validator: v, Amount: "40", Denom: "uatom"},
	}}
	got, err := LegacyReplayLiquid(block)
	require.NoError(t, err)
	require.Equal(t, ReplayNet{a + "\x00uatom": "37", b + "\x00uatom": "7", replayGovBurn() + "\x00uatom": "3"}, got)

	export := rr.Height{Number: 9}
	payout := rr.Record{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported", Label: "delegator_reward/bank.add", Address: b, Validator: v, Delegator: a, Denom: "uatom", Amount: "3"}
	closing := []rr.Record{{Kind: "accounting", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}, {Kind: "export_completed", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}}
	export.Records = append([]rr.Record{payout}, closing...)
	out, err := LegacyReplayExport(ledger.Block{Chain: "cosmoshub-1", Height: 9}, export)
	require.NoError(t, err)
	require.Len(t, out.Rows, 1)
	paid, err := LegacyReplayLiquid(out)
	require.NoError(t, err)
	recorded, err := LegacyReplayRecorded(export)
	require.NoError(t, err)
	require.True(t, paid.Equal(recorded))
	require.Equal(t, ReplayNet{b + "\x00uatom": "3"}, paid)

	export.Records = append([]rr.Record{payout, payout}, closing...)
	_, err = LegacyReplayExport(ledger.Block{Chain: "cosmoshub-1", Height: 9}, export)
	require.ErrorContains(t, err, "twice")
	_, err = LegacyReplayExport(ledger.Block{Chain: "cosmoshub-3", Height: 9}, export)
	require.Error(t, err)
}
