package core

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/stretchr/testify/require"
)

func TestReplayEndBlockActualKeeperCauses(t *testing.T) {
	a, b, v, _ := replayAddresses()
	movement := func(label, address, amount string) rr.Record {
		return rr.Record{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "end_block", Status: "committed", Label: label, Address: address, Amount: amount, Denom: "uatom", Delegator: a}
	}
	for _, name := range []string{"completion", "removed commission", "refund", "burn"} {
		t.Run(name, func(t *testing.T) {
			h, items, r := replayFixture(t, 0, true)
			var extra []rr.Record
			category := ""
			switch name {
			case "completion":
				x := movement("staking/unbonding_complete/bank.undelegate", a, "5")
				x.Validator = v
				extra = []rr.Record{x}
				category = "unbonding_complete"
			case "removed commission":
				x := movement("distribution/removed_validator_commission/bank.add", b, "5")
				x.Validator = v
				extra = []rr.Record{x}
				category = "commission"
			case "refund":
				extra = []rr.Record{movement("gov/deposit_refund/bank.send", replayGovEscrow(), "-5"), movement("gov/deposit_refund/bank.send", a, "5")}
				category = "gov_deposit_refund"
			case "burn":
				extra = []rr.Record{movement("gov/deposit_burn/bank.send", replayGovEscrow(), "-5"), movement("gov/deposit_burn/bank.send", replayGovBurn(), "5")}
				category = "gov_deposit_burn"
			}
			tail := append([]rr.Record(nil), r.Records[len(r.Records)-2:]...)
			r.Records = append(append(r.Records[:len(r.Records)-2], extra...), tail...)
			out, e := LegacyReplayBlock(h, items, r)
			require.NoError(t, e)
			require.Len(t, out.Rows, 2)
			require.Equal(t, category, out.Rows[1].Category)
			require.Equal(t, ledger.PhaseEnd, out.Rows[1].Phase)
			require.Equal(t, "state", out.Rows[1].Source)
			r.Records[7].Label = "ambiguous/bank.add"
			out, e = LegacyReplayBlock(h, items, r)
			require.Error(t, e)
			require.Empty(t, out.Rows)
		})
	}
}
func TestReplayGovernanceTransfersMustPair(t *testing.T) {
	h, items, r := replayFixture(t, 0, true)
	a, _, _, _ := replayAddresses()
	x := rr.Record{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "end_block", Status: "committed", Label: "gov/deposit_refund/bank.send", Address: a, Amount: "5", Denom: "uatom", Delegator: a}
	r.Records = append(r.Records[:7], append([]rr.Record{x}, r.Records[7:]...)...)
	out, e := LegacyReplayBlock(h, items, r)
	require.Error(t, e)
	require.Empty(t, out.Rows)
}
