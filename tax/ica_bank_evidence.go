package tax

import (
	"fmt"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func icaRedeemPrincipalTransfer(sender, recipient, signer, module string) bool {
	if sender == signer && recipient == module || sender == module && recipient == signer {
		return true
	}
	bonded := authtypes.NewModuleAddress(stakingtypes.BondedPoolName).String()
	unbonded := authtypes.NewModuleAddress(stakingtypes.NotBondedPoolName).String()
	return sender == bonded && recipient == unbonded || sender == unbonded && recipient == bonded
}

// Check all retained bank debits and credits against transfers plus burns.
// This is deliberately independent of tax income: a burn or pool movement is
// evidence to retain and reconcile, not another wallet reward.
func icaRedeemBankEvidence(log *txlog.LogMessage, msgs []*MsgRedeemTokensForShares) error {
	actualSpent, actualReceived := map[string]sdk.Coins{}, map[string]sdk.Coins{}
	wantSpent, wantReceived := map[string]sdk.Coins{}, map[string]sdk.Coins{}
	present := false
	add := func(dst map[string]sdk.Coins, who, amount string) error {
		coins, err := sdk.ParseCoinsNormalized(amount)
		if err != nil || who == "" {
			return fmt.Errorf("malformed bank evidence")
		}
		dst[who] = dst[who].Add(coins...)
		return nil
	}
	for _, e := range log.Events {
		var first string
		switch e.Type {
		case eventCoinSpent:
			first = attributeSpender
			present = true
		case eventCoinReceived:
			first = attributeReceiver
			present = true
		case string(CategoryTransfer):
			first = attributeRecipient
		case eventBurn:
			first = "burner"
		default:
			continue
		}
		groups, err := legacyRecords(e.Attributes, first)
		if err != nil {
			return err
		}
		for _, g := range groups {
			who, amount := attrValue(g, first), attrValue(g, attributeAmount)
			switch e.Type {
			case eventCoinSpent:
				err = add(actualSpent, who, amount)
			case eventCoinReceived:
				err = add(actualReceived, who, amount)
			case string(CategoryTransfer):
				if err = add(wantSpent, attrValue(g, attributeSender), amount); err == nil {
					err = add(wantReceived, who, amount)
				}
			case eventBurn:
				err = add(wantSpent, who, amount)
			}
			if err != nil {
				return err
			}
		}
	}
	if !present {
		return nil
	}
	// Redeeming returns native principal to the signer and stakes it again.
	// DelegateCoins emits debit/credit events but no transfer event. Prove
	// those bank-only legs separately; do not mistake them for hidden income.
	principalTotal := sdk.Coins{}
	transfers := [][]txlog.Attribute{}
	burns := [][]txlog.Attribute{}
	for _, ev := range log.Events {
		if ev.Type == string(CategoryTransfer) {
			rs, err := legacyRecords(ev.Attributes, attributeRecipient)
			if err != nil {
				return err
			}
			transfers = append(transfers, rs...)
		}
		if ev.Type == eventBurn {
			rs, err := legacyRecords(ev.Attributes, "burner")
			if err != nil {
				return err
			}
			burns = append(burns, rs...)
		}
	}
	if len(burns) != len(msgs) {
		return fmt.Errorf("share burn count differs")
	}
	for i, m := range msgs {
		var shares sdk.Coin
		if err := shares.Unmarshal(m.AmountRaw); err != nil {
			return err
		}
		found := false
		for j, tr := range transfers {
			if attrValue(tr, attributeSender) != m.DelegatorAddress || attrValue(tr, attributeAmount) != shares.String() {
				continue
			}
			if found || j+1 == len(transfers) {
				return fmt.Errorf("ambiguous principal evidence")
			}
			found = true
			next := transfers[j+1]
			module := attrValue(tr, attributeRecipient)
			if attrValue(next, attributeSender) != module || attrValue(next, attributeRecipient) != m.DelegatorAddress || attrValue(burns[i], "burner") != module || attrValue(burns[i], attributeAmount) != shares.String() {
				return fmt.Errorf("share burn/principal evidence differs")
			}
			coins, err := sdk.ParseCoinsNormalized(attrValue(next, attributeAmount))
			if err != nil || len(coins) != 1 || coins[0].Denom != "uatom" {
				return fmt.Errorf("non-native redeem principal")
			}
			if err := add(wantSpent, m.DelegatorAddress, coins.String()); err != nil {
				return err
			}
			principalTotal = principalTotal.Add(coins...)
		}
		if !found {
			return fmt.Errorf("missing share evidence")
		}
	}
	credited := sdk.Coins{}
	for _, pool := range []string{authtypes.NewModuleAddress(stakingtypes.BondedPoolName).String(), authtypes.NewModuleAddress(stakingtypes.NotBondedPoolName).String()} {
		residual, negative := actualReceived[pool].SafeSub(wantReceived[pool]...)
		if negative {
			return fmt.Errorf("pool credit evidence differs")
		}
		for _, coin := range residual {
			if coin.Denom != "uatom" {
				return fmt.Errorf("non-native pool credit")
			}
		}
		credited = credited.Add(residual...)
		wantReceived[pool] = wantReceived[pool].Add(residual...)
		if len(wantReceived[pool]) == 0 {
			delete(wantReceived, pool)
		}
	}
	if !credited.IsEqual(principalTotal) {
		return fmt.Errorf("restaked principal does not reconcile")
	}
	for _, pair := range [][2]map[string]sdk.Coins{{actualSpent, wantSpent}, {actualReceived, wantReceived}} {
		if len(pair[0]) != len(pair[1]) {
			return fmt.Errorf("bank account evidence count differs")
		}
		for who, coins := range pair[0] {
			if !coins.IsEqual(pair[1][who]) {
				return fmt.Errorf("bank evidence does not reconcile")
			}
		}
	}
	return nil
}
