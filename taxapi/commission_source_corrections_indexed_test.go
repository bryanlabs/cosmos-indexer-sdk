package taxapi

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// The two re-indexed hub4 messages: one wallet paid by four share records of one
// validator (one indexed row of 2932922), another by two records of two
// validators (two indexed rows, 607912 and 586313).
var correctionIndexedAmounts = []string{"398877", "1161437", "997194", "375414", "607912", "586313"}

func correctionIndexedManifest(t *testing.T) *CommissionSourceCorrections {
	t.Helper()
	p := correctionTestManifest(t)
	for i := range p.Rewards {
		p.Rewards[i].Amount = correctionIndexedAmounts[i]
	}
	p.rewardsByWallet = map[string][]int{}
	p.feesByWallet = map[string][]int{}
	require.NoError(t, p.validate())
	return p
}

func expectCorrectionReward(m sqlmock.Sqlmock, c CommissionNativeRewardCorrection, message uint) {
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(message, c.MessageType))
}

func correctionIndexedRow(c CommissionNativeRewardCorrection, message uint, sub int, amount, validator string) tax.TaxableEvent {
	return tax.TaxableEvent{TxHash: c.TxHash, MessageID: message, SubIndex: sub, Category: apiReward, Denom: apiUatom, Amount: amount, ToAddr: c.Recipient, ValidatorAddress: validator, BlockHeight: c.Height, Timestamp: c.Timestamp.Truncate(time.Microsecond)}
}

func TestCommissionSourceCorrectionIndexedRewardUsedOnce(t *testing.T) {
	p := correctionIndexedManifest(t)
	a, b := p.Rewards[0], p.Rewards[4]
	for _, tc := range []struct {
		wallet  string
		message uint
		effects []CommissionNativeRewardCorrection
		rows    []tax.TaxableEvent
	}{
		{"reward-a", 71, p.Rewards[:4], []tax.TaxableEvent{correctionIndexedRow(a, 71, 0, "2932922", "validator-985c")}},
		{"reward-b", 72, p.Rewards[4:], []tax.TaxableEvent{correctionIndexedRow(b, 72, 0, "607912", "validator-0013"), correctionIndexedRow(b, 72, 1, "586313", "validator-5290")}},
	} {
		t.Run(tc.wallet, func(t *testing.T) {
			s, m := correctionTestSQL(t, p)
			for _, c := range tc.effects {
				expectCorrectionReward(m, c, tc.message)
			}
			rows := walletRows{Events: tc.rows}
			c := tc.effects[0]
			out, e := s.applyCommissionSourceCorrections(apiMainnet, tc.wallet, c.Timestamp, c.Timestamp.Add(time.Second), rows)
			require.NoError(t, e)
			require.Equal(t, rows.Events, out.Events, "the indexed rows are used as stored")
			require.Empty(t, out.CommissionCorrections, "nothing is added on top of the indexed reward")
			receipts := s.sourceCorrectionReceipts.receipts()
			require.Len(t, receipts, 1)
			require.Equal(t, "bounded_native_effects_consumed", receipts[0].Status)
			require.Len(t, receipts[0].Effects, len(tc.effects))
			for _, effect := range receipts[0].Effects {
				require.Equal(t, "indexed_reward_equals_correction_not_added", effect.Disposition)
				require.Equal(t, tc.message, effect.SourceMessageID)
				require.Equal(t, correctionEventsSHA(tc.rows, c.TxHash, tc.message), effect.SourceRowsSHA256)
				require.Nil(t, effect.ResolvedSourceKey)
			}
		})
	}
}

// An empty footprint still applies the corrections exactly as before.
func TestCommissionSourceCorrectionEmptyFootprintStillApplies(t *testing.T) {
	p := correctionIndexedManifest(t)
	s, m := correctionTestSQL(t, p)
	for _, c := range p.Rewards[:4] {
		expectCorrectionReward(m, c, 71)
	}
	c := p.Rewards[0]
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.NoError(t, e)
	require.Len(t, out.CommissionCorrections, 4)
	sum := decimal.Zero
	for i, event := range out.CommissionCorrections {
		require.Equal(t, correctionIndexedAmounts[i], event.Amount.String())
		require.Equal(t, flowSourceCorrectionTrigger, event.Trigger)
		sum = sum.Add(event.Amount)
	}
	require.Equal(t, "2932922", sum.String())
	for _, effect := range s.sourceCorrectionReceipts.receipts()[0].Effects {
		require.Equal(t, "native_credit_added_after_commuting_message_credits", effect.Disposition)
		require.Equal(t, correctionEmptyFootprint, effect.SourceRowsSHA256)
	}
}

// Any footprint other than the exact corrected rewards still refuses, at the
// first correction of the message, and the refusal is the read failure cause.
func TestCommissionSourceCorrectionIndexedRewardOtherFootprintRefuses(t *testing.T) {
	p := correctionIndexedManifest(t)
	a := p.Rewards[0]
	row := func(amount string) tax.TaxableEvent { return correctionIndexedRow(a, 71, 0, amount, "validator-985c") }
	for name, rows := range map[string][]tax.TaxableEvent{
		"one less":         {row("2932921")},
		"one more":         {row("2932923")},
		"split but short":  {row("2932922"), {TxHash: a.TxHash, MessageID: 71, SubIndex: 1, Category: apiReward, Denom: apiUatom, Amount: "1", ToAddr: a.Recipient, ValidatorAddress: "validator-985c", BlockHeight: a.Height, Timestamp: a.Timestamp}},
		"no validator":     {correctionIndexedRow(a, 71, 0, "2932922", "")},
		"other denom":      {row("2932922"), {TxHash: a.TxHash, MessageID: 71, SubIndex: 1, Category: apiReward, Denom: "ibc/27BC", Amount: "2", ToAddr: a.Recipient, ValidatorAddress: "validator-985c", BlockHeight: a.Height, Timestamp: a.Timestamp}},
		"outbound in msg":  {row("2932922"), {TxHash: a.TxHash, MessageID: 71, SubIndex: 1, Category: apiTransfer, Denom: apiUatom, Amount: "5", FromAddr: a.Recipient, ToAddr: "outside", BlockHeight: a.Height, Timestamp: a.Timestamp}},
		"other height":     {func() tax.TaxableEvent { e := row("2932922"); e.BlockHeight++; return e }()},
		"other time":       {func() tax.TaxableEvent { e := row("2932922"); e.Timestamp = e.Timestamp.Add(time.Second); return e }()},
		"triggered reward": {func() tax.TaxableEvent { e := row("2932922"); e.RewardTrigger = "withdraw_address"; return e }()},
	} {
		t.Run(name, func(t *testing.T) {
			s, m := correctionTestSQL(t, p)
			expectCorrectionReward(m, a, 71)
			_, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", a.Timestamp, a.Timestamp.Add(time.Second), walletRows{Events: rows})
			require.Error(t, e)
			require.Empty(t, s.sourceCorrectionReceipts.receipts())
			phases := &privateReadPhases{failure: CommissionReadFailure{Phase: "native_source_corrections", PhaseMillis: map[string]int64{}}, started: time.Now(), phaseStarted: time.Now()}
			var failure *CommissionReadFailure
			require.ErrorAs(t, phases.wrap(e), &failure)
			require.Equal(t, "correction_reward_source_footprint_changed", failure.Cause)
			require.Contains(t, failure.Error(), "cause=correction_reward_source_footprint_changed")
		})
	}
}

// Only a message the corrections were proved empty against may switch to the
// indexed rows; a correction proved against existing rows still refuses.
func TestCommissionSourceCorrectionIndexedRewardNeedsEmptyProof(t *testing.T) {
	p := correctionIndexedManifest(t)
	a := p.Rewards[0]
	proved := []tax.TaxableEvent{{TxHash: a.TxHash, MessageID: 71, SubIndex: 0, Category: apiReward, Denom: apiUatom, Amount: "9", ToAddr: a.Recipient, BlockHeight: a.Height, Timestamp: a.Timestamp}}
	for i := 0; i < 4; i++ {
		p.Rewards[i].OriginalMessageEventsSHA256 = correctionEventsSHA(proved, a.TxHash, 71)
	}
	s, m := correctionTestSQL(t, p)
	expectCorrectionReward(m, a, 71)
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", a.Timestamp, a.Timestamp.Add(time.Second), walletRows{Events: []tax.TaxableEvent{correctionIndexedRow(a, 71, 0, "2932922", "validator-985c")}})
	require.ErrorContains(t, e, "reward source footprint changed")
}

func TestCommissionSourceCorrectionRefusalNamesCheck(t *testing.T) {
	require.Equal(t, "correction_reward_source_footprint_changed", commissionCorrectionRefusal("native correction reward source footprint changed").check())
	require.Equal(t, "correction_source_bank_events_footprint_changed", commissionCorrectionRefusal("native correction source BANK/events footprint changed").check())
	require.Equal(t, "other", privateReadCause(fmt.Errorf("unsafe driver wallet text")))
	require.Equal(t, "correction_indexed_message_identity_changed", privateReadCause(fmt.Errorf("read: %w", correctionRefusal("native correction indexed message identity changed"))))
}

// The run where the corrections were added to an empty footprint and the run
// that uses the re-indexed rows trace the same coins: every fate of both rules
// is equal. The chain paid the re-indexed rewards through the share-record
// accounts, which the payout reader returns for those messages; before the
// re-index those message events did not exist either.
func TestCommissionSourceCorrectionIndexedRewardSameTracedTotals(t *testing.T) {
	p := correctionIndexedManifest(t)
	for i := range p.Rewards {
		p.Rewards[i].Recipient = "payer"
	}
	p.Windows[3].Wallet, p.Windows[4].Wallet = "payer", "payer"
	p.rewardsByWallet = map[string][]int{}
	p.feesByWallet = map[string][]int{}
	require.NoError(t, p.validate())
	base := []tax.TaxableEvent{
		{Timestamp: flowTestStart, BlockHeight: 1, MessageID: 1, Category: apiCommission, Amount: "1000000", Denom: apiUatom, ToAddr: "payer", ValidatorAddress: flowTestValidator},
		{Timestamp: flowTestStart.AddDate(0, 1, 4), BlockHeight: 100000, MessageID: 999, Category: apiTransfer, Amount: "3000000", Denom: apiUatom, FromAddr: "payer", ToAddr: flowTestExchange},
	}
	messages := []uint{71, 71, 71, 71, 72, 72}
	read := func(t *testing.T, events []tax.TaxableEvent, paid map[uint]flowRewardPayout) map[string]string {
		s, m := correctionTestSQL(t, p)
		for i, c := range p.Rewards {
			expectCorrectionReward(m, c, messages[i])
		}
		out, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", flowTestStart, flowTestStart.AddDate(0, 3, 0), walletRows{Events: events})
		require.NoError(t, err)
		l := newFakeFlowLedger()
		l.rows["payer"] = reconcileRewardPayouts("payer", commissionFlowEventsFromRows(out, "payer", apiUatom, nil), paid)
		require.NoError(t, indexedRewardCorrectionsKept("payer", l.rows["payer"], out.IndexedRewardCorrections))
		sort.SliceStable(l.rows["payer"], func(i, j int) bool { return l.rows["payer"][i].Key.before(l.rows["payer"][j].Key) })
		tr := runFlowTest(t, l, "payer")
		fates := map[string]string{}
		for k, v := range tr.Fates {
			fates[fmt.Sprintf("%+v", k)] = v.String()
		}
		return fates
	}
	for i := range p.Fees {
		p.Fees[i].Timestamp = flowTestStart.AddDate(0, 4, 0).Add(time.Duration(i) * time.Second)
	}
	p.rewardsByWallet = map[string][]int{}
	p.feesByWallet = map[string][]int{}
	require.NoError(t, p.validate())

	corrected := read(t, base, nil)

	a, b := p.Rewards[0], p.Rewards[4]
	indexed := append(append([]tax.TaxableEvent(nil), base...), correctionIndexedRow(a, 71, 0, "2932922", "validator-985c"), correctionIndexedRow(b, 72, 0, "607912", "validator-0013"), correctionIndexedRow(b, 72, 1, "586313", "validator-5290"))
	paid := map[uint]flowRewardPayout{
		71: {Key: flowKey{Time: a.Timestamp.Truncate(time.Microsecond), Height: a.Height, Message: 71, Sub: flowPayoutSub}, Amount: decimal.RequireFromString("2932922")},
		72: {Key: flowKey{Time: b.Timestamp.Truncate(time.Microsecond), Height: b.Height, Message: 72, Sub: flowPayoutSub}, Amount: decimal.RequireFromString("1194225")},
	}
	reindexed := read(t, indexed, paid)

	require.NotEmpty(t, corrected)
	require.Equal(t, corrected, reindexed)
	total := decimal.Zero
	for k, v := range corrected {
		if k[:len("{Rule:0 ")] == "{Rule:0 " {
			total = total.Add(decimal.RequireFromString(v))
		}
	}
	require.Equal(t, "1000000", total.String(), "every traced commission coin has one fate")
}

// The retained six-reward manifest: both re-indexed messages switch to the
// indexed rows whether the indexer books one row per validator or one per
// share record, and nothing is added on top.
func TestCommissionSourceCorrectionIndexedRewardRetainedManifest(t *testing.T) {
	path := os.Getenv("COMMISSION_CORRECTIONS_SIX_REWARDS")
	if path == "" {
		t.Skip("retained private manifests not configured")
	}
	raw, e := os.ReadFile(path)
	require.NoError(t, e)
	p, e := LoadCommissionSourceCorrections(path, privateSHA(raw))
	require.NoError(t, e)
	require.Len(t, p.Rewards, 6)
	totals := map[int64]string{}
	for wallet, list := range p.rewardsByWallet {
		first := p.Rewards[list[0]]
		sum := decimal.Zero
		var perRecord []tax.TaxableEvent
		for i, idx := range list {
			c := p.Rewards[idx]
			require.Equal(t, first.TxHash, c.TxHash)
			require.Equal(t, correctionEmptyFootprint, c.OriginalMessageEventsSHA256)
			sum = sum.Add(decimal.RequireFromString(c.Amount))
			perRecord = append(perRecord, correctionIndexedRow(c, 71, i, c.Amount, fmt.Sprintf("validator-%d", i)))
		}
		totals[first.Height] = sum.String()
		for name, rows := range map[string][]tax.TaxableEvent{
			"one row":        {correctionIndexedRow(first, 71, 0, sum.String(), "validator-0")},
			"row per record": perRecord,
		} {
			t.Run(fmt.Sprintf("%d/%s", first.Height, name), func(t *testing.T) {
				s, m := correctionTestSQL(t, p)
				for _, idx := range list {
					expectCorrectionReward(m, p.Rewards[idx], 71)
				}
				out, e := s.applyCommissionSourceCorrections(apiMainnet, wallet, first.Timestamp.Truncate(time.Microsecond), first.Timestamp.Add(time.Second), walletRows{Events: rows})
				require.NoError(t, e)
				require.Empty(t, out.CommissionCorrections)
				require.Equal(t, rows, out.Events)
				for _, effect := range s.sourceCorrectionReceipts.receipts()[0].Effects {
					require.Equal(t, "indexed_reward_equals_correction_not_added", effect.Disposition)
				}
			})
		}
	}
	require.Equal(t, map[int64]string{31944427: "2932922", 31944434: "1194225"}, totals)
}

// Cross-review cases: a row of the corrected message under another hash, or an
// outbound withdraw-address move in it, refuses; and a trace whose payout
// reconciliation would cut the indexed reward refuses instead of crediting
// less than the corrections did.
func TestCommissionSourceCorrectionIndexedRewardReviewCases(t *testing.T) {
	p := correctionIndexedManifest(t)
	a := p.Rewards[0]
	row := correctionIndexedRow(a, 71, 0, "2932922", "validator-985c")
	extra := row
	extra.SubIndex, extra.Category, extra.FromAddr, extra.ToAddr, extra.ValidatorAddress, extra.Amount = 1, apiTransfer, a.Recipient, "outside", "", "5"
	otherHash, move := extra, extra
	otherHash.TxHash = ""
	move.Denom, move.RewardTrigger = "ibc/27BC", tax.TriggerWithdrawAddress
	rewardOtherHash := row
	rewardOtherHash.SubIndex, rewardOtherHash.TxHash = 1, ""
	for name, rows := range map[string][]tax.TaxableEvent{
		"outbound under another hash": {row, otherHash},
		"reward under another hash":   {row, rewardOtherHash},
		"withdraw-address move":       {row, move},
	} {
		t.Run(name, func(t *testing.T) {
			s, m := correctionTestSQL(t, p)
			expectCorrectionReward(m, a, 71)
			_, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", a.Timestamp, a.Timestamp.Add(time.Second), walletRows{Events: rows})
			require.ErrorContains(t, e, "reward source footprint changed")
		})
	}

	s, m := correctionTestSQL(t, p)
	for _, c := range p.Rewards[:4] {
		expectCorrectionReward(m, c, 71)
	}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", a.Timestamp, a.Timestamp.Add(time.Second), walletRows{Events: []tax.TaxableEvent{row}})
	require.NoError(t, e)
	require.Equal(t, "2932922", out.IndexedRewardCorrections[71].String())
	events := commissionFlowEventsFromRows(out, "reward-a", apiUatom, nil)
	key := flowKey{Time: a.Timestamp.Truncate(time.Microsecond), Height: a.Height, Message: 71, Sub: flowPayoutSub}
	for name, paid := range map[string]map[uint]flowRewardPayout{
		"no payout":    nil,
		"short payout": {71: {Key: key, Amount: decimal.RequireFromString("2932921")}},
		"more paid":    {71: {Key: key, Amount: decimal.RequireFromString("2932923")}},
	} {
		got := reconcileRewardPayouts("reward-a", events, paid)
		err := indexedRewardCorrectionsKept("reward-a", got, out.IndexedRewardCorrections)
		require.ErrorContains(t, err, "indexed reward payout differs", name)
		require.Equal(t, "correction_indexed_reward_payout_differs", privateReadCause(err), name)
	}
	got := reconcileRewardPayouts("reward-a", events, map[uint]flowRewardPayout{71: {Key: key, Amount: decimal.RequireFromString("2932922")}})
	require.NoError(t, indexedRewardCorrectionsKept("reward-a", got, out.IndexedRewardCorrections))
	require.NoError(t, indexedRewardCorrectionsKept("reward-a", got, nil))
}
