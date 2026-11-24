package tax

import (
	"strings"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// A contract can redeem tokenized shares itself, as a DAO DAO proposal that
// redeems the DAO's LSM shares does (mainnet message 8123609, proposal 37,
// two records). The chain then settles each record's accrued reward exactly
// as it does for a native MsgRedeemTokensForShares: the record's module
// account withdraws its reward and pays it to the record owner's withdraw
// address, and a withdraw_tokenize_share_reward names that address and
// amount. That payment is the owner's staking reward, so it is booked the
// way the native redeem books it: a reward to the payee, trigger redeem,
// against the record's validator. The record account is never a wallet side.

// lsmSettlement is one record reward the chain paid during a contract call.
type lsmSettlement struct {
	record, payee, validator string
	coins                    sdk.Coins
}

// lsmRecordSettlements finds each withdraw_tokenize_share_reward in the log
// whose withdraw address and amount match exactly one transfer from a record
// account the log proves: the account derives from a record id that a
// "<valoper>/<record id>" share denom in the same log names, which also gives
// the validator. A settlement without exactly one such transfer is left to
// the bank fallback, as before.
func lsmRecordSettlements(log *indexerTxTypes.LogMessage) []lsmSettlement {
	if log == nil {
		return nil
	}
	type claim struct{ payee, amount string }
	var claims []claim
	records := map[string]string{}
	for _, ev := range log.Events {
		if ev.Type == eventWithdrawTokenizeShareReward {
			for _, g := range ibcAttributeGroups(ev, attributeWithdrawAddress, attributeAmount) {
				if g[attributeWithdrawAddress] != "" && g[attributeAmount] != "" {
					claims = append(claims, claim{g[attributeWithdrawAddress], g[attributeAmount]})
				}
			}
		}
		for _, a := range ev.Attributes {
			if a.Key != attributeAmount {
				continue
			}
			coins, err := sdk.ParseCoinsNormalized(a.Value)
			if err != nil {
				continue
			}
			for _, c := range coins {
				valoper, id, ok := strings.Cut(c.Denom, "/")
				if !ok || id == "" || strings.Trim(id, "0123456789") != "" {
					continue
				}
				if _, err := sdk.ValAddressFromBech32(valoper); err != nil {
					continue
				}
				records[lsmRecordAccount(id)] = valoper
			}
		}
	}
	if len(claims) == 0 || len(records) == 0 {
		return nil
	}
	legs := transfersIn(log)
	var out []lsmSettlement
	for _, c := range claims {
		coins, err := sdk.ParseCoinsNormalized(c.amount)
		if err != nil || coins.IsZero() {
			continue
		}
		var match *coinTransfer
		n := 0
		for i := range legs {
			l := &legs[i]
			if _, ok := records[l.from]; ok && l.to == c.payee && l.coins.String() == coins.String() {
				match, n = l, n+1
			}
		}
		if n != 1 {
			continue
		}
		out = append(out, lsmSettlement{record: match.from, payee: c.payee, validator: records[match.from], coins: coins})
	}
	return dedupeSettlements(out)
}

// dedupeSettlements drops a settlement whose transfer another claim already
// took, so two identical claims never book one payment twice.
func dedupeSettlements(in []lsmSettlement) []lsmSettlement {
	seen := map[string]bool{}
	var out []lsmSettlement
	for _, s := range in {
		k := s.record + "|" + s.payee + "|" + s.coins.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// lsmSettlementEvents are the reward rows of a contract call's record
// settlements, one per record and denom, in the order the chain paid them.
func lsmSettlementEvents(settled []lsmSettlement) []TaxableEvent {
	var out []TaxableEvent
	for _, s := range settled {
		for _, c := range s.coins {
			if !c.Amount.IsPositive() {
				continue
			}
			out = append(out, TaxableEvent{
				Category:         string(CategoryReward),
				ToAddr:           s.payee,
				Amount:           c.Amount.String(),
				Denom:            c.Denom,
				ValidatorAddress: s.validator,
				RewardTrigger:    "redeem",
			})
		}
	}
	return out
}

// withoutSettlementLegs removes the transfers a settlement booked, once
// each, so the bank fallback never books them again as a plain transfer.
func withoutSettlementLegs(legs []coinTransfer, settled []lsmSettlement) []coinTransfer {
	if len(settled) == 0 {
		return legs
	}
	left := map[string]int{}
	for _, s := range settled {
		left[s.record+"|"+s.payee+"|"+s.coins.String()]++
	}
	out := make([]coinTransfer, 0, len(legs))
	for _, l := range legs {
		k := l.from + "|" + l.to + "|" + l.coins.String()
		if left[k] > 0 {
			left[k]--
			continue
		}
		out = append(out, l)
	}
	return out
}
