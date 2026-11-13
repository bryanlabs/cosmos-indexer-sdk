package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	crisis "github.com/cosmos/cosmos-sdk/x/crisis/types"
	distr "github.com/cosmos/cosmos-sdk/x/distribution/types"
	gov "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	slash "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stake "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

func replayAddresses() (string, string, string, string) {
	a := sdk.AccAddress(bytes.Repeat([]byte{1}, 20))
	b := sdk.AccAddress(bytes.Repeat([]byte{2}, 20))
	return a.String(), b.String(), sdk.ValAddress(a).String(), sdk.ValAddress(b).String()
}
func replayFixture(t *testing.T, code uint32, ante bool) (*capture.Height, []LegacyInclusion, rr.Height) {
	a, b, _, _ := replayAddresses()
	raw := intakeRaw(t, "cosmoshub-2")
	tx, e := legacy.Decode("cosmoshub-2", raw)
	require.NoError(t, e)
	block := &tm.Block{Header: tm.Header{ChainID: "cosmoshub-2", Height: 9, Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, Data: tm.Data{Txs: tm.Txs{raw}}}
	result := &abci.ResponseDeliverTx{Code: code}
	h := &capture.Height{Block: &coretypes.ResultBlock{Block: block}, Results: &rpc.CustomBlockResults{TxsResults: []*abci.ResponseDeliverTx{result}}, Hash: strings.Repeat("AB", 32), Proposer: "synthetic"}
	items := []LegacyInclusion{{Tx: tx, Result: result, Hash: strings.ToUpper(fmtHash(raw)), CodeEvidence: "explicit"}}
	r := rr.Height{Number: 9, Records: []rr.Record{{Kind: "accounting", Height: 9, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"}}}
	status := "committed"
	if !ante {
		status = "aborted"
	}
	r.Records = append(r.Records, rr.Record{Kind: "movement", Height: 9, Tx: 0, Message: -1, Phase: "ante", Label: "ante_fee", Status: status, Address: a, Denom: "uatom", Amount: "-7"}, rr.Record{Kind: "accounting", Height: 9, Tx: 0, Message: -1, Phase: "ante", Status: status})
	if ante {
		status = "committed"
		if code != 0 {
			status = "aborted"
		}
		r.Records = append(r.Records, rr.Record{Kind: "movement", Height: 9, Tx: 0, Message: 0, Phase: "message", Label: "bank/send/bank.send", Status: status, Address: a, Denom: "uatom", Amount: "-53"}, rr.Record{Kind: "movement", Height: 9, Tx: 0, Message: 0, Phase: "message", Label: "bank/send/bank.send", Status: status, Address: b, Denom: "uatom", Amount: "53"}, rr.Record{Kind: "accounting", Height: 9, Tx: 0, Message: 0, Phase: "message", Label: "bank/send", Status: status})
	}
	r.Records = append(r.Records, rr.Record{Kind: "accounting", Height: 9, Tx: 0, Message: -1, Phase: "transaction", Code: code, Status: "committed"}, rr.Record{Kind: "accounting", Height: 9, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"}, rr.Record{Kind: "height_completed", Height: 9, Tx: -1, Message: -1, Phase: "block", Status: "committed"})
	return h, items, r
}
func fmtHash(tx tm.Tx) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, v := range tx.Hash() {
		b.WriteByte(hex[v>>4])
		b.WriteByte(hex[v&15])
	}
	return b.String()
}
func TestReplayBlockAnteExecutionAndAbortedEvidence(t *testing.T) {
	for _, tc := range []struct {
		code uint32
		ante bool
		rows int
	}{{0, true, 1}, {5, true, 0}, {5, false, 0}} {
		h, items, r := replayFixture(t, tc.code, tc.ante)
		b, e := LegacyReplayBlock(h, items, r)
		require.NoError(t, e)
		require.Len(t, b.Rows, tc.rows)
		require.Equal(t, tc.ante, b.Inclusions[0].FeeCharged)
		require.Equal(t, "7uatom", b.Inclusions[0].Fee)
		var kept []rr.Record
		require.NoError(t, json.Unmarshal([]byte(b.Events[0].Attributes[0][1]), &kept))
		require.Equal(t, r.Records, kept)
	}
}
func TestReplayBlockRejectsContradictions(t *testing.T) {
	for _, name := range []string{"fee", "code", "message position", "message label", "missing movement", "missing ante", "extra transaction", "hub3", "committed failed message", "missing close"} {
		t.Run(name, func(t *testing.T) {
			h, items, r := replayFixture(t, 0, true)
			switch name {
			case "fee":
				r.Records[1].Amount = "-6"
			case "code":
				r.Records[6].Code = 7
			case "message position":
				r.Records[3].Message = 1
			case "message label":
				r.Records[5].Label = "bank/multisend"
			case "missing movement":
				r.Records = append(r.Records[:4], r.Records[5:]...)
			case "missing ante":
				r.Records = append(r.Records[:2], r.Records[3:]...)
			case "extra transaction":
				r.Records[1].Tx = 1
			case "hub3":
				h.Block.Block.ChainID = "cosmoshub-3"
			case "committed failed message":
				items[0].Result.Code = 5
				r.Records[6].Code = 5
			case "missing close":
				r.Records = r.Records[:len(r.Records)-1]
			}
			b, e := LegacyReplayBlock(h, items, r)
			require.Error(t, e)
			require.Empty(t, b.Rows)
		})
	}
}
func TestReplayAllPinnedMessageFamilies(t *testing.T) {
	a, b, v, w := replayAddresses()
	coins := sdk.NewCoins(sdk.NewInt64Coin("uatom", 11))
	movement := func(label, address, amount string) rr.Record {
		return rr.Record{Kind: "movement", Phase: "message", Status: "committed", Label: label, Address: address, Denom: "uatom", Amount: amount}
	}
	reward := movement("distribution/delegation_reward/bank.add", b, "3")
	reward.Validator, reward.Delegator = v, a
	principal := rr.Record{Kind: "principal", Phase: "message", Status: "committed", Label: "undelegate", Address: a, Delegator: a, Validator: v, Denom: "uatom", Amount: "10"}
	tests := []struct {
		name       string
		msg        sdk.Msg
		rs         []rr.Record
		categories []string
	}{
		{"MsgSend", &bank.MsgSend{FromAddress: a, ToAddress: b, Amount: coins}, []rr.Record{movement("bank/send/bank.send", a, "-11"), movement("bank/send/bank.send", b, "11")}, []string{"transfer"}},
		{"MsgMultiSend", &bank.MsgMultiSend{Inputs: []bank.Input{{Address: a, Coins: coins}}, Outputs: []bank.Output{{Address: b, Coins: coins}}}, []rr.Record{movement("bank/multisend/bank.input_output", a, "-11"), movement("bank/multisend/bank.input_output", b, "11")}, []string{"transfer", "transfer"}},
		{"MsgDelegate", &stake.MsgDelegate{DelegatorAddress: a, ValidatorAddress: v, Amount: coins[0]}, []rr.Record{reward, movement("staking/delegate/bank.delegate", a, "-11")}, []string{"delegate", "reward"}},
		{"MsgCreateValidator", &stake.MsgCreateValidator{DelegatorAddress: a, ValidatorAddress: v, Value: coins[0]}, []rr.Record{movement("staking/create_validator/bank.delegate", a, "-11")}, []string{"delegate"}},
		{"MsgUndelegate", &stake.MsgUndelegate{DelegatorAddress: a, ValidatorAddress: v, Amount: coins[0]}, []rr.Record{reward, principal}, []string{"reward", "undelegate"}},
		{"MsgBeginRedelegate", &stake.MsgBeginRedelegate{DelegatorAddress: a, ValidatorSrcAddress: v, ValidatorDstAddress: w, Amount: coins[0]}, nil, nil},
		{"MsgWithdrawDelegationReward", &distr.MsgWithdrawDelegatorReward{DelegatorAddress: a, ValidatorAddress: v}, []rr.Record{reward}, []string{"reward"}},
		{"MsgWithdrawValidatorCommission", &distr.MsgWithdrawValidatorCommission{ValidatorAddress: v}, nil, nil},
		{"MsgDeposit", &gov.MsgDeposit{Depositor: a, Amount: coins}, []rr.Record{movement("gov/gov/deposit/bank.send", a, "-11"), movement("gov/gov/deposit/bank.send", replayGovEscrow(), "11")}, []string{"gov_deposit"}},
		{"MsgSubmitProposal", &gov.MsgSubmitProposal{Proposer: a, InitialDeposit: coins}, []rr.Record{movement("gov/gov/submit_proposal/bank.send", a, "-11"), movement("gov/gov/submit_proposal/bank.send", replayGovEscrow(), "11")}, []string{"gov_deposit"}},
		{"MsgModifyWithdrawAddress", &distr.MsgSetWithdrawAddress{}, nil, nil}, {"MsgEditValidator", &stake.MsgEditValidator{}, nil, nil}, {"MsgUnjail", &slash.MsgUnjail{}, nil, nil}, {"MsgVote", &gov.MsgVote{}, nil, nil},
		{"MsgVerifyInvariant", &crisis.MsgVerifyInvariant{Sender: a}, []rr.Record{movement("crisis/crisis/verify_invariant/bank.subtract", a, "-11")}, []string{"expense"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "MsgBeginRedelegate" {
				p := principal
				p.Label = "redelegate"
				tc.rs = []rr.Record{p}
				tc.categories = []string{"redelegate"}
			}
			if tc.name == "MsgWithdrawValidatorCommission" {
				r := reward
				r.Label = "distribution/validator_commission/bank.add"
				tc.rs = []rr.Record{r}
				tc.categories = []string{"commission"}
			}
			tc.rs = append(tc.rs, rr.Record{Kind: "accounting", Status: "committed", Label: replayMessageLabel("cosmos-sdk/" + tc.name)})
			m := legacy.Message{AminoName: "cosmos-sdk/" + tc.name, Derived: tc.msg}
			rows, e := replayMessage(m, tc.rs)
			require.NoError(t, e)
			require.Len(t, rows, len(tc.categories))
			for i, c := range tc.categories {
				require.Equal(t, c, rows[i].Category)
			}
			if tc.name == "MsgUndelegate" {
				require.Equal(t, "10", rows[1].Amount)
				require.Equal(t, "state", rows[1].Source)
			}
			bad := append([]rr.Record(nil), tc.rs...)
			bad = append(bad, movement("unscoped/bank.add", b, "1"))
			_, e = replayMessage(m, bad)
			require.Error(t, e)
		})
	}
}
func TestReplayRewardCausalityAndPrincipalRequired(t *testing.T) {
	a, b, v, w := replayAddresses()
	msg := legacy.Message{AminoName: "cosmos-sdk/MsgBeginRedelegate", Derived: &stake.MsgBeginRedelegate{DelegatorAddress: a, ValidatorSrcAddress: v, ValidatorDstAddress: w}}
	rs := []rr.Record{{Kind: "principal", Status: "committed", Label: "redelegate", Address: a, Delegator: a, Validator: v, Denom: "uatom", Amount: "0"}, {Kind: "movement", Status: "committed", Label: "distribution/delegation_reward/bank.add", Address: b, Delegator: a, Validator: v, Denom: "uatom", Amount: "2"}, {Kind: "movement", Status: "committed", Label: "distribution/delegation_reward/bank.add", Address: b, Delegator: a, Validator: w, Denom: "uatom", Amount: "3"}}
	rows, e := replayMessage(msg, rs)
	require.NoError(t, e)
	require.Len(t, rows, 2)
	require.NotEqual(t, rows[0].Validator, rows[1].Validator)
	_, e = replayMessage(msg, rs[1:])
	require.Error(t, e)
	rs[2].Delegator = b
	_, e = replayMessage(msg, rs)
	require.Error(t, e)
}
func TestReplayActualExportOnly(t *testing.T) {
	a, b, v, _ := replayAddresses()
	base := ledger.Block{Chain: "cosmoshub-2", Height: 9}
	r := rr.Height{Number: 9, Records: []rr.Record{{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported", Label: "validator_commission/bank.add", Address: b, Validator: v, Denom: "uatom", Amount: "3"}, {Kind: "accounting", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}, {Kind: "export_completed", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}}}
	out, e := LegacyReplayExport(base, r)
	require.NoError(t, e)
	require.Len(t, out.Rows, 1)
	require.Equal(t, a, out.Rows[0].To)
	require.Equal(t, "export_payout", out.Rows[0].Source)
	require.Equal(t, ledger.PhaseExport, out.Rows[0].Phase)
	for _, label := range []string{"residual", "distribution/validator_commission/bank.add"} {
		r.Records[0].Label = label
		_, e = LegacyReplayExport(base, r)
		require.Error(t, e)
	}
	r.Records = r.Records[1:2]
	_, e = LegacyReplayExport(base, r)
	require.Error(t, e)
}
