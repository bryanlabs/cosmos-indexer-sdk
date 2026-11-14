package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/cometbft/cometbft/crypto/tmhash"
	sdk "github.com/cosmos/cosmos-sdk/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	crisis "github.com/cosmos/cosmos-sdk/x/crisis/types"
	distr "github.com/cosmos/cosmos-sdk/x/distribution/types"
	gov "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

var replayHeld = fmt.Errorf("Hub2 replay height held: causal accounting evidence does not reconcile")

type replayKey struct{ address, denom string }
type replayDeltas map[replayKey]sdk.Int

func (d replayDeltas) add(a, denom string, n sdk.Int) {
	k := replayKey{a, denom}
	v, ok := d[k]
	if !ok {
		v = sdk.ZeroInt()
	}
	d[k] = v.Add(n)
}
func (d replayDeltas) coins(a string, c sdk.Coins, sign int) {
	for _, v := range c {
		n := v.Amount
		if sign < 0 {
			n = n.Neg()
		}
		d.add(a, v.Denom, n)
	}
}
func (d replayDeltas) equals(e replayDeltas) bool {
	for k, v := range d {
		w, ok := e[k]
		if !ok {
			w = sdk.ZeroInt()
		}
		if !v.Equal(w) {
			return false
		}
	}
	for k, v := range e {
		w, ok := d[k]
		if !ok {
			w = sdk.ZeroInt()
		}
		if !v.Equal(w) {
			return false
		}
	}
	return true
}
func replayAmount(r replayrecords.Record) (sdk.Int, error) {
	n, ok := sdk.NewIntFromString(r.Amount)
	if !ok || n.IsZero() || n.String() != r.Amount {
		return sdk.Int{}, replayHeld
	}
	return n, nil
}

// LegacyReplayBlock is Hub2 only. Every signed transaction, ante scope,
// executed message and keeper movement must reconcile before any row is usable.
// Aborted operations are retained verbatim as private block evidence, never rows.
func LegacyReplayBlock(h *capture.Height, items []LegacyInclusion, records replayrecords.Height) (ledger.Block, error) {
	if h == nil || h.Block == nil || h.Block.Block == nil || h.Results == nil {
		return ledger.Block{}, replayHeld
	}
	b := h.Block.Block
	if b.ChainID != "cosmoshub-2" || records.Number != b.Height || len(items) != len(b.Txs) || len(h.Results.TxsResults) != len(items) {
		return ledger.Block{}, replayHeld
	}
	out := ledger.Block{Chain: b.ChainID, Height: b.Height, Time: b.Time, Hash: h.Hash, Proposer: h.Proposer}
	txRecords := make([][]replayrecords.Record, len(items))
	begin, end, completed := 0, 0, 0
	govNet := replayDeltas{}
	for _, r := range records.Records {
		if r.Height != b.Height {
			return ledger.Block{}, replayHeld
		}
		if r.Kind == "movement" && !(r.Phase == "ante" && r.Label == "ante_fee" && r.Amount == "0") {
			if _, err := replayAmount(r); err != nil {
				return ledger.Block{}, err
			}
		}
		if r.Tx >= 0 {
			if r.Tx >= len(items) {
				return ledger.Block{}, replayHeld
			}
			txRecords[r.Tx] = append(txRecords[r.Tx], r)
			continue
		}
		if r.Tx != -1 || r.Message != -1 || r.Status != "committed" {
			return ledger.Block{}, replayHeld
		}
		switch r.Kind {
		case "height_completed":
			if r.Phase != "block" || r.Code != 0 {
				return ledger.Block{}, replayHeld
			}
			completed++
		case "accounting":
			if r.Code != 0 {
				return ledger.Block{}, replayHeld
			}
			if r.Phase == "begin_block" {
				begin++
			} else if r.Phase == "end_block" {
				end++
			} else {
				return ledger.Block{}, replayHeld
			}
		case "movement":
			if r.Phase != "end_block" {
				return ledger.Block{}, replayHeld
			}
			n, err := replayAmount(r)
			if err != nil {
				return ledger.Block{}, err
			}
			row := ledger.Row{Phase: ledger.PhaseEnd, SubIndex: len(out.Rows), Denom: r.Denom, Amount: n.Abs().String(), Source: "state", Validator: r.Validator}
			if strings.HasPrefix(r.Label, "gov/") {
				govNet.add(r.Label+"/"+r.Delegator, r.Denom, n)
			}
			switch r.Label {
			case "staking/unbonding_complete/bank.undelegate":
				if !n.IsPositive() || r.Delegator == "" || r.Validator == "" || r.Address != r.Delegator {
					return ledger.Block{}, replayHeld
				}
				row.Category, row.To = "unbonding_complete", r.Address
			case "distribution/removed_validator_commission/bank.add":
				if !n.IsPositive() || r.Validator == "" || r.Delegator == "" || r.Delegator != replayOperator(r.Validator) {
					return ledger.Block{}, replayHeld
				}
				row.Category, row.To, row.Trigger = "commission", r.Delegator, "commission"
				row.Detail = replayPaidTo(r.Address, "validator_removed")
			case "gov/deposit_refund/bank.send":
				if r.Delegator == "" {
					return ledger.Block{}, replayHeld
				}
				if r.Address == replayGovEscrow() {
					if !n.IsNegative() {
						return ledger.Block{}, replayHeld
					}
					continue
				}
				if !n.IsPositive() || r.Address != r.Delegator {
					return ledger.Block{}, replayHeld
				}
				row.Category, row.To = "gov_deposit_refund", r.Address
			case "gov/deposit_burn/bank.send":
				if r.Delegator == "" {
					return ledger.Block{}, replayHeld
				}
				if r.Address == replayGovBurn() {
					if !n.IsPositive() {
						return ledger.Block{}, replayHeld
					}
					continue
				}
				if r.Address != replayGovEscrow() || !n.IsNegative() {
					return ledger.Block{}, replayHeld
				}
				row.Category, row.From = "gov_deposit_burn", r.Delegator
			default:
				return ledger.Block{}, replayHeld
			}
			out.Rows = append(out.Rows, row)
		default:
			return ledger.Block{}, replayHeld
		}
	}
	if begin != 1 || end != 1 || completed != 1 || !govNet.equals(replayDeltas{}) {
		return ledger.Block{}, replayHeld
	}
	for i, item := range items {
		if item.Tx == nil || item.Result == nil || len(item.Tx.Issues) != 0 || !item.Tx.SignersComplete || len(item.Tx.Signers) == 0 || !bytes.Equal(item.Tx.Raw, b.Txs[i]) || item.Result.Code != h.Results.TxsResults[i].Code || item.Hash != fmt.Sprintf("%X", b.Txs[i].Hash()) {
			return ledger.Block{}, replayHeld
		}
		tx := item.Tx
		inc := ledger.Inclusion{Index: i, Hash: item.Hash, Code: item.Result.Code, Codespace: item.Result.Codespace, CodeEvidence: item.CodeEvidence, GasWanted: item.Result.GasWanted, GasUsed: item.Result.GasUsed, Fee: tx.Fee.String(), FeePayer: tx.Signers[0].String(), Memo: ledger.Text(tx.Memo), Raw: bytes.Clone(b.Txs[i]), FeeEvidence: "pinned replay actual ante reconciled to signed fee"}
		for _, s := range tx.Signers {
			inc.Signers = append(inc.Signers, s.String())
		}
		ante, transaction := 0, 0
		anteCommitted := false
		actualFee := replayDeltas{}
		// Zero deltas disappear in reconciliation, so authenticate their signed
		// denomination and multiplicity separately, including aborted ante evidence.
		expectedZeroFees := map[string]int{}
		for _, coin := range tx.Fee {
			if coin.Amount.IsZero() {
				expectedZeroFees[coin.Denom]++
			}
		}
		zeroFees := map[string]int{}
		var zeroFeeStatuses []string
		anteNotInvoked := false
		messages := make([][]replayrecords.Record, len(tx.Messages))
		markers := make([]int, len(messages))
		nextMessage := 0
		startedMessage := false
		for _, r := range txRecords[i] {
			if transaction != 0 {
				return ledger.Block{}, replayHeld
			}
			switch r.Phase {
			case "transaction":
				if r.Kind != "accounting" || r.Status != "committed" || r.Code != item.Result.Code || r.Message != -1 {
					return ledger.Block{}, replayHeld
				}
				transaction++
			case "ante":
				if startedMessage || r.Message != -1 {
					return ledger.Block{}, replayHeld
				}
				if r.Kind == "accounting" {
					ante++
					anteCommitted = r.Status == "committed"
					anteNotInvoked = r.Label == "ante_not_invoked"
					if anteNotInvoked && r.Status != "aborted" {
						return ledger.Block{}, replayHeld
					}
					if (anteCommitted && r.Code != 0) || (!anteCommitted && r.Status != "aborted") {
						return ledger.Block{}, replayHeld
					}
				} else if r.Kind == "movement" {
					if r.Label != "ante_fee" || r.Address != inc.FeePayer {
						return ledger.Block{}, replayHeld
					}
					n, e := replayAmount(r)
					if r.Amount == "0" {
						n, e = sdk.ZeroInt(), nil
						zeroFees[r.Denom]++
						zeroFeeStatuses = append(zeroFeeStatuses, r.Status)
					}
					if e != nil || n.IsPositive() || r.Code != 0 {
						return ledger.Block{}, replayHeld
					}
					if r.Status == "committed" {
						actualFee.add(r.Address, r.Denom, n)
					} else if r.Status != "aborted" {
						return ledger.Block{}, replayHeld
					}
				} else {
					return ledger.Block{}, replayHeld
				}
			case "message":
				startedMessage = true
				if r.Message < 0 || r.Message >= len(messages) || (r.Status != "committed" && r.Status != "aborted") || (item.Result.Code != 0 && r.Status != "aborted") {
					return ledger.Block{}, replayHeld
				}
				if r.Kind == "accounting" {
					if r.Message != nextMessage {
						return ledger.Block{}, replayHeld
					}
					nextMessage++
					markers[r.Message]++
					if item.Result.Code == 0 && r.Code != 0 {
						return ledger.Block{}, replayHeld
					}
				}
				messages[r.Message] = append(messages[r.Message], r)
			default:
				return ledger.Block{}, replayHeld
			}
		}
		if ante != 1 || transaction != 1 || (item.Result.Code == 0 && (!anteCommitted || nextMessage != len(messages))) {
			return ledger.Block{}, replayHeld
		}
		if anteNotInvoked {
			// A failure before ante runs cannot produce any fee evidence.
			for _, r := range txRecords[i] {
				if r.Phase == "ante" && r.Kind == "movement" {
					return ledger.Block{}, replayHeld
				}
			}
		} else {
			if len(zeroFees) != len(expectedZeroFees) {
				return ledger.Block{}, replayHeld
			}
			for denom, count := range expectedZeroFees {
				if zeroFees[denom] != count {
					return ledger.Block{}, replayHeld
				}
			}
		}
		for _, status := range zeroFeeStatuses {
			if (status == "committed") != anteCommitted {
				return ledger.Block{}, replayHeld
			}
		}
		expected := replayDeltas{}
		if anteCommitted {
			expected.coins(inc.FeePayer, tx.Fee, -1)
		}
		if !actualFee.equals(expected) {
			return ledger.Block{}, replayHeld
		}
		inc.FeeCharged = anteCommitted
		if !anteCommitted && nextMessage != 0 {
			return ledger.Block{}, replayHeld
		}
		for j, m := range tx.Messages {
			if m.Derived == nil || m.DerivedTypeURL == "" {
				return ledger.Block{}, replayHeld
			}
			if item.Result.Code != 0 {
				for _, r := range messages[j] {
					if r.Kind == "accounting" && r.Label != replayMessageLabel(m.AminoName) {
						return ledger.Block{}, replayHeld
					}
				}
				continue
			}
			if markers[j] != 1 {
				return ledger.Block{}, replayHeld
			}
			rows, err := replayMessage(m, messages[j])
			if err != nil {
				return ledger.Block{}, err
			}
			for sub, row := range rows {
				row.Phase, row.TxIndex, row.MsgIndex, row.SubIndex = ledger.PhaseTx, i, j, sub
				row.TxHash = item.Hash
				out.Rows = append(out.Rows, row)
			}
		}
		out.Inclusions = append(out.Inclusions, inc)
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.TxIndex != b.TxIndex {
			return a.TxIndex < b.TxIndex
		}
		if a.MsgIndex != b.MsgIndex {
			return a.MsgIndex < b.MsgIndex
		}
		return a.SubIndex < b.SubIndex
	})
	// Preserve all records, including aborted attempts, without printing them.
	raw, err := json.Marshal(records.Records)
	if err != nil {
		return ledger.Block{}, replayHeld
	}
	attributes := [][2]string{{"json", string(raw)}}
	for _, evidence := range []struct {
		name  string
		value any
	}{{"block_evidence", h.Evidence}, {"cached_begin_events", h.Results.BeginBlockEvents}, {"cached_end_events", h.Results.EndBlockEvents}} {
		retained, err := json.Marshal(evidence.value)
		if err != nil {
			return ledger.Block{}, replayHeld
		}
		attributes = append(attributes, [2]string{evidence.name, string(retained)})
	}
	if h.Results.Legacy != nil {
		attributes = append(attributes, [2]string{"cached_result_envelope", string(h.Results.Legacy.RawJSON)})
	}
	out.Events = []ledger.Event{{Phase: ledger.PhaseBegin, Index: 0, Type: "pinned_replay_records", Attributes: attributes}}
	return out, nil
}
func replayGovEscrow() string {
	return sdk.AccAddress(tmhash.SumTruncated([]byte("govDepositedCoins"))).String()
}
func replayGovBurn() string {
	return sdk.AccAddress(tmhash.SumTruncated([]byte("govBurnedDepositCoins"))).String()
}
func replayPaidTo(a, cause string) string {
	b, _ := json.Marshal(struct {
		PaidTo string `json:"paid_to"`
		Cause  string `json:"cause,omitempty"`
	}{a, cause})
	return string(b)
}

func replayMessage(m legacy.Message, rs []replayrecords.Record) ([]ledger.Row, error) {
	expected, actual := replayDeltas{}, replayDeltas{}
	var rows []ledger.Row
	add := func(cat, from, to, val string, coins sdk.Coins) {
		for _, c := range coins {
			if c.IsPositive() {
				rows = append(rows, ledger.Row{Category: cat, From: from, To: to, Validator: val, Amount: c.Amount.String(), Denom: c.Denom, Source: "message"})
			}
		}
	}
	// All message types registered by pinned Gaia have an explicit branch.
	principalLabel, principalOwner, principalVal := "", "", ""
	bankLabel := ""
	switch v := m.Derived.(type) {
	case *bank.MsgSend:
		expected.coins(v.FromAddress, v.Amount, -1)
		expected.coins(v.ToAddress, v.Amount, 1)
		bankLabel = "bank/send/bank.send"
		add("transfer", v.FromAddress, v.ToAddress, "", v.Amount)
	case *bank.MsgMultiSend:
		bankLabel = "bank/multisend/bank.input_output"
		for _, in := range v.Inputs {
			expected.coins(in.Address, in.Coins, -1)
			add("transfer", in.Address, "", "", in.Coins)
		}
		for _, out := range v.Outputs {
			expected.coins(out.Address, out.Coins, 1)
			add("transfer", "", out.Address, "", out.Coins)
		}
	case *staking.MsgDelegate:
		bankLabel = "staking/delegate/bank.delegate"
		expected.coins(v.DelegatorAddress, sdk.NewCoins(v.Amount), -1)
		add("delegate", v.DelegatorAddress, "", v.ValidatorAddress, sdk.NewCoins(v.Amount))
	case *staking.MsgCreateValidator:
		bankLabel = "staking/create_validator/bank.delegate"
		expected.coins(v.DelegatorAddress, sdk.NewCoins(v.Value), -1)
		add("delegate", v.DelegatorAddress, "", v.ValidatorAddress, sdk.NewCoins(v.Value))
	case *staking.MsgUndelegate:
		principalLabel, principalOwner, principalVal = "undelegate", v.DelegatorAddress, v.ValidatorAddress
	case *staking.MsgBeginRedelegate:
		principalLabel, principalOwner, principalVal = "redelegate", v.DelegatorAddress, v.ValidatorSrcAddress
	case *gov.MsgDeposit:
		bankLabel = "gov/gov/deposit/bank.send"
		expected.coins(v.Depositor, v.Amount, -1)
		expected.coins(replayGovEscrow(), v.Amount, 1)
		add("gov_deposit", v.Depositor, "", "", v.Amount)
	case *gov.MsgSubmitProposal:
		bankLabel = "gov/gov/submit_proposal/bank.send"
		expected.coins(v.Proposer, v.InitialDeposit, -1)
		expected.coins(replayGovEscrow(), v.InitialDeposit, 1)
		add("gov_deposit", v.Proposer, "", "", v.InitialDeposit)
	case *distr.MsgWithdrawDelegatorReward, *distr.MsgWithdrawValidatorCommission, *distr.MsgSetWithdrawAddress, *staking.MsgEditValidator, *slashing.MsgUnjail, *gov.MsgVote:
	case *crisis.MsgVerifyInvariant:
		bankLabel = "crisis/crisis/verify_invariant/bank.subtract"
	default:
		return nil, replayHeld
	}
	principalCount := 0
	for _, r := range rs {
		if r.Status != "committed" {
			return nil, replayHeld
		}
		if r.Kind == "accounting" {
			if r.Label != replayMessageLabel(m.AminoName) || r.Code != 0 {
				return nil, replayHeld
			}
			continue
		}
		n, ok := sdk.NewIntFromString(r.Amount)
		if !ok || n.String() != r.Amount || (n.IsZero() && r.Kind != "principal") {
			return nil, replayHeld
		}
		if r.Kind == "principal" {
			if principalLabel == "" || r.Label != principalLabel || r.Validator != principalVal || r.Delegator != principalOwner || r.Address != principalOwner || n.IsNegative() || r.Denom != "uatom" {
				return nil, replayHeld
			}
			principalCount++
			row := ledger.Row{Category: principalLabel, To: principalOwner, Validator: principalVal, Amount: n.String(), Denom: r.Denom, Source: "state"}
			if v, ok := m.Derived.(*staking.MsgBeginRedelegate); ok {
				row.Validator = v.ValidatorDstAddress
				row.From = principalOwner
				d, _ := json.Marshal(map[string]string{"source_validator": v.ValidatorSrcAddress, "destination_validator": v.ValidatorDstAddress})
				row.Detail = string(d)
			}
			if n.IsPositive() {
				rows = append(rows, row)
			}
			continue
		}
		if r.Kind != "movement" {
			return nil, replayHeld
		}
		switch r.Label {
		case "distribution/delegation_reward/bank.add", "distribution/validator_commission/bank.add", "distribution/removed_validator_commission/bank.add":
			if !n.IsPositive() || r.Delegator == "" || r.Validator == "" || !replayRewardCause(m.Derived, r) {
				return nil, replayHeld
			}
			category, trigger, cause := "reward", "withdraw", ""
			switch m.Derived.(type) {
			case *staking.MsgDelegate, *staking.MsgCreateValidator:
				trigger = "delegate"
			case *staking.MsgUndelegate:
				trigger = "undelegate"
			case *staking.MsgBeginRedelegate:
				trigger = "redelegate"
			}
			if strings.Contains(r.Label, "commission") {
				category, trigger = "commission", "commission"
			}
			if strings.Contains(r.Label, "removed_validator") {
				cause = "validator_removed"
			}
			rows = append(rows, ledger.Row{Category: category, Amount: n.String(), Denom: r.Denom, To: r.Delegator, Validator: r.Validator, Trigger: trigger, Detail: replayPaidTo(r.Address, cause), Source: "state"})
		case "staking/undelegate/bank.undelegate":
			if principalLabel != "undelegate" || r.Address != principalOwner || r.Validator != principalVal || !n.IsPositive() {
				return nil, replayHeld
			}
			rows = append(rows, ledger.Row{Category: "unbonding_complete", To: r.Address, Validator: r.Validator, Amount: n.String(), Denom: r.Denom, Source: "state"})
		default:
			if r.Label != bankLabel || bankLabel == "" {
				return nil, replayHeld
			}
			if _, ok := m.Derived.(*crisis.MsgVerifyInvariant); ok {
				v := m.Derived.(*crisis.MsgVerifyInvariant)
				if r.Address != v.Sender || !n.IsNegative() {
					return nil, replayHeld
				}
				rows = append(rows, ledger.Row{Category: "expense", From: r.Address, Amount: n.Abs().String(), Denom: r.Denom, Trigger: "verify_invariant", Source: "state"})
			} else {
				actual.add(r.Address, r.Denom, n)
			}
		}
	}
	if !actual.equals(expected) || principalCount > 1 || (principalLabel != "" && principalCount != 1) {
		return nil, replayHeld
	}
	// Zero-token undelegations/redelegations legitimately have no principal row.
	return rows, nil
}
func replayRewardCause(msg sdk.Msg, r replayrecords.Record) bool {
	commission := strings.Contains(r.Label, "commission")
	removed := strings.Contains(r.Label, "removed_validator")
	switch m := msg.(type) {
	case *distr.MsgWithdrawDelegatorReward:
		return !commission && r.Delegator == m.DelegatorAddress && r.Validator == m.ValidatorAddress
	case *distr.MsgWithdrawValidatorCommission:
		return commission && !removed && r.Validator == m.ValidatorAddress && replayOperator(r.Validator) == r.Delegator
	case *staking.MsgDelegate:
		return !commission && r.Delegator == m.DelegatorAddress && r.Validator == m.ValidatorAddress
	case *staking.MsgCreateValidator:
		return !commission && r.Delegator == m.DelegatorAddress && r.Validator == m.ValidatorAddress
	case *staking.MsgUndelegate:
		return r.Validator == m.ValidatorAddress && ((!commission && r.Delegator == m.DelegatorAddress) || (removed && r.Delegator == replayOperator(r.Validator)))
	case *staking.MsgBeginRedelegate:
		return (!commission && r.Delegator == m.DelegatorAddress && (r.Validator == m.ValidatorSrcAddress || r.Validator == m.ValidatorDstAddress)) || (removed && r.Validator == m.ValidatorSrcAddress && r.Delegator == replayOperator(r.Validator))
	}
	return false
}
func replayMessageLabel(name string) string {
	return map[string]string{
		"cosmos-sdk/MsgSend": "bank/send", "cosmos-sdk/MsgMultiSend": "bank/multisend",
		"cosmos-sdk/MsgDelegate": "delegate", "cosmos-sdk/MsgCreateValidator": "create_validator",
		"cosmos-sdk/MsgUndelegate": "undelegate", "cosmos-sdk/MsgBeginRedelegate": "redelegate",
		"cosmos-sdk/MsgWithdrawDelegationReward": "delegation_reward", "cosmos-sdk/MsgWithdrawValidatorCommission": "validator_commission",
		"cosmos-sdk/MsgModifyWithdrawAddress": "distr/set_withdraw_address", "cosmos-sdk/MsgEditValidator": "staking/edit_validator",
		"cosmos-sdk/MsgDeposit": "gov/deposit", "cosmos-sdk/MsgSubmitProposal": "gov/submit_proposal", "cosmos-sdk/MsgVote": "gov/vote",
		"cosmos-sdk/MsgUnjail": "slashing/unjail", "cosmos-sdk/MsgVerifyInvariant": "crisis/verify_invariant",
	}[name]
}

func replayOperator(v string) string {
	a, e := sdk.ValAddressFromBech32(v)
	if e != nil {
		return ""
	}
	return sdk.AccAddress(a).String()
}

// LegacyReplayExport only consumes actual completed zero-height export records.
// The block metadata is the independently replay-verified source height.
func LegacyReplayExport(b ledger.Block, h replayrecords.Height) (ledger.Block, error) {
	if b.Chain != "cosmoshub-2" || b.Height != h.Number {
		return ledger.Block{}, replayHeld
	}
	out := b
	out.Rows = append([]ledger.Row(nil), b.Rows...)
	exportSub := 0
	complete, accounting := 0, 0
	for _, r := range h.Records {
		if r.Height != b.Height || r.Status != "exported" || r.Phase != "zero_height_export" || r.Tx != -1 || r.Message != -1 || r.Code != 0 {
			return ledger.Block{}, replayHeld
		}
		switch r.Kind {
		case "accounting":
			accounting++
		case "export_completed":
			complete++
		case "movement":
			n, e := replayAmount(r)
			if e != nil || !n.IsPositive() || r.Validator == "" {
				return ledger.Block{}, replayHeld
			}
			category := ""
			switch r.Label {
			case "validator_commission/bank.add":
				category = "commission"
				if r.Delegator != "" {
					return ledger.Block{}, replayHeld
				}
				r.Delegator = replayOperator(r.Validator)
				if r.Delegator == "" {
					return ledger.Block{}, replayHeld
				}
			case "delegator_reward/bank.add":
				category = "reward"
				if r.Delegator == "" {
					return ledger.Block{}, replayHeld
				}
			default:
				return ledger.Block{}, replayHeld
			}
			out.Rows = append(out.Rows, ledger.Row{Phase: ledger.PhaseExport, SubIndex: exportSub, Category: category, Amount: n.String(), Denom: r.Denom, To: r.Delegator, Validator: r.Validator, Trigger: "zero_height_export", Detail: replayPaidTo(r.Address, ""), Source: "export_payout"})
			exportSub++
		default:
			return ledger.Block{}, replayHeld
		}
	}
	if complete != 1 || accounting < 1 {
		return ledger.Block{}, replayHeld
	}
	raw, err := json.Marshal(h.Records)
	if err != nil {
		return ledger.Block{}, replayHeld
	}
	out.Events = append(append([]ledger.Event(nil), b.Events...), ledger.Event{Phase: ledger.PhaseBegin, Index: 1, Type: "pinned_export_records", Attributes: [][2]string{{"json", string(raw)}}})
	return out, nil
}
