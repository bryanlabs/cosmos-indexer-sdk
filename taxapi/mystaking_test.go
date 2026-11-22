package taxapi

import (
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func msd(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func myStakingFixture() [][]Row {
	day := func(s string) time.Time { t, _ := time.Parse(apiDateLayout, s); return t }
	return [][]Row{
		{
			{Time: day("2025-01-05"), Category: apiReward, Symbol: "ATOM", Amount: msd("1"), ValueUSD: msd("6"), TxHash: "A1", ValidatorAddress: "valA"},
			{Time: day("2025-01-10"), Category: apiReward, Symbol: "ATOM", Amount: msd("2"), ValueUSD: msd("12"), TxHash: "A2", ValidatorAddress: "valA"},
			{Time: day("2025-01-10"), Category: apiReward, Symbol: "ATOM", Amount: msd("3"), ValueUSD: msd("18"), TxHash: "B1", ValidatorAddress: "valB"},
			{Time: day("2025-01-11"), Category: apiReward, Symbol: "NTRN", Amount: msd("5"), PriceMissing: true, TxHash: "B2", ValidatorAddress: "valB"},
			{Time: day("2025-01-12"), Category: apiCommission, Symbol: "ATOM", Amount: msd("4"), ValueUSD: msd("24"), TxHash: "C1", ValidatorAddress: "valB"},
			{Time: day("2025-01-13"), Category: apiReward, Symbol: "ATOM", Amount: msd("1"), ValueUSD: msd("6"), TxHash: "U1"},
			{Time: day("2025-01-14"), Category: "transfer", Direction: "in", UserTag: TagIncome, Symbol: "ATOM", Amount: msd("7"), ValueUSD: msd("42"), TxHash: "T1"},
			{Time: day("2025-01-15"), Category: "transfer", Direction: "in", Symbol: "ATOM", Amount: msd("100"), ValueUSD: msd("600"), TxHash: "X1"},
			{Time: day("2025-01-16"), Category: apiReward, Symbol: "ATOM", Amount: msd("9"), ValueUSD: msd("54"), TxHash: "S1", ValidatorAddress: "valA", UserTag: TagSpam},
			{Time: day("2025-01-17"), Category: apiReward, Symbol: "ATOM", Amount: msd("9"), ValueUSD: msd("54"), TxHash: "E1", ValidatorAddress: "valA", Excluded: true},
		},
		{
			{Time: day("2025-01-08"), Category: apiReward, Symbol: "ATOM", Amount: msd("2"), ValueUSD: msd("12"), TxHash: "Z1", ValidatorAddress: "valA"},
		},
	}
}

// The validators plus the other buckets must add up to the income report's
// own total for the same rows: same filter, nothing dropped or added.
func TestMyStakingAddsUpToIncome(t *testing.T) {
	rows := myStakingFixture()
	start, _ := time.Parse(apiDateLayout, "2025-01-01")
	end, _ := time.Parse(apiDateLayout, "2025-02-01")
	out := buildMyStaking(apiMainnet, start, end, rows)
	want := decimal.Zero
	for _, rs := range rows {
		for _, r := range rs {
			if !r.Excluded && isIncomeRow(r) {
				want = want.Add(r.ValueUSD)
			}
		}
	}
	rec := out.Reconciliation
	if rec["income_report_total_usd"] != want.String() || rec["difference_usd"] != "0" {
		t.Fatalf("reconciliation %v, want total %s and no difference", rec, want)
	}
	if len(out.Validators) != 2 || out.Validators[0].Operator != "valA" && out.Validators[0].Operator != "valB" {
		t.Fatalf("validators %+v", out.Validators)
	}
	by := map[string]myStakingValidator{}
	for _, v := range out.Validators {
		by[v.Operator] = v
	}
	a, b := by["valA"], by["valB"]
	if a.RewardsUSD != "30" || a.RewardRows != 3 || len(a.Transactions) != 3 {
		t.Fatalf("valA %+v", a)
	}
	if b.RewardsUSD != "18" || b.OwnCommissionUSD != "24" || b.IncomeUSD != "42" || b.PriceMissingRows != 1 {
		t.Fatalf("valB %+v", b)
	}
	if len(b.Rewards) != 2 || b.Rewards[0].Symbol != "ATOM" || b.Rewards[1].Symbol != "NTRN" {
		t.Fatalf("valB rewards %+v", b.Rewards)
	}
	kinds := map[string]string{}
	for _, o := range out.Other {
		kinds[o.Kind] = o.ValueUSD
	}
	if kinds["reward_unattributed"] != "6" || len(kinds) != 2 {
		t.Fatalf("other %+v", out.Other)
	}
	if out.Slashing["status"] != "hidden" || a.Slashed != nil {
		t.Fatal("slashing must stay hidden, not zero")
	}
}

// Commission paid uses the rate over the days the claimed rewards built up
// in, per wallet: the first claim in the period takes that day's rate.
func TestMyStakingCommissionEstimate(t *testing.T) {
	rows := myStakingFixture()
	out := buildMyStaking(apiMainnet, time.Time{}, time.Now(), rows)
	days := map[string]map[string][2]decimal.Decimal{
		"valA": {
			"2025-01-05": {msd("100"), msd("10")}, // 10%
			"2025-01-06": {msd("100"), msd("50")}, // 50%
			"2025-01-08": {msd("100"), msd("5")},
			"2025-01-10": {msd("100"), msd("50")},
		},
		"valB": {"2025-01-10": {msd("100"), msd("20")}},
	}
	applyCommissionEstimate(&out, days)
	for _, v := range out.Validators {
		switch v.Operator {
		case "valA":
			// wallet 0: 01-05 first row at day rate 10/90 -> 0.111111;
			// 01-10 window (01-05, 01-10] = 300 rewards, 105 commission -> 2 * 105/195.
			// wallet 1: 01-08 first row at day rate 5/95.
			got := map[string]string{}
			for _, tx := range v.Transactions {
				got[tx.TxHash] = tx.CommissionPaid
			}
			if got["A1"] != "0.111111" || got["A2"] != "1.076923" || got["Z1"] != "0.105263" {
				t.Fatalf("valA commission %v", got)
			}
			if v.CommissionDayRate != 2 {
				t.Fatalf("valA day-rate rows %d", v.CommissionDayRate)
			}
		case "valB":
			// B1 day rate 20/80 = 0.25 -> 0.75 ATOM; B2 has no data on 01-11.
			if v.CommissionPaid[0].Amount != "0.75" || v.CommissionNoData != 1 {
				t.Fatalf("valB %+v", v)
			}
		}
	}
}

func TestMyStakingCommissionNotice(t *testing.T) {
	out := myStakingResponse{Validators: []myStakingValidator{{Operator: "valA"}, {Operator: "valB"}, {Operator: "valC"}}}
	changes := map[string][]myStakingCommissionChange{
		"valA": {{Date: "2024-01-01", To: "0.05"}, {Date: "2025-01-20", Kind: "edit", From: "0.05", To: "0.1"}, {Date: "2025-03-01", Kind: "edit", From: "0.1", To: "0.2"}},
		"valB": {{Date: "2025-01-25", Kind: "create", To: "0.05"}},
		"valC": {{Date: "2025-01-26", Kind: "edit", To: "0.07"}},
	}
	end, _ := time.Parse(apiDateLayout, "2025-02-01")
	applyCommissionChanges(&out, changes, end)
	a, b := out.Validators[0], out.Validators[1]
	if len(a.CommissionChanges) != 2 || a.CommissionRateNow != "0.1" || a.CommissionNotice == nil || a.CommissionNotice.From != "0.05" {
		t.Fatalf("valA %+v", a)
	}
	if b.CommissionNotice != nil || b.CommissionRateNow != "0.05" {
		t.Fatalf("valB %+v", b)
	}
	if c := out.Validators[2]; c.CommissionNotice == nil || c.CommissionNotice.From != "" {
		t.Fatalf("a genesis validator's first edit still raises the notice: %+v", c)
	}
}

func TestVoteOptionLabel(t *testing.T) {
	for raw, want := range map[string]string{
		`option:VOTE_OPTION_YES weight:"1.000000000000000000"`:                                                          "yes",
		`option:VOTE_OPTION_NO_WITH_VETO weight:"1.000000000000000000"`:                                                 "no_with_veto",
		`[{"option":3,"weight":"1.000000000000000000"}]`:                                                                "no",
		`{"option":2,"weight":"1.000000000000000000"}`:                                                                  "abstain",
		`[{"option":1,"weight":"0.5"},{"option":3,"weight":"0.5"}]`:                                                     "weighted",
		"option:VOTE_OPTION_YES weight:\"0.500000000000000000\"\noption:VOTE_OPTION_NO weight:\"0.500000000000000000\"": "weighted",
		"VOTE_OPTION_ABSTAIN": "abstain",
		"Yes":                 "yes",
		"":                    "unknown",
		"garbage":             "unknown",
	} {
		if got := voteOptionLabel(raw); got != want {
			t.Errorf("%q: got %s, want %s", raw, got, want)
		}
	}
}

func TestLastVotesBeforeVotingEnd(t *testing.T) {
	endP := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	votes := []castVote{
		{Account: "a", ProposalID: "1", Option: "no", Height: 10, Time: endP.Add(-48 * time.Hour)},
		{Account: "a", ProposalID: "1", Option: "yes", Height: 20, Time: endP.Add(-time.Hour)},
		{Account: "a", ProposalID: "1", Option: "abstain", Height: 30, Time: endP.Add(time.Hour)},
		{Account: "b", ProposalID: "2", Option: "yes", Height: 5, Time: endP},
	}
	got := lastVotes(votes, map[string]time.Time{"1": endP})
	if got["a"]["1"].Option != "yes" || len(got["b"]) != 0 {
		t.Fatalf("%+v", got)
	}
}

// TestMyStakingLiveReadOnly runs the route's own reads against the live
// index through a read-only connection. It is skipped unless
// MYSTAKING_LIVE_DSN names one; MYSTAKING_LIVE_ACCOUNT and
// MYSTAKING_LIVE_OPERATOR pick the validator to read.
func TestMyStakingLiveReadOnly(t *testing.T) {
	dsn := os.Getenv("MYSTAKING_LIVE_DSN")
	if dsn == "" {
		t.Skip("MYSTAKING_LIVE_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn+" default_transaction_read_only=on statement_timeout=30000"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var ro string
	if err := db.Raw("SHOW default_transaction_read_only").Scan(&ro).Error; err != nil || ro != "on" {
		t.Fatalf("connection is not read only: %q %v", ro, err)
	}
	s := &Server{db: db}
	op, acct := os.Getenv("MYSTAKING_LIVE_OPERATOR"), os.Getenv("MYSTAKING_LIVE_ACCOUNT")
	if operatorAccount(op) != acct {
		t.Fatal("operator and account do not match")
	}
	t0 := time.Now()
	votes, err := s.votesBy([]string{acct})
	if err != nil {
		t.Fatal(err)
	}
	opts := map[string]int{}
	for _, v := range votes {
		opts[v.Option]++
	}
	t.Logf("votes %d in %s, options %v", len(votes), time.Since(t0).Round(time.Millisecond), opts)
	if len(votes) == 0 || opts["unknown"] > 0 {
		t.Fatal("expected decoded votes")
	}
	t0 = time.Now()
	changes, err := s.commissionChanges(apiMainnet)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("commission changes: %d validators, this one %d, in %s", len(changes), len(changes[op]), time.Since(t0).Round(time.Millisecond))
	if len(changes) < 100 {
		t.Fatal("expected commission history for most validators")
	}
	start, _ := time.Parse(apiDateLayout, "2025-01-01")
	end, _ := time.Parse(apiDateLayout, "2026-01-01")
	ys, err := readValidatorYield(db, apiMainnet, []string{op}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	y := ys[op]
	if y == nil || y.Days < 360 || y.APR <= 0 || y.APR > 0.5 || y.GrossAPR < y.APR {
		t.Fatalf("yield %+v", y)
	}
	t.Logf("yield 2025: apr %.4f gross %.4f rate %.4f days %d", y.APR, y.GrossAPR, y.RateAvg, y.Days)
	days, err := readValidatorDaysFor(db, StakingChain, []string{op}, "2025-01-01", "2025-12-31")
	if err != nil || len(days[op]) < 360 {
		t.Fatalf("days %d %v", len(days[op]), err)
	}
}
