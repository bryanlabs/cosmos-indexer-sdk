package taxapi

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/shopspring/decimal"
)

const (
	ledgerTestAddr = "cosmos1wallet"
	ledgerTestVal  = "cosmosvaloper1a"
	ledgerTestVal2 = "cosmosvaloper1b"
)

func bi(n int64) *big.Int { return big.NewInt(n) }

func ev(id uint64, h int64, phase int, typ string, kv ...string) ledgerEvent {
	e := ledgerEvent{ID: id, Height: h, Phase: phase, Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Attrs = append(e.Attrs, ledgerAttr{kv[i], kv[i+1]})
	}
	return e
}

func intsEqual(t *testing.T, what string, got map[string]*big.Int, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g.Int64() != v {
			t.Fatalf("%s[%s]: got %v, want %d", what, k, got[k], v)
		}
	}
}

func TestLedgerRecordsSplitOnRepeatedKey(t *testing.T) {
	merged := ev(1, 10, phaseTx, "coin_spent",
		"spender", "a", "amount", "5uatom",
		"spender", "b", "amount", "7uatom")
	if got := merged.records(); len(got) != 2 || got[1]["spender"] != "b" || got[1]["amount"] != "7uatom" {
		t.Fatalf("merged log records = %v", got)
	}
	// LSM emits the amount before the address.
	lsm := ev(2, 10, phaseTx, "coin_received",
		"amount", "1uatom", "receiver", "a",
		"amount", "2uatom", "receiver", "b")
	if got := lsm.records(); len(got) != 2 || got[0]["receiver"] != "a" || got[1]["amount"] != "2uatom" {
		t.Fatalf("amount-first records = %v", got)
	}
}

func TestWalkBackLiquidAndFees(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor:    ledgerAnchor{Height: 100, Bank: map[string]*big.Int{"uatom": bi(1000), "ibc/X": bi(5)}},
		Events: []ledgerEvent{
			ev(1, 50, phaseTx, "coin_received", "receiver", ledgerTestAddr, "amount", "300uatom,5ibc/X"),
			ev(2, 60, phaseTx, "coin_spent", "spender", ledgerTestAddr, "amount", "100uatom"),
			ev(3, 60, phaseTx, "coin_spent", "spender", "cosmos1other", "amount", "999uatom"),
			ev(4, 90, phaseEnd, "coin_received", "receiver", ledgerTestAddr, "amount", "50uatom"),
		},
		Fees: []ledgerFee{{Height: 60, Coins: sdk.NewCoins(sdk.NewInt64Coin("uatom", 20))}},
	}
	got, err := walkBack(in, []int64{100, 89, 55, 49})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h100", got[100].Liquid, map[string]int64{"uatom": 1000, "ibc/X": 5})
	intsEqual(t, "h89", got[89].Liquid, map[string]int64{"uatom": 950, "ibc/X": 5})
	intsEqual(t, "h55", got[55].Liquid, map[string]int64{"uatom": 1070, "ibc/X": 5})
	intsEqual(t, "h49", got[49].Liquid, map[string]int64{"uatom": 770})
}

func TestWalkBackNegativeIsInconsistent(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor:    ledgerAnchor{Height: 100, Bank: map[string]*big.Int{"uatom": bi(10)}},
		Events:    []ledgerEvent{ev(1, 50, phaseTx, "coin_received", "receiver", ledgerTestAddr, "amount", "30uatom")},
	}
	_, err := walkBack(in, []int64{40})
	var inc ledgerInconsistent
	if !errors.As(err, &inc) {
		t.Fatalf("err = %v, want ledgerInconsistent", err)
	}
}

func TestWalkBackStaking(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		Operator:  ledgerTestVal,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(500), ledgerTestVal2: bi(200)},
			Rates:       map[string]validatorRate{ledgerTestVal: unitRate(1000), ledgerTestVal2: unitRate(300)},
		},
		Events: []ledgerEvent{
			ev(1, 20, phaseTx, "create_validator", "validator", ledgerTestVal, "amount", "100"),
			ev(2, 30, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "400uatom"),
			ev(3, 40, phaseTx, "redelegate", "delegator", ledgerTestAddr, "source_validator", ledgerTestVal, "destination_validator", ledgerTestVal2, "amount", "150uatom"),
			ev(4, 50, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom"),
		},
	}
	got, err := walkBack(in, []int64{45, 35, 25, 10})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h45", got[45].Delegations, map[string]int64{ledgerTestVal: 550, ledgerTestVal2: 200})
	intsEqual(t, "h35", got[35].Delegations, map[string]int64{ledgerTestVal: 700, ledgerTestVal2: 50})
	intsEqual(t, "h25", got[25].Delegations, map[string]int64{ledgerTestVal: 300, ledgerTestVal2: 50})
	intsEqual(t, "h10", got[10].Delegations, map[string]int64{ledgerTestVal: 200, ledgerTestVal2: 50})
	if len(got[10].Approximate) > 0 {
		t.Fatalf("validators at one token per share rebuild exactly, got approximate %v", got[10].Approximate)
	}
}

func unitRate(n int64) validatorRate {
	return validatorRate{Tokens: bi(n), Shares: decimal.NewFromInt(n)}
}

func TestWalkBackRoundingIsApproximate(t *testing.T) {
	slashed := validatorRate{Tokens: bi(999), Shares: decimal.NewFromInt(1000)}
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(99), ledgerTestVal2: bi(10)},
			Rates:       map[string]validatorRate{ledgerTestVal: slashed, ledgerTestVal2: unitRate(500)},
		},
		// The delegation of 100 shows as 99 at the anchor: shares hold their
		// value and the shown tokens round down.
		Events: []ledgerEvent{
			ev(1, 30, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "100uatom"),
			ev(2, 20, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "10uatom"),
		},
	}
	got, err := walkBack(in, []int64{25, 10})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h25", got[25].Delegations, map[string]int64{ledgerTestVal2: 10})
	if !got[25].Approximate[ledgerTestVal] {
		t.Fatal("a delegation on a validator below one token per share must be approximate")
	}
	if got[10].Approximate[ledgerTestVal2] {
		t.Fatal("rounding on one validator must not mark another approximate")
	}
	in.Events = nil
	got, err = walkBack(in, []int64{50})
	if err != nil {
		t.Fatal(err)
	}
	if !got[50].Approximate[ledgerTestVal] || got[50].Approximate[ledgerTestVal2] {
		t.Fatalf("a delegation below one token per share drifts with no event of the wallet's, got %v", got[50].Approximate)
	}
}

func TestWalkBackRedeemShares(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(700)},
			Rates:       map[string]validatorRate{ledgerTestVal: {Tokens: bi(1000), Shares: decimal.NewFromInt(2000)}},
		},
		// LSM names the redeemed shares in the record's share denom.
		Events: []ledgerEvent{ev(1, 50, phaseTx, "redeem_shares", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "401"+ledgerTestVal+"/7")},
	}
	got, err := walkBack(in, []int64{40})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h40", got[40].Delegations, map[string]int64{ledgerTestVal: 500})
	if !got[40].Approximate[ledgerTestVal] {
		t.Fatal("redeemed shares valued at a rate other than one must be approximate")
	}
	in.Anchor.Rates = nil
	_, err = walkBack(in, []int64{40})
	var unk ledgerUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("redeemed shares of a validator without a rate must be unknown, got %v", err)
	}
}

func TestScanHole(t *testing.T) {
	chain := []nodeTx{
		{Hash: "AA", Accounts: []string{"cosmos1a"}},
		{Hash: "BB", Accounts: []string{"cosmos1b"}},
		{Hash: "BB", Code: 18, Accounts: []string{"cosmos1b"}},
		{Hash: "CC", Code: 5, Accounts: []string{"cosmos1c"}},
	}
	full := []indexedTx{{Hash: "aa", Messages: true}, {Hash: "BB", Messages: true}, {Hash: "CC", Code: 5}}
	if n, _ := scanHole(chain, full); n != 0 {
		t.Fatalf("a block with every transaction and its messages is no hole, got %d", n)
	}
	n, accounts := scanHole(chain, []indexedTx{{Hash: "AA"}, {Hash: "BB", Messages: true}})
	if n != 3 {
		t.Fatalf("the included-twice copy counts once, got %d chain txs", n)
	}
	if strings.Join(accounts, ",") != "cosmos1a,cosmos1c" {
		t.Fatalf("a successful tx without messages and a missing failed tx name their accounts, got %v", accounts)
	}
}

func TestWalkBackSlashIsApproximate(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor:    ledgerAnchor{Height: 100, Bank: map[string]*big.Int{}, Delegations: map[string]*big.Int{ledgerTestVal: bi(10990)}, Rates: map[string]validatorRate{ledgerTestVal: unitRate(20000)}},
		// The delegation made in the slash block's transactions was not slashed.
		Events:  []ledgerEvent{ev(1, 60, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "1000uatom")},
		Slashes: []ledgerSlash{{Height: 60, Validator: ledgerTestVal, Fraction: decimal.RequireFromString("0.001")}},
	}
	got, err := walkBack(in, []int64{70, 59})
	if err != nil {
		t.Fatal(err)
	}
	if got[70].Approximate[ledgerTestVal] {
		t.Fatal("a slash after the target must not mark it approximate")
	}
	intsEqual(t, "h59", got[59].Delegations, map[string]int64{ledgerTestVal: 10000})
	if !got[59].Approximate[ledgerTestVal] {
		t.Fatal("a delegation carried back across a slash must be approximate")
	}
}

func TestUnbondingAt(t *testing.T) {
	anchorTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height: 100,
			Time:   anchorTime,
			Unbonding: []unbondingEntry{
				{Validator: ledgerTestVal, CreationHeight: 10, Balance: bi(70)},
				{Validator: ledgerTestVal, CreationHeight: 80, Balance: bi(40)},
			},
		},
		Events: []ledgerEvent{
			// Part of the height-10 entry was cancelled after the target.
			ev(1, 60, phaseTx, "cancel_unbonding_delegation", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "30uatom", "creation_height", "10"),
			// An entry open at the target completed before the anchor.
			ev(2, 70, phaseEnd, "complete_unbonding", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "25uatom"),
			// Created after the target and completed by the anchor: never open at the target.
			ev(3, 55, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "15uatom", "completion_time", anchorTime.Add(-time.Hour).Format(time.RFC3339Nano)),
			ev(4, 90, phaseEnd, "complete_unbonding", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "15uatom"),
			// Created after the target and still open at the anchor.
			ev(5, 80, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "40uatom", "completion_time", anchorTime.Add(time.Hour).Format(time.RFC3339Nano)),
		},
	}
	got, approx := unbondingAt(in, 50)
	intsEqual(t, "unbonding", pruneZero(got), map[string]int64{ledgerTestVal: 100, ledgerTestVal2: 25})
	if len(approx) != 0 {
		t.Fatalf("approx = %v", approx)
	}
	in.Slashes = []ledgerSlash{{Height: 75, Validator: ledgerTestVal, DoubleSign: true}}
	if _, approx = unbondingAt(in, 50); !approx[ledgerTestVal] {
		t.Fatal("an entry open across a double sign slash must be approximate")
	}
}

func TestTouchedValidators(t *testing.T) {
	anchor := ledgerAnchor{Delegations: map[string]*big.Int{ledgerTestVal: bi(1)}, Unbonding: []unbondingEntry{{Validator: ledgerTestVal2}}}
	got := touchedValidators(anchor, []ledgerEvent{
		ev(1, 5, phaseTx, "redelegate", "source_validator", "cosmosvaloper1c", "destination_validator", "cosmosvaloper1d", "amount", "1uatom"),
	})
	for _, v := range []string{ledgerTestVal, ledgerTestVal2, "cosmosvaloper1c", "cosmosvaloper1d"} {
		if !got[v] {
			t.Fatalf("missing %s in %v", v, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func redelRow(msg, ev uint64, height int64, typ, key, value string) ledgerAttrRow {
	return ledgerAttrRow{EventID: ev, MessageID: msg, Height: height, Phase: 1, Type: typ, Key: key, Value: value}
}

func TestAttributeRedelegations(t *testing.T) {
	other := "cosmos1other"
	rows := []ledgerAttrRow{
		// The wallet's own MsgBeginRedelegate.
		redelRow(1, 10, 100, "message", "action", "/cosmos.staking.v1beta1.MsgBeginRedelegate"),
		redelRow(1, 10, 100, "message", "sender", ledgerTestAddr),
		redelRow(1, 10, 100, "message", "module", "staking"),
		redelRow(1, 11, 100, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(1, 11, 100, "withdraw_rewards", "delegator", ledgerTestAddr),
		redelRow(1, 12, 100, "redelegate", "source_validator", ledgerTestVal),
		redelRow(1, 12, 100, "redelegate", "destination_validator", ledgerTestVal2),
		redelRow(1, 12, 100, "redelegate", "amount", "5uatom"),
		// Someone else's redelegation that only sent rewards to the wallet.
		redelRow(2, 20, 110, "message", "sender", other),
		redelRow(2, 20, 110, "message", "module", "staking"),
		redelRow(2, 21, 110, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(2, 21, 110, "withdraw_rewards", "delegator", other),
		redelRow(2, 22, 110, "redelegate", "source_validator", ledgerTestVal),
		redelRow(2, 22, 110, "redelegate", "destination_validator", ledgerTestVal2),
		redelRow(2, 22, 110, "redelegate", "amount", "7uatom"),
		// An authz exec for two granters, matched by the preceding withdrawal.
		redelRow(3, 30, 120, "message", "module", "authz"),
		redelRow(3, 31, 120, "withdraw_rewards", "validator", ledgerTestVal2),
		redelRow(3, 31, 120, "withdraw_rewards", "delegator", other),
		redelRow(3, 32, 120, "redelegate", "source_validator", ledgerTestVal2),
		redelRow(3, 32, 120, "redelegate", "destination_validator", ledgerTestVal),
		redelRow(3, 32, 120, "redelegate", "amount", "3uatom"),
		redelRow(3, 33, 120, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(3, 33, 120, "withdraw_rewards", "delegator", ledgerTestAddr),
		redelRow(3, 34, 120, "redelegate", "source_validator", ledgerTestVal),
		redelRow(3, 34, 120, "redelegate", "destination_validator", ledgerTestVal2),
		redelRow(3, 34, 120, "redelegate", "amount", "4uatom"),
	}
	events, unattributed := attributeRedelegations(rows, ledgerTestAddr)
	if unattributed != 0 {
		t.Fatalf("unattributed = %d", unattributed)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	for i, want := range []string{"5uatom", "4uatom"} {
		rec := events[i].records()
		if len(rec) != 1 || rec[0]["delegator"] != ledgerTestAddr || rec[0]["amount"] != want {
			t.Fatalf("event %d = %+v", i, rec)
		}
	}

	// Two granters withdrawing from the same source leave it ambiguous.
	ambiguous := []ledgerAttrRow{
		redelRow(4, 40, 130, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(4, 40, 130, "withdraw_rewards", "delegator", other),
		redelRow(4, 41, 130, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(4, 41, 130, "withdraw_rewards", "delegator", ledgerTestAddr),
		redelRow(4, 42, 130, "redelegate", "source_validator", ledgerTestVal),
		redelRow(4, 42, 130, "redelegate", "destination_validator", ledgerTestVal2),
		redelRow(4, 42, 130, "redelegate", "amount", "1uatom"),
	}
	events, unattributed = attributeRedelegations(ambiguous, ledgerTestAddr)
	if len(events) != 0 || unattributed != 130 {
		t.Fatalf("events = %+v, unattributed = %d", events, unattributed)
	}
}
