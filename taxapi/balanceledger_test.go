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
	if len(got[10].DelegationError) > 0 {
		t.Fatalf("validators at one token per share rebuild exactly, got bounds %v", got[10].DelegationError)
	}
}

func unitRate(n int64) validatorRate {
	return validatorRate{Tokens: bi(n), Shares: decimal.NewFromInt(n)}
}

func rate(tokens, shares int64) validatorRate {
	return validatorRate{Tokens: bi(tokens), Shares: decimal.NewFromInt(shares)}
}

func TestWalkBackSharesAtRecordedRates(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(99), ledgerTestVal2: bi(10)},
			Shares:      map[string]decimal.Decimal{ledgerTestVal: decimal.NewFromInt(100), ledgerTestVal2: decimal.NewFromInt(10)},
			Rates:       map[string]validatorRate{ledgerTestVal: rate(999, 1000), ledgerTestVal2: unitRate(500)},
		},
		// Other delegators' rounding raised the rate from 0.985 to 0.999.
		Samples: map[string][]rateSample{ledgerTestVal: {{10, rate(985, 1000)}, {25, rate(990, 1000)}, {50, rate(998, 1000)}}},
	}
	got, err := walkBack(in, []int64{50, 25})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h50", got[50].Delegations, map[string]int64{ledgerTestVal: 99, ledgerTestVal2: 10})
	intsEqual(t, "h25", got[25].Delegations, map[string]int64{ledgerTestVal: 99, ledgerTestVal2: 10})
	if len(got[25].DelegationError) > 0 {
		t.Fatalf("a delegation with no movement is shown exactly at the recorded rate, got bounds %v", got[25].DelegationError)
	}

	// A delegation names its shares; an undelegation names only the tokens
	// paid out, rounded down.
	in.Events = []ledgerEvent{
		ev(1, 30, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom", "new_shares", "50.505050505050505050"),
		ev(2, 20, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "1000uatom"),
		ev(3, 15, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "5uatom"),
	}
	in.Anchor.Shares[ledgerTestVal] = decimal.RequireFromString("150.505050505050505050")
	in.Anchor.Delegations[ledgerTestVal] = bi(150)
	got, err = walkBack(in, []int64{25, 12})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h25", got[25].Delegations, map[string]int64{ledgerTestVal: 99, ledgerTestVal2: 10})
	if len(got[25].DelegationError) > 0 {
		t.Fatalf("a delegation's own shares undo exactly, got bounds %v", got[25].DelegationError)
	}
	if got[12].Delegations[ledgerTestVal2].Int64() != 15 || got[12].DelegationError[ledgerTestVal2] != nil {
		t.Fatalf("an undelegation at one token per share undoes exactly, got %v %v", got[12].Delegations, got[12].DelegationError)
	}
	// 1000 tokens paid out at a rate in [0.985, 0.990] are 1010.1 to 1016.3
	// shares, 994 to 1001 tokens at 0.985 with the 99 still held.
	d, bound := got[12].Delegations[ledgerTestVal], got[12].DelegationError[ledgerTestVal]
	if bound == nil || bound.Int64() < 1 || bound.Int64() > 8 || d.Int64() < 1093 || d.Int64() > 1100 {
		t.Fatalf("an undelegation on a validator below one token per share is bounded, got %v ± %v", d, bound)
	}

	in.Samples = nil
	_, err = walkBack(in, []int64{25})
	var unk ledgerUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("a delegation below one token per share without recorded rates must be unknown, got %v", err)
	}
}

func TestWalkBackRedeemShares(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(630)},
			Shares:      map[string]decimal.Decimal{ledgerTestVal: decimal.NewFromInt(1400)},
			Rates:       map[string]validatorRate{ledgerTestVal: rate(900, 2000)},
		},
		// LSM names the redeemed shares in the record's share denom. They are
		// paid out at the rate of the redemption, before a later slash.
		Events:  []ledgerEvent{ev(1, 50, phaseTx, "redeem_shares", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "400"+ledgerTestVal+"/7")},
		Slashes: []ledgerSlash{{Height: 60, Validator: ledgerTestVal, Fraction: decimal.RequireFromString("0.1")}},
		Samples: map[string][]rateSample{ledgerTestVal: {{40, rate(1000, 2000)}}},
	}
	got, err := walkBack(in, []int64{40})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h40", got[40].Delegations, map[string]int64{ledgerTestVal: 500})
	if len(got[40].DelegationError) > 0 {
		t.Fatalf("redeemed shares between recorded rates rebuild exactly, got bounds %v", got[40].DelegationError)
	}
	// When the wallet's own spend in the message names the tokens delegated
	// back, they decide the shares, not the share tokens times the rate.
	in.Events[0].Msg = 9
	in.Events = append(in.Events, ledgerEvent{ID: 2, Msg: 9, Height: 50, Phase: phaseTx, Type: "coin_spent", Attrs: []ledgerAttr{{"spender", ledgerTestAddr}, {"amount", "201uatom"}}},
		ledgerEvent{ID: 3, Msg: 9, Height: 50, Phase: phaseTx, Type: "coin_received", Attrs: []ledgerAttr{{"receiver", ledgerTestAddr}, {"amount", "201uatom"}}})
	in.Anchor.Shares[ledgerTestVal] = decimal.NewFromInt(1402)
	got, err = walkBack(in, []int64{40})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "h40 spent", got[40].Delegations, map[string]int64{ledgerTestVal: 500})
	in.Samples = nil
	_, err = walkBack(in, []int64{40})
	var unk ledgerUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("redeemed shares of a validator without recorded rates must be unknown, got %v", err)
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

func TestWalkBackSlashBetweenRates(t *testing.T) {
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(10990)},
			Shares:      map[string]decimal.Decimal{ledgerTestVal: decimal.RequireFromString("11001.001001001001001001")},
			Rates:       map[string]validatorRate{ledgerTestVal: rate(19980, 20000)},
		},
		// The delegation made in the slash block's transactions was not slashed.
		Events:  []ledgerEvent{ev(1, 60, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "1000uatom", "new_shares", "1001.001001001001001001")},
		Slashes: []ledgerSlash{{Height: 60, Validator: ledgerTestVal, Fraction: decimal.RequireFromString("0.001")}},
		Samples: map[string][]rateSample{ledgerTestVal: {{50, unitRate(20000)}}},
	}
	got, err := walkBack(in, []int64{70, 59})
	if err != nil {
		t.Fatal(err)
	}
	// The new shares were truncated, but the chain rounds their value to 18
	// places before rounding down, so the truncation does not show.
	intsEqual(t, "h70", got[70].Delegations, map[string]int64{ledgerTestVal: 10990})
	intsEqual(t, "h59", got[59].Delegations, map[string]int64{ledgerTestVal: 10000})
	if len(got[70].DelegationError)+len(got[59].DelegationError) > 0 {
		t.Fatalf("a slash that explains the recorded rates exactly leaves no error, got %v %v", got[70].DelegationError, got[59].DelegationError)
	}

	// Rounding by other delegators also raised the rate after the slash, so
	// between the records the slash's exact share of the change is unknown.
	in.Anchor.Rates[ledgerTestVal] = rate(999050, 1000000)
	in.Anchor.Shares[ledgerTestVal] = decimal.NewFromInt(100000)
	in.Anchor.Delegations[ledgerTestVal] = bi(99905)
	in.Events = nil
	got, err = walkBack(in, []int64{100, 55})
	if err != nil {
		t.Fatal(err)
	}
	if got[100].Delegations[ledgerTestVal].Int64() != 99905 || got[100].DelegationError[ledgerTestVal] != nil {
		t.Fatalf("the anchor is exact, got %v %v", got[100].Delegations, got[100].DelegationError)
	}
	d, bound := got[55].Delegations[ledgerTestVal], got[55].DelegationError[ledgerTestVal]
	if bound == nil || bound.Int64() < 1 || bound.Int64() > 5 || d.Int64() < 100000 || d.Int64() > 100005 {
		t.Fatalf("between records across a slash the delegation is bounded, got %v ± %v", d, bound)
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
	got, bounds := unbondingAt(in, 50)
	intsEqual(t, "unbonding", pruneZero(got), map[string]int64{ledgerTestVal: 100, ledgerTestVal2: 25})
	if len(bounds) != 0 {
		t.Fatalf("bounds = %v", bounds)
	}
	in.Slashes = []ledgerSlash{{Height: 75, Validator: ledgerTestVal, DoubleSign: true, Fraction: decimal.RequireFromString("0.05")}}
	if _, bounds = unbondingAt(in, 50); bounds[ledgerTestVal] == nil || bounds[ledgerTestVal].Int64() != 7 {
		t.Fatalf("an entry open across a double sign slash is bounded by the slashed share, got %v", bounds)
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

func TestApproximateLinesCarryTheirBound(t *testing.T) {
	s := &Server{native: DefaultNativeAsset, oracle: NewOracle(apiTestUnusedOrigin, "")}
	h := ledgerHoldings{
		Delegations:     map[string]*big.Int{ledgerTestVal: bi(500), ledgerTestVal2: bi(10)},
		DelegationError: map[string]*big.Int{ledgerTestVal: bi(3)},
		Unbonding:       map[string]*big.Int{ledgerTestVal2: bi(7)},
		UnbondingError:  map[string]*big.Int{ledgerTestVal2: bi(1)},
	}
	var hb HistoricalBalance
	s.fillBalanceLines("cosmoshub", nil, &hb, h, time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
	if hb.Status != balanceApproximate {
		t.Fatalf("status = %s", hb.Status)
	}
	for _, l := range hb.Lines {
		if l.Approximate != (l.MaxErrorBase != "") {
			t.Fatalf("an approximate line must give its bound and only then: %+v", l)
		}
	}
	h.DelegationError[ledgerTestVal] = new(big.Int).Add(balanceMaxError, bi(1))
	hb = HistoricalBalance{}
	s.fillBalanceLines("cosmoshub", nil, &hb, h, time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
	if hb.Status != balanceIncompleteS || len(hb.Lines) != 0 || !strings.Contains(hb.Reason, ledgerTestVal) {
		t.Fatalf("a bound over the limit must refuse the date, got %s %q %d lines", hb.Status, hb.Reason, len(hb.Lines))
	}
}

func TestRateBookToleratesPayoutRounding(t *testing.T) {
	in := ledgerInput{
		Anchor:  ledgerAnchor{Height: 100, Rates: map[string]validatorRate{ledgerTestVal: {Tokens: bi(44099567617), Shares: decimal.RequireFromString("44112799918.151163393115632111")}}},
		Samples: map[string][]rateSample{ledgerTestVal: {{50, validatorRate{Tokens: bi(44488811520), Shares: decimal.RequireFromString("44502160615.778051406490846151")}}}},
	}
	lo, hi, err := newRateBook(in).at(ledgerTestVal, 60, 60, 60)
	if err != nil || lo.GreaterThan(hi) {
		t.Fatalf("a fall of 1e-29 between records is payout rounding, got %s %s %v", lo, hi, err)
	}
	in.Samples[ledgerTestVal][0].Rate.Tokens = bi(44488811520 + 10)
	if _, _, err := newRateBook(in).at(ledgerTestVal, 60, 60, 60); err == nil {
		t.Fatal("a fall larger than rounding with no slash must be unknown")
	}
}

func TestChainTokensRoundsTo18PlacesFirst(t *testing.T) {
	for in, want := range map[string]int64{
		"51782136.9999999999999999996": 51782137,
		"51782136.999999999999999999":  51782136,
		"7.5":                          7,
	} {
		if got := chainTokens(decimal.RequireFromString(in)); got.IntPart() != want {
			t.Fatalf("chainTokens(%s) = %s, want %d", in, got, want)
		}
	}
}
