package taxapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// completeIncomeGz is the embedded income file marked complete. The embedded
// development file says blocks_counted 0 (its per-validator pass was partial);
// rows built from the tables are complete by construction, so the identity
// proofs compare against this copy.
func completeIncomeGz(t *testing.T) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(validatorIncomeGz))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["blocks_counted"] = doc["blocks_expected"]
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(out); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// useCompleteIncome swaps the embedded income file for its complete copy for
// one test.
func useCompleteIncome(t *testing.T) {
	t.Helper()
	orig := validatorIncomeGz
	validatorIncomeGz = completeIncomeGz(t)
	incomeOnce, incomeSet, incomeErr = sync.Once{}, nil, nil
	t.Cleanup(func() {
		validatorIncomeGz = orig
		incomeOnce, incomeSet, incomeErr = sync.Once{}, nil, nil
	})
}

// stakingRowsFromEmbedded writes the embedded files out as table rows, the
// inverse of buildStakingSet: the day rows carry the inflation columns, and
// each validator gets rows whose exact sums floor to the file's daily
// commission and monthly rewards.
func stakingRowsFromEmbedded(t *testing.T) (StakingState, []StakingDayRow, []StakingValidatorRow) {
	t.Helper()
	infl, err := loadInflation()
	if err != nil {
		t.Fatal(err)
	}
	inc, err := loadIncome()
	if err != nil {
		t.Fatal(err)
	}
	if inc.File.FirstDay != infl.Days[0].Date || len(inc.Days) != len(infl.Days) {
		t.Fatalf("embedded files cover different days")
	}
	asOf, err := time.Parse(time.RFC3339, infl.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	first, err := time.Parse(time.RFC3339, infl.FirstBlockTime)
	if err != nil {
		t.Fatal(err)
	}
	pidAt := map[int64][]string{}
	for pid, h := range infl.ProposalHeights {
		pidAt[h] = append(pidAt[h], pid)
	}
	var days []StakingDayRow
	for i, d := range infl.Days {
		var props [][]any
		var hs []int64
		for h := range pidAt {
			if h >= d.FirstHeight && h <= d.LastHeight {
				hs = append(hs, h)
			}
		}
		sort.Slice(hs, func(a, b int) bool { return hs[a] < hs[b] })
		for _, h := range hs {
			for _, pid := range pidAt[h] {
				props = append(props, []any{h, pid, "proposal_passed"})
			}
		}
		pj, _ := json.Marshal(props)
		if props == nil {
			pj = []byte("[]")
		}
		w := inc.File.Withdrawn[i]
		days = append(days, StakingDayRow{Day: d.Date, FirstHeight: d.FirstHeight, LastHeight: d.LastHeight, Blocks: d.Blocks,
			Minted: d.Minted.String(), CommunityPool: d.CommunityPool.String(), Stakers: d.Stakers.String(),
			Commission: d.Commission.String(), RewardsEvents: d.RewardsEvents.String(), CommissionEvents: d.CommissionEvents.String(),
			SupplyStart: d.SupplyStart.String(), InflationStart: "0.1", InflationAvg: strconv.FormatFloat(d.InflationAvg, 'f', -1, 64),
			BondedAvg: strconv.FormatFloat(d.BondedAvg, 'f', -1, 64), CommunityTax: d.CommunityTax,
			WithdrawnCommission: strconv.FormatInt(w[0], 10), WithdrawnRewards: strconv.FormatInt(w[1], 10), Proposals: string(pj)})
	}
	type cell struct{ r, c int64 }
	byDay := make([]map[string]cell, len(inc.Days))
	for _, v := range inc.Validators {
		need := map[int]bool{v.First: true, v.First + len(v.Commission) - 1: true}
		firstOfMonth := map[string]int{}
		for j, c := range v.Commission {
			if c != 0 {
				need[v.First+j] = true
			}
			m := inc.Days[v.First+j][:7]
			if _, ok := firstOfMonth[m]; !ok {
				firstOfMonth[m] = v.First + j
			}
		}
		for m := range v.Rewards {
			need[firstOfMonth[m]] = true
		}
		paid := map[string]bool{}
		for j := v.First; j < v.First+len(v.Commission); j++ {
			if !need[j] {
				continue
			}
			m := inc.Days[j][:7]
			if _, ok := v.Rewards[m]; !ok {
				t.Fatalf("%s has a row in %s with no monthly rewards", v.Operator, m)
			}
			c := cell{c: v.Commission[j-v.First]}
			if !paid[m] {
				c.r, paid[m] = v.Rewards[m], true
			}
			if byDay[j] == nil {
				byDay[j] = map[string]cell{}
			}
			byDay[j][v.Operator] = c
		}
	}
	var vals []StakingValidatorRow
	for j, m := range byDay {
		ops := make([]string, 0, len(m))
		for op := range m {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			vals = append(vals, StakingValidatorRow{Day: inc.Days[j], Operator: op,
				Rewards: strconv.FormatInt(m[op].r, 10), Commission: strconv.FormatInt(m[op].c, 10)})
		}
	}
	st := StakingState{Chain: infl.Chain, FirstDay: days[0].Day, LastDay: days[len(days)-1].Day, FirstHeight: infl.FirstHeight,
		LastHeight: infl.LastHeight, FirstBlockTime: first, UpdatedAt: asOf, Source: "test", Version: StakingTablesVersion}
	return st, days, vals
}

func stubTables(st StakingState, days []StakingDayRow, vals []StakingValidatorRow, loads *int) *stakingTables {
	now := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	t := &stakingTables{now: func() time.Time { return now }}
	t.load = func(_ context.Context, rows bool) (StakingState, bool, []StakingDayRow, []StakingValidatorRow, error) {
		if loads != nil && rows {
			*loads++
		}
		if !rows {
			return st, true, nil, nil, nil
		}
		return st, true, days, vals, nil
	}
	return t
}

var stakingPaths = []string{
	"/staking/inflation", "/staking/inflation?view=yearly", "/staking/inflation?view=daily",
	"/staking/inflation?view=daily&from=2022-12-18&to=2022-12-20", "/staking/inflation?from=2025-05&to=2025-06",
	"/staking/validator-income", "/staking/validator-income?view=yearly", "/staking/validator-income?from=2023-11&to=2024-02",
	"/staking/validator-income/break-even?atom_usd=10&cost_usd=500",
	"/staking/validator-income/break-even?atom_usd=4.5&cost_usd=1200&month=2025-05",
	"/staking/inflation?view=weekly", "/staking/validator-income?view=daily",
}

func serveBodies(t *testing.T, s *Server) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range stakingPaths {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		out[p] = fmt.Sprintf("%d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	return out
}

func sameBodies(t *testing.T, want, got map[string]string) {
	t.Helper()
	for p, w := range want {
		if got[p] != w {
			n := len(w)
			if len(got[p]) < n {
				n = len(got[p])
			}
			i := 0
			for i < n && w[i] == got[p][i] {
				i++
			}
			lo := i - 80
			if lo < 0 {
				lo = 0
			}
			t.Fatalf("%s differs at byte %d:\nembedded %q\ntables   %q", p, i, w[lo:min(len(w), i+80)], got[p][lo:min(len(got[p]), i+80)])
		}
	}
}

// The tables built from the same data give exactly the structures the
// embedded files parse into.
func TestStakingTablesBuildMatchesEmbedded(t *testing.T) {
	useCompleteIncome(t)
	st, days, vals := stakingRowsFromEmbedded(t)
	embInfl, _ := loadInflation()
	embInc, _ := loadIncome()
	set, err := buildStakingSet(st, days, vals, embInfl, embInc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(set.infl, embInfl) {
		t.Fatalf("inflation data differs")
	}
	if !reflect.DeepEqual(set.inc.File, embInc.File) {
		t.Fatalf("income file differs")
	}
	if !reflect.DeepEqual(set.inc, embInc) {
		t.Fatalf("income data differs")
	}
}

// Every staking handler answers byte for byte the same from the tables as
// from the embedded files.
func TestStakingHandlersSameFromTables(t *testing.T) {
	useCompleteIncome(t)
	st, days, vals := stakingRowsFromEmbedded(t)
	want := serveBodies(t, &Server{})
	loads := 0
	got := serveBodies(t, &Server{staking: stubTables(st, days, vals, &loads)})
	sameBodies(t, want, got)
	if loads != 1 {
		t.Fatalf("rows loaded %d times within the cache TTL", loads)
	}
}

// The tables are served only when they cover the embedded files; otherwise
// the embedded files are.
func TestStakingTablesFallBack(t *testing.T) {
	useCompleteIncome(t)
	st, days, vals := stakingRowsFromEmbedded(t)
	want := serveBodies(t, &Server{})
	embInfl, _ := loadInflation()
	embInc, _ := loadIncome()
	bump := func(s StakingState) StakingState { s.UpdatedAt = s.UpdatedAt.Add(time.Hour); return s }
	cases := map[string]func() (StakingState, []StakingDayRow, []StakingValidatorRow){
		"old version": func() (StakingState, []StakingDayRow, []StakingValidatorRow) {
			s := bump(st)
			s.Version = 0
			return s, days, vals
		},
		"ends early": func() (StakingState, []StakingDayRow, []StakingValidatorRow) {
			s := bump(st)
			s.LastDay, s.LastHeight = days[len(days)-2].Day, days[len(days)-2].LastHeight
			return s, days[:len(days)-1], vals
		},
		"starts late": func() (StakingState, []StakingDayRow, []StakingValidatorRow) {
			s := bump(st)
			s.FirstDay, s.FirstHeight = days[1].Day, days[1].FirstHeight
			return s, days[1:], vals
		},
		"missing day": func() (StakingState, []StakingDayRow, []StakingValidatorRow) {
			d := append(append([]StakingDayRow{}, days[:100]...), days[101:]...)
			return bump(st), d, vals
		},
		"height gap": func() (StakingState, []StakingDayRow, []StakingValidatorRow) {
			d := append([]StakingDayRow{}, days...)
			d[50].FirstHeight++
			d[50].Blocks--
			return bump(st), d, vals
		},
	}
	for name, mk := range cases {
		s, d, v := mk()
		if set, _, err := (&stakingTables{load: stubTables(s, d, v, nil).load}).refresh(context.Background()); err == nil || set != nil {
			t.Fatalf("%s: tables used", name)
		}
		got := serveBodies(t, &Server{staking: stubTables(s, d, v, nil)})
		sameBodies(t, want, got)
	}
	if err := stakingStateUsable(st, embInfl, embInc); err != nil {
		t.Fatalf("the full tables are usable: %v", err)
	}
}

// The cache checks the state at most every ten minutes and reloads rows only
// when updated_at moved.
func TestStakingTablesCache(t *testing.T) {
	useCompleteIncome(t)
	st, days, vals := stakingRowsFromEmbedded(t)
	now := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	stateReads, rowReads := 0, 0
	cur := st
	tb := &stakingTables{now: func() time.Time { return now }}
	tb.load = func(_ context.Context, rows bool) (StakingState, bool, []StakingDayRow, []StakingValidatorRow, error) {
		if !rows {
			stateReads++
			return cur, true, nil, nil, nil
		}
		rowReads++
		return cur, true, days, vals, nil
	}
	if tb.current(context.Background()) == nil {
		t.Fatal("tables not used")
	}
	now = now.Add(9 * time.Minute)
	tb.current(context.Background())
	if stateReads != 1 || rowReads != 1 {
		t.Fatalf("within the TTL: %d state reads, %d row reads", stateReads, rowReads)
	}
	now = now.Add(2 * time.Minute)
	tb.current(context.Background())
	if stateReads != 2 || rowReads != 1 {
		t.Fatalf("same updated_at: %d state reads, %d row reads", stateReads, rowReads)
	}
	cur.UpdatedAt = cur.UpdatedAt.Add(24 * time.Hour)
	now = now.Add(11 * time.Minute)
	set := tb.current(context.Background())
	if stateReads != 3 || rowReads != 2 || set == nil || set.infl.AsOf != cur.UpdatedAt.UTC().Format(time.RFC3339) {
		t.Fatalf("after updated_at moved: %d state reads, %d row reads", stateReads, rowReads)
	}
	// A failed read keeps the last good tables.
	tb.load = func(context.Context, bool) (StakingState, bool, []StakingDayRow, []StakingValidatorRow, error) {
		return StakingState{}, false, nil, nil, fmt.Errorf("connection refused")
	}
	now = now.Add(11 * time.Minute)
	if tb.current(context.Background()) != set {
		t.Fatal("a failed read dropped the last good tables")
	}
}

func TestFix18(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "5.": "5", ".5": "0.5", "1.230000000000000000": "1.23",
		"23190967499.979998625158886778": "23190967499.979998625158886778", "-0.000000000000000001": "-0.000000000000000001"} {
		v, err := ParseFix18(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := Fix18String(v); got != want {
			t.Fatalf("%s: got %s want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", ".", "1.2.3", "1e5", "0.0000000000000000001", "abc", "-"} {
		if _, err := ParseFix18(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	v, _ := ParseFix18("2.999999999999999999")
	if Fix18Floor(v).Int64() != 2 {
		t.Fatal("floor")
	}
}
