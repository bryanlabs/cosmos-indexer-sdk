package tax

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	lsmTestOwner    = "cosmos1lsmowner"
	lsmTestRecordA  = "cosmos1lsmrecorda"
	lsmTestRecordB  = "cosmos1lsmrecordb"
	lsmTestRecordC  = "cosmos1lsmrecordc"
	lsmTestValoperA = "cosmosvaloper1nuhls0wyf8slhmuasha5pz0u89jrf9nnugq8ak"
	lsmTestValoperB = "cosmosvaloper18m9wncg0w74vusm4xu0mk6ha2lpdrqq7c9cjq3"
	lsmTestValoperC = "cosmosvaloper1hkqejlyrj9h8knms9hwvrnp9xhqvjqlseh06vq"
)

// recordWithdrawal is the x/liquid per-record pattern: distribution pays the
// record's module account, which forwards its balance to the withdraw address.
func recordWithdrawal(record, validator, amount string) []indexerTxTypes.LogMessageEvent {
	return []indexerTxTypes.LogMessageEvent{
		ev(string(CategoryTransfer), [2]string{attributeRecipient, record}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, amount}),
		ev("withdraw_rewards", [2]string{attributeAmount, amount}, [2]string{attributeValidator, validator}, [2]string{attributeDelegator, record}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, record}, [2]string{attributeAmount, amount}),
	}
}

func lsmRewardRows(t *testing.T, out []TaxableEvent) map[[2]string]string {
	t.Helper()
	rows := map[[2]string]string{}
	for _, e := range out {
		if e.Category != string(CategoryReward) {
			continue
		}
		if e.ToAddr != lsmTestOwner {
			t.Fatalf("reward paid to %q, want the owner: %+v", e.ToAddr, e)
		}
		k := [2]string{e.ValidatorAddress, e.Denom}
		if _, dup := rows[k]; dup {
			t.Fatalf("two rows for %v: %+v", k, out)
		}
		rows[k] = e.Amount
	}
	return rows
}

func requireLSMRows(t *testing.T, out []TaxableEvent, want map[[2]string]string) {
	t.Helper()
	got := lsmRewardRows(t, out)
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// Mainnet gaia.liquid WithdrawAll at height 28085004 settled 31 records, 30
// on one validator and one on another, and emitted a single
// withdraw_tokenize_share_reward holding the total. Trimmed here to three
// records with the real amounts.
func TestTokenizeShareRewardSplitsAggregateByRecordValidator(t *testing.T) {
	var events []indexerTxTypes.LogMessageEvent
	events = append(events, recordWithdrawal(lsmTestRecordA, lsmTestValoperA, "157uatom")...)
	events = append(events, recordWithdrawal(lsmTestRecordB, lsmTestValoperA, "156uatom")...)
	events = append(events, recordWithdrawal(lsmTestRecordC, lsmTestValoperB, "3008uatom")...)
	events = append(events, ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, "3321uatom"}))
	out := classify(&MsgWithdrawAllTokenizeShareRecordReward{OwnerAddress: lsmTestOwner}, &indexerTxTypes.LogMessage{Events: events})
	requireLSMRows(t, out, map[[2]string]string{
		{lsmTestValoperA, fixtureNativeDenom}: "313",
		{lsmTestValoperB, fixtureNativeDenom}: "3008",
	})
}

// The legacy SDK fork merges same-type events: one transfer event with
// recipient, sender, amount runs, one withdraw_rewards with an amount and
// validator per withdrawal and no delegator, and one reward event with the
// summed multi-denom total. Shape of mainnet MsgRedeemTokensForShares at
// height 18492161: the record account withdraws 43121uatom, the owner's
// redelegated position 1uatom, and the record forwards its whole 48442uatom
// balance. The 1uatom is the owner's own reward, not the record's.
func TestTokenizeShareRewardLegacyMergedEvents(t *testing.T) {
	ibc := "ibc/B011C1A0AD5E717F674BA59FD8E05B2F946E4FD41C9CB3311A8B5E2D2B6E9F2C"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, lsmTestRecordA}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, "86301507999" + ibc + ",43121uatom"},
			[2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, lsmTestRecordA}, [2]string{attributeAmount, "111102764034" + ibc + ",48442uatom"},
			[2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, "1uatom"},
		),
		ev("withdraw_rewards",
			[2]string{attributeAmount, "86301507999" + ibc + ",43121uatom"}, [2]string{attributeValidator, lsmTestValoperC},
			[2]string{attributeAmount, "1uatom"}, [2]string{attributeValidator, lsmTestValoperC},
		),
		ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, "111102764034" + ibc + ",48442uatom"}),
	}}
	requireLSMRows(t, tokenizeShareRewardEvents(log, lsmTestOwner, ""), map[[2]string]string{
		{lsmTestValoperC, ibc}:                "111102764034",
		{lsmTestValoperC, fixtureNativeDenom}: "48442",
	})
}

// Two legacy withdrawals of the same amount from different validators, paid to
// two record accounts: the amount alone cannot tell which record is which.
func TestTokenizeShareRewardLegacyEqualAmountsStayUnattributed(t *testing.T) {
	five, record := "5uatom", "21uatom"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, lsmTestRecordA}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, five},
			[2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, lsmTestRecordA}, [2]string{attributeAmount, five},
			[2]string{attributeRecipient, lsmTestRecordB}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, five},
			[2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, lsmTestRecordB}, [2]string{attributeAmount, five},
			[2]string{attributeRecipient, lsmTestRecordC}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, record},
			[2]string{attributeRecipient, lsmTestOwner}, [2]string{attributeSender, lsmTestRecordC}, [2]string{attributeAmount, record},
		),
		ev("withdraw_rewards",
			[2]string{attributeAmount, five}, [2]string{attributeValidator, lsmTestValoperA},
			[2]string{attributeAmount, five}, [2]string{attributeValidator, lsmTestValoperB},
			[2]string{attributeAmount, record}, [2]string{attributeValidator, lsmTestValoperC},
		),
		ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, "31uatom"}),
	}}
	out := classify(&LegacyMsgWithdrawAllTokenizeShareRecordReward{MsgWithdrawAllTokenizeShareRecordReward{OwnerAddress: lsmTestOwner}}, log)
	requireLSMRows(t, out, map[[2]string]string{
		{lsmTestValoperC, fixtureNativeDenom}: "21",
		{"", fixtureNativeDenom}:              "10",
	})
}

// A record account that withdrew from two validators proves neither.
func TestTokenizeShareRewardAmbiguousRecordStaysUnattributed(t *testing.T) {
	reward := "8uatom"
	events := recordWithdrawal(lsmTestRecordA, lsmTestValoperA, reward)
	events = append(events,
		ev("withdraw_rewards", [2]string{attributeAmount, "0uatom"}, [2]string{attributeValidator, lsmTestValoperB}, [2]string{attributeDelegator, lsmTestRecordA}),
		ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, reward}),
	)
	requireLSMRows(t, tokenizeShareRewardEvents(&indexerTxTypes.LogMessage{Events: events}, lsmTestOwner, ""), map[[2]string]string{
		{"", fixtureNativeDenom}: "8",
	})
}

// Transfers that add up to more than the settled total cannot all be reward,
// so none of them is used.
func TestTokenizeShareRewardOverAttributionKeepsTotal(t *testing.T) {
	events := recordWithdrawal(lsmTestRecordA, lsmTestValoperA, "9uatom")
	events = append(events, ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, "4uatom"}))
	requireLSMRows(t, tokenizeShareRewardEvents(&indexerTxTypes.LogMessage{Events: events}, lsmTestOwner, ""), map[[2]string]string{
		{"", fixtureNativeDenom}: "4",
	})
}

// A redeem settles only the record its share denom names, so what the
// transfers cannot place belongs to that record's validator.
func TestRedeemTokenizeShareRewardUsesShareDenomValidator(t *testing.T) {
	shares := sdk.Coin{Denom: lsmTestValoperC + "/11838", Amount: sdkmath.NewInt(15727600)}
	raw, err := shares.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	msg := &MsgRedeemTokensForShares{DelegatorAddress: lsmTestOwner, AmountRaw: raw}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, lsmTestOwner}, [2]string{attributeAmount, "48442uatom"}),
	}}
	out := classify(msg, log)
	requireLSMRows(t, out, map[[2]string]string{{lsmTestValoperC, fixtureNativeDenom}: "48442"})
	for _, e := range out {
		if e.Category == string(CategoryReward) && e.RewardTrigger != "redeem" {
			t.Fatalf("trigger = %q", e.RewardTrigger)
		}
	}
	if v := shareDenomValidator([]byte{0x0a, 5, 'u', 'a', 't', 'o', 'm'}); v != "" {
		t.Fatalf("plain denom gave validator %q", v)
	}
}
