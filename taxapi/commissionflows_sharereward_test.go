package taxapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	shareTestOwner    = "cosmos1owner"
	shareTestRecord   = "cosmos1record"
	shareTestRedeemer = "cosmos1redeemer"
	shareTestPool     = "cosmos1bondedpool"
)

func shareTestTransfer(msg, event uint, rcpt, from, amount string) []flowTransferAttr {
	at := time.Date(2025, 6, 11, 0, 0, 0, 0, time.UTC)
	row := func(idx uint64, key, value string) flowTransferAttr {
		return flowTransferAttr{EventID: event, MessageID: msg, Height: 26108851, TimeStamp: at, Index: idx, Key: key, Value: value}
	}
	return []flowTransferAttr{row(0, "recipient", rcpt), row(1, "sender", from), row(2, "amount", amount)}
}

func shareTestEvent(msg, event uint, typ string, kv ...string) []flowShareEventAttr {
	var out []flowShareEventAttr
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, flowShareEventAttr{EventID: event, MessageID: msg, Type: typ, Index: uint64(i / 2), Key: kv[i], Value: kv[i+1]})
	}
	return out
}

// shareTestRedeem is a redeem by another wallet that settles the owner's
// record: the record's account withdraws 509635583 uatom from the
// distribution module and pays it to the owner, and the redeemer's own
// 3764 uatom reward is paid by the distribution module. Only the transfer to
// the owner is in the owner's payout read.
func shareTestRedeem(msg uint) ([]flowTransferAttr, []flowShareEventAttr) {
	const coins = "26ibc/AAA,509635583uatom"
	transfers := shareTestTransfer(msg, 12, shareTestOwner, shareTestRecord, coins)
	var events []flowShareEventAttr
	events = append(events, shareTestEvent(msg, 5, "withdraw_rewards", "amount", coins, "validator", flowTestValidator, "delegator", shareTestRecord, "msg_index", "0")...)
	events = append(events, shareTestEvent(msg, 14, "withdraw_tokenize_share_reward", "withdraw_address", shareTestOwner, "amount", coins, "msg_index", "0")...)
	events = append(events, shareTestEvent(msg, 29, "withdraw_rewards", "amount", "3764uatom", "validator", flowTestValidator, "delegator", shareTestRedeemer, "msg_index", "0")...)
	return transfers, events
}

// A reward a tokenize-share record pays out is paid by the record's own
// account, not the distribution module. Before this, a redeem by another
// wallet dropped the owner's 509.635583 ATOM reward row and the trace absorbed
// the coins as overdraw and drift.
func TestFlowShareRecordRewardPayout(t *testing.T) {
	const msg = 18666132
	dist := moduleAddress("distribution")
	transfers, events := shareTestRedeem(msg)
	require.Equal(t, []uint{msg}, shareRewardCandidates(transfers, shareTestOwner, dist, "uatom"))
	require.Empty(t, rewardPayoutsFrom(transfers, shareTestOwner, dist, "uatom"))

	paid := shareRecordPayoutsFrom(transfers, events, shareTestOwner, "uatom")
	require.Len(t, paid, 1)
	require.Equal(t, "509635583", paid[msg].Amount.String())
	require.Equal(t, int64(26108851), paid[msg].Key.Height)
	require.Equal(t, uint(msg), paid[msg].Key.Message)

	k := paid[msg].Key
	k.Sub = 0
	row := flowEvent{Key: k, Category: apiReward, Dir: "in", Amount: amt(509635583), To: shareTestOwner, Validator: flowTestValidator, Trigger: "redeem"}
	// The old read had no payout for the message and dropped the row.
	require.Empty(t, reconcileRewardPayouts(shareTestOwner, []flowEvent{row}, nil))
	// The share-record payout keeps it as booked.
	require.Equal(t, []flowEvent{row}, reconcileRewardPayouts(shareTestOwner, []flowEvent{row}, paid))
	// With no reward row, the payout still becomes a reward inflow.
	got := reconcileRewardPayouts(shareTestOwner, nil, paid)
	require.Len(t, got, 1)
	require.Equal(t, apiReward, got[0].Category)
	require.Equal(t, "509635583", got[0].Amount.String())
	require.Equal(t, shareTestOwner, got[0].To)

	// The redeemer is not the record's withdraw address: nothing the record
	// paid is its reward, and its 509 ATOM row is cut to what it was paid.
	require.Empty(t, shareRecordPayoutsFrom(transfers, events, shareTestRedeemer, "uatom"))

	// In a trace, the owner's coins arrive as reward, so selling them later
	// draws nothing it did not hold. Without the payout the row is dropped
	// and the sale overdraws by the reward.
	run := func(paid map[uint]flowRewardPayout) *flowTrace {
		l := newFakeFlowLedger()
		l.commission(flowAt(1, 0), flowTestReceiver, 100)
		l.send(flowAt(2, 0), flowTestReceiver, shareTestOwner, 100)
		r := row
		r.Key = flowAt(3, 0)
		r.Key.Message = msg
		r.Amount = amt(50)
		p := map[uint]flowRewardPayout{}
		for m := range paid {
			p[m] = flowRewardPayout{Key: r.Key, Amount: amt(50)}
		}
		l.rows[shareTestOwner] = append(l.rows[shareTestOwner], reconcileRewardPayouts(shareTestOwner, []flowEvent{r}, p)...)
		l.send(flowAt(4, 0), shareTestOwner, flowTestExchange, 150)
		return runFlowTest(t, l, flowTestReceiver)
	}
	before := run(nil).Wallets[shareTestOwner].Overdraw[flowRuleFIFO]
	after := run(paid).Wallets[shareTestOwner].Overdraw[flowRuleFIFO]
	require.True(t, before.Equal(amt(50)), before.String())
	require.True(t, after.IsZero(), after.String())
}

// A plain distribution payout reads exactly as before: no share-record
// payout is added, even beside LSM events in the same message, and a redeem
// by the owner itself counts both its own reward and its record's.
func TestFlowShareRecordKeepsDistributionPayouts(t *testing.T) {
	dist := moduleAddress("distribution")
	transfers := shareTestTransfer(7, 1, shareTestOwner, dist, "120uatom")
	require.Empty(t, shareRewardCandidates(transfers, shareTestOwner, dist, "uatom"))
	paid := rewardPayoutsFrom(transfers, shareTestOwner, dist, "uatom")
	require.Equal(t, "120", paid[7].Amount.String())
	events := shareTestEvent(7, 2, "withdraw_rewards", "amount", "120uatom", "validator", flowTestValidator, "delegator", shareTestOwner)
	require.Empty(t, shareRecordPayoutsFrom(transfers, events, shareTestOwner, "uatom"))

	// The owner redeems its own tokens: the distribution module pays its own
	// delegation's 10 uatom and the record's account pays the record's 40.
	own := append(shareTestTransfer(8, 1, shareTestOwner, dist, "10uatom"), shareTestTransfer(8, 2, shareTestOwner, shareTestRecord, "40uatom")...)
	ev := append(shareTestEvent(8, 3, "withdraw_rewards", "amount", "40uatom", "delegator", shareTestRecord),
		shareTestEvent(8, 4, "withdraw_tokenize_share_reward", "withdraw_address", shareTestOwner, "amount", "40uatom")...)
	ev = append(ev, shareTestEvent(8, 5, "withdraw_rewards", "amount", "10uatom", "delegator", shareTestOwner)...)
	require.Equal(t, "10", rewardPayoutsFrom(own, shareTestOwner, dist, "uatom")[8].Amount.String())
	require.Equal(t, "40", shareRecordPayoutsFrom(own, ev, shareTestOwner, "uatom")[8].Amount.String())
}

// Coins from a share-record account count as reward only inside the
// settlement that names the wallet, and never past the settled amount.
func TestFlowShareRecordRejectsOtherSends(t *testing.T) {
	dist := moduleAddress("distribution")
	// A plain bank send from a record's account, with no
	// withdraw_tokenize_share_reward in the message, is not a reward.
	send := shareTestTransfer(9, 1, shareTestOwner, shareTestRecord, "70uatom")
	require.Equal(t, []uint{9}, shareRewardCandidates(send, shareTestOwner, dist, "uatom"))
	withdrew := shareTestEvent(9, 2, "withdraw_rewards", "amount", "70uatom", "delegator", shareTestRecord)
	require.Empty(t, shareRecordPayoutsFrom(send, withdrew, shareTestOwner, "uatom"))
	require.Empty(t, shareRecordPayoutsFrom(send, nil, shareTestOwner, "uatom"))

	transfers, events := shareTestRedeem(10)
	// A settlement naming another withdraw address pays this wallet nothing.
	other := shareTestEvent(10, 14, "withdraw_tokenize_share_reward", "withdraw_address", shareTestRedeemer, "amount", "509635583uatom")
	require.Empty(t, shareRecordPayoutsFrom(transfers, append(events[:4:4], other...), shareTestOwner, "uatom"))
	// A sender that withdrew no reward in the message is not a record.
	stranger := shareTestTransfer(10, 13, shareTestOwner, shareTestPool, "5uatom")
	got := shareRecordPayoutsFrom(append(transfers, stranger...), events, shareTestOwner, "uatom")
	require.Equal(t, "509635583", got[10].Amount.String())
	// More than the settled amount is capped at it.
	extra := shareTestTransfer(10, 15, shareTestOwner, shareTestRecord, "1000uatom")
	got = shareRecordPayoutsFrom(append(transfers, extra...), events, shareTestOwner, "uatom")
	require.Equal(t, "509635583", got[10].Amount.String())
}
