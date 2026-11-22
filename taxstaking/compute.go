package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

// dayRange is one UTC day's heights.
type dayRange struct {
	Day         string
	First, Last int64
}

// planDays splits the chain from start (the first block of day) into whole
// UTC days that end before until. A day is whole once the node holds a block
// at or after the next midnight. It refuses a start that is not the first
// block of its day and a day without blocks.
func planDays(ctx context.Context, n *node, start int64, day string, until time.Time, latest int64) ([]dayRange, error) {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return nil, err
	}
	t0, err := n.blockTime(ctx, start)
	if err != nil {
		return nil, err
	}
	if t0.Before(d) || !t0.Before(d.AddDate(0, 0, 1)) {
		return nil, fmt.Errorf("height %d is at %s, not on %s", start, t0.Format(time.RFC3339), day)
	}
	if prev, err := n.blockTime(ctx, start-1); err == nil && !prev.Before(d) {
		return nil, fmt.Errorf("height %d is not the first block of %s", start, day)
	} else if err != nil && !errors.Is(err, errMissing) {
		return nil, err
	}
	var out []dayRange
	lo := start
	for next := d.AddDate(0, 0, 1); !next.After(until); next = next.AddDate(0, 0, 1) {
		h, err := n.firstHeightAtOrAfter(ctx, next, lo, latest)
		if err != nil {
			return nil, err
		}
		if h > latest {
			break // the node has no block after this midnight yet
		}
		if h == lo {
			return nil, fmt.Errorf("no block on %s", d.Format(time.DateOnly))
		}
		out = append(out, dayRange{Day: d.Format(time.DateOnly), First: lo, Last: h - 1})
		lo, d = h, next
	}
	return out, nil
}

// rangesFromBounds reads day starts from a bounds file (boundaries.py
// output) for parse-only runs over heights the node no longer holds.
func dayOfFunc(starts []int64, days []string) func(int64) string {
	return func(h int64) string {
		i := sort.Search(len(starts), func(i int) bool { return starts[i] > h }) - 1
		if i < 0 {
			return "before"
		}
		return days[i]
	}
}

// taxStep: blocks from From on split their mint at Rate (until the next step).
type taxStep struct {
	From int64
	Rate *big.Int
}

// taxSchedule finds the community tax each block lo..hi split its mint at.
// Block h used the rate in the state after block h-1. A governance change
// executes at the end of its tally block, so this holds across it; a change
// made in a begin block (an upgrade handler) would apply one block earlier,
// so a change at a height without a passed proposal is refused for review.
func taxSchedule(ctx context.Context, n *node, lo, hi int64, passed map[int64]bool) ([]taxStep, error) {
	pts := map[int64]bool{lo - 1: true, hi - 1: true}
	for p := range passed {
		if p-1 >= lo-1 && p <= hi-1 {
			pts[p-1], pts[p] = true, true
		}
	}
	var xs []int64
	for x := range pts {
		xs = append(xs, x)
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	val := func(x int64) (*big.Int, error) { return n.communityTax(ctx, x) }
	var changes []int64
	var find func(a, b int64) error
	find = func(a, b int64) error {
		va, err := val(a)
		if err != nil {
			return err
		}
		vb, err := val(b)
		if err != nil {
			return err
		}
		if va.Cmp(vb) == 0 {
			return nil
		}
		if b == a+1 {
			changes = append(changes, b)
			return nil
		}
		m := a + (b-a)/2
		if err := find(a, m); err != nil {
			return err
		}
		return find(m, b)
	}
	for i := 0; i+1 < len(xs); i++ {
		if err := find(xs[i], xs[i+1]); err != nil {
			return nil, err
		}
	}
	r0, err := val(lo - 1)
	if err != nil {
		return nil, err
	}
	steps := []taxStep{{From: lo, Rate: r0}}
	sort.Slice(changes, func(i, j int) bool { return changes[i] < changes[j] })
	for _, q := range changes {
		if !passed[q] {
			return nil, fmt.Errorf("community tax changed in block %d without a passed proposal there; needs review", q)
		}
		r, err := val(q)
		if err != nil {
			return nil, err
		}
		steps = append(steps, taxStep{From: q + 1, Rate: r})
	}
	return steps, nil
}

func rateAt(steps []taxStep, h int64) *big.Int {
	r := steps[0].Rate
	for _, s := range steps {
		if h >= s.From {
			r = s.Rate
		}
	}
	return r
}

type unbondRow struct {
	CompletionDay string
	Amount        string
}

// computedDay is one day ready to write.
type computedDay struct {
	Range  dayRange
	Agg    *dayAgg
	Row    taxapi.StakingDayRow
	Vals   []taxapi.StakingValidatorRow
	Unbond []unbondRow
}

func passedHeights(d *dayAgg) map[int64]bool {
	m := map[int64]bool{}
	for _, p := range d.Proposals {
		if p.passed() {
			m[p.Height] = true
		}
	}
	return m
}

// dayProblems lists why a day cannot be written.
func dayProblems(r dayRange, d *dayAgg) []string {
	var out []string
	if d == nil {
		return []string{"no blocks read"}
	}
	if len(d.Missing) > 0 {
		out = append(out, fmt.Sprintf("missing=%d first=%d", len(d.Missing), d.Missing[0]))
	}
	if len(d.Bad) > 0 {
		out = append(out, fmt.Sprintf("unreadable=%d first=%d (%s)", len(d.Bad), d.Bad[0].Height, d.Bad[0].Err))
	}
	if len(d.NoMint) > 0 {
		out = append(out, fmt.Sprintf("no_mint=%d first=%d", len(d.NoMint), d.NoMint[0]))
	}
	if d.Blocks != r.Last-r.First+1 && len(out) == 0 {
		out = append(out, fmt.Sprintf("blocks=%d want=%d", d.Blocks, r.Last-r.First+1))
	}
	if _, ok := d.Vals["?"]; ok {
		out = append(out, "rewards or commission event without a validator")
	}
	if _, ok := d.Unbond["?"]; ok {
		out = append(out, "unbond without a completion time")
	}
	for k := range d.Unbond {
		if _, err := time.Parse(time.DateOnly, k); k != "?" && err != nil {
			out = append(out, fmt.Sprintf("unbond completion %q", trim(k)))
		}
	}
	if d.First == nil {
		out = append(out, "no mint")
	}
	return out
}

// buildDay turns a day's exact sums into its table rows, with the formulas
// of the merge (merge.py, 60 digit Decimal).
func buildDay(r dayRange, d *dayAgg, steps []taxStep) (computedDay, error) {
	minted := new(big.Int).Set(d.Minted)
	poolFix := new(big.Int)
	for h, amt := range d.MintAt {
		poolFix.Add(poolFix, new(big.Int).Mul(amt, rateAt(steps, h)))
	}
	pool := fixRat(poolFix)
	poolInt := roundHalfEven(pool)
	stakers := new(big.Int).Sub(minted, poolInt)
	toStakers := roundSig(new(big.Rat).Sub(new(big.Rat).SetInt(minted), pool), decimalPrec)
	comShare := new(big.Rat)
	if d.Rewards.Sign() != 0 {
		comShare = decDiv(fixRat(d.Commission), fixRat(d.Rewards))
	}
	commission := roundHalfEven(decMul(toStakers, comShare))
	infl, err := parseFix(d.First.Inflation)
	if err != nil {
		return computedDay{}, err
	}
	ap, err := parseFix(d.First.Provision)
	if err != nil {
		return computedDay{}, err
	}
	if infl.Sign() == 0 {
		return computedDay{}, fmt.Errorf("%s: zero inflation at %d", r.Day, d.First.Height)
	}
	supply := roundHalfEven(decDiv(fixRat(ap), fixRat(infl)))
	var props [][3]any
	for _, p := range d.Proposals {
		if p.passed() {
			props = append(props, [3]any{p.Height, p.ID, p.Result})
		}
	}
	if props == nil {
		props = [][3]any{}
	}
	pj, err := json.Marshal(props)
	if err != nil {
		return computedDay{}, err
	}
	row := taxapi.StakingDayRow{
		Day: r.Day, FirstHeight: r.First, LastHeight: r.Last, Blocks: d.Blocks,
		Minted:              minted.String(),
		CommunityPool:       poolInt.String(),
		Stakers:             stakers.String(),
		Commission:          commission.String(),
		RewardsEvents:       fixTrunc(d.Rewards).String(),
		CommissionEvents:    fixTrunc(d.Commission).String(),
		SupplyStart:         supply.String(),
		InflationStart:      d.First.Inflation,
		InflationAvg:        meanPlaces(d.InflSum, d.Blocks, 8),
		BondedAvg:           meanPlaces(d.BondedSum, d.Blocks, 8),
		CommunityTax:        taxString(rateAt(steps, r.First)),
		WithdrawnCommission: fixString(d.WithdrawC),
		WithdrawnRewards:    fixString(d.WithdrawR),
		Proposals:           string(pj),
	}
	out := computedDay{Range: r, Agg: d, Row: row}
	ops := make([]string, 0, len(d.Vals))
	for op := range d.Vals {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		v := d.Vals[op]
		out.Vals = append(out.Vals, taxapi.StakingValidatorRow{Day: r.Day, Operator: op, Rewards: fixString(v[0]), Commission: fixString(v[1])})
	}
	keys := make([]string, 0, len(d.Unbond))
	for k := range d.Unbond {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Unbond = append(out.Unbond, unbondRow{CompletionDay: k, Amount: fixString(d.Unbond[k])})
	}
	return out, nil
}

// progress reports each computed day (stderr in -compute and -check runs).
var progress = func(string, ...any) {}

// computeDays reads and builds each day. A day with any problem is an error
// naming it; strict=false returns what it could (for -compute reports).
func computeDays(ctx context.Context, n *node, days []dayRange, workers int, strict bool) ([]computedDay, error) {
	var out []computedDay
	for _, r := range days {
		r := r
		began := time.Now()
		aggs, err := aggregate(ctx, n, r.First, r.Last, func(int64) string { return r.Day }, workers)
		if err != nil {
			return out, err
		}
		d := aggs[r.Day]
		if p := dayProblems(r, d); len(p) > 0 {
			if strict {
				return out, fmt.Errorf("day=%s heights=%d-%d refused: %s", r.Day, r.First, r.Last, strings.Join(p, "; "))
			}
			out = append(out, computedDay{Range: r, Agg: d})
			continue
		}
		steps, err := taxSchedule(ctx, n, r.First, r.Last, passedHeights(d))
		if err != nil {
			return out, fmt.Errorf("day=%s: %v", r.Day, err)
		}
		c, err := buildDay(r, d, steps)
		if err != nil {
			return out, err
		}
		progress("day=%s heights=%d-%d blocks=%d validators=%d read_secs=%d", r.Day, r.First, r.Last, d.Blocks, len(c.Vals),
			int(time.Since(began).Seconds()))
		out = append(out, c)
	}
	return out, nil
}
