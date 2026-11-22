package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
)

// snap is a block's mint: height, inflation, bonded_ratio, annual_provisions, amount.
type snap struct {
	Height    int64
	Inflation string
	Bonded    string
	Provision string
	Amount    *big.Int
}

type badBlock struct {
	Height int64
	Err    string
}

// dayAgg is one UTC day's sums (extract_v.run_unit with --by-validator),
// kept exact.
type dayAgg struct {
	Blocks     int64
	Minted     *big.Int
	Rewards    *big.Int
	Commission *big.Int
	First      *snap
	Last       *snap
	InflSum    *big.Int
	BondedSum  *big.Int
	Proposals  []proposal
	Vals       map[string]*[2]*big.Int
	WithdrawC  *big.Int
	WithdrawR  *big.Int
	Unbond     map[string]*big.Int
	// MintAt is each block's minted amount, for the community pool split.
	MintAt  map[int64]*big.Int
	Missing []int64
	NoMint  []int64
	Bad     []badBlock
}

func newDayAgg() *dayAgg {
	return &dayAgg{Minted: new(big.Int), Rewards: new(big.Int), Commission: new(big.Int), InflSum: new(big.Int),
		BondedSum: new(big.Int), Vals: map[string]*[2]*big.Int{}, WithdrawC: new(big.Int), WithdrawR: new(big.Int),
		Unbond: map[string]*big.Int{}, MintAt: map[int64]*big.Int{}}
}

func (d *dayAgg) problems() int { return len(d.Missing) + len(d.NoMint) + len(d.Bad) }

func addInto(m map[string]*big.Int, k string, v *big.Int) {
	if m[k] == nil {
		m[k] = new(big.Int)
	}
	m[k].Add(m[k], v)
}

// add counts one block. A block whose mint cannot be read counts as bad.
func (d *dayAgg) add(b *blockData) {
	var s *snap
	if b.Mint != nil {
		var err error
		s, err = mintSnap(b)
		if err != nil {
			d.Bad = append(d.Bad, badBlock{b.Height, err.Error()})
			return
		}
	}
	d.Blocks++
	d.Rewards.Add(d.Rewards, b.Rewards)
	d.Commission.Add(d.Commission, b.Commission)
	d.Proposals = append(d.Proposals, b.Proposals...)
	for op, v := range b.PerVal {
		t := d.Vals[op]
		if t == nil {
			t = &[2]*big.Int{new(big.Int), new(big.Int)}
			d.Vals[op] = t
		}
		t[0].Add(t[0], v[0])
		t[1].Add(t[1], v[1])
	}
	d.WithdrawC.Add(d.WithdrawC, b.WithdrawC)
	d.WithdrawR.Add(d.WithdrawR, b.WithdrawR)
	for k, v := range b.Unbond {
		addInto(d.Unbond, k, v)
	}
	if s == nil {
		d.NoMint = append(d.NoMint, b.Height)
		return
	}
	d.Minted.Add(d.Minted, s.Amount)
	d.MintAt[b.Height] = s.Amount
	if d.First == nil || s.Height < d.First.Height {
		d.First = s
	}
	if d.Last == nil || s.Height > d.Last.Height {
		d.Last = s
	}
	inf, _ := parseFix(s.Inflation)
	bon, _ := parseFix(s.Bonded)
	d.InflSum.Add(d.InflSum, inf)
	d.BondedSum.Add(d.BondedSum, bon)
}

func mintSnap(b *blockData) (*snap, error) {
	amt, ok := new(big.Int).SetString(b.Mint["amount"], 10)
	if !ok || amt.Sign() < 0 {
		return nil, fmt.Errorf("mint amount %q", b.Mint["amount"])
	}
	s := &snap{Height: b.Height, Inflation: b.Mint["inflation"], Bonded: b.Mint["bonded_ratio"], Provision: b.Mint["annual_provisions"], Amount: amt}
	for _, v := range []string{s.Inflation, s.Bonded, s.Provision} {
		if _, err := parseFix(v); err != nil {
			return nil, fmt.Errorf("mint attribute: %v", err)
		}
	}
	return s, nil
}

// merge folds another partial sum of the same day into d.
func (d *dayAgg) merge(o *dayAgg) {
	d.Blocks += o.Blocks
	d.Minted.Add(d.Minted, o.Minted)
	d.Rewards.Add(d.Rewards, o.Rewards)
	d.Commission.Add(d.Commission, o.Commission)
	d.InflSum.Add(d.InflSum, o.InflSum)
	d.BondedSum.Add(d.BondedSum, o.BondedSum)
	if o.First != nil && (d.First == nil || o.First.Height < d.First.Height) {
		d.First = o.First
	}
	if o.Last != nil && (d.Last == nil || o.Last.Height > d.Last.Height) {
		d.Last = o.Last
	}
	d.Proposals = append(d.Proposals, o.Proposals...)
	for op, v := range o.Vals {
		t := d.Vals[op]
		if t == nil {
			t = &[2]*big.Int{new(big.Int), new(big.Int)}
			d.Vals[op] = t
		}
		t[0].Add(t[0], v[0])
		t[1].Add(t[1], v[1])
	}
	d.WithdrawC.Add(d.WithdrawC, o.WithdrawC)
	d.WithdrawR.Add(d.WithdrawR, o.WithdrawR)
	for k, v := range o.Unbond {
		addInto(d.Unbond, k, v)
	}
	for h, v := range o.MintAt {
		d.MintAt[h] = v
	}
	d.Missing = append(d.Missing, o.Missing...)
	d.NoMint = append(d.NoMint, o.NoMint...)
	d.Bad = append(d.Bad, o.Bad...)
}

func (d *dayAgg) sortLists() {
	sort.SliceStable(d.Proposals, func(i, j int) bool { return d.Proposals[i].Height < d.Proposals[j].Height })
	sort.Slice(d.Missing, func(i, j int) bool { return d.Missing[i] < d.Missing[j] })
	sort.Slice(d.NoMint, func(i, j int) bool { return d.NoMint[i] < d.NoMint[j] })
	sort.Slice(d.Bad, func(i, j int) bool { return d.Bad[i].Height < d.Bad[j].Height })
}

// errMissing is a block the source does not hold.
var errMissing = errors.New("missing")

type blockSource interface {
	blockResults(ctx context.Context, h int64) ([]byte, error)
}

// aggregate reads heights from..to with workers in parallel and sums them
// per day. Blocks the source lacks, cannot parse or that lack a mint are
// listed on their day, never skipped silently.
func aggregate(ctx context.Context, src blockSource, from, to int64, dayOf func(int64) string, workers int) (map[string]*dayAgg, error) {
	if workers < 1 {
		workers = 1
	}
	heights := make(chan int64)
	parts := make([]map[string]*dayAgg, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		parts[w] = map[string]*dayAgg{}
		wg.Add(1)
		go func(part map[string]*dayAgg) {
			defer wg.Done()
			for h := range heights {
				k := dayOf(h)
				d := part[k]
				if d == nil {
					d = newDayAgg()
					part[k] = d
				}
				body, err := src.blockResults(ctx, h)
				if errors.Is(err, errMissing) {
					d.Missing = append(d.Missing, h)
					continue
				}
				if err != nil {
					d.Bad = append(d.Bad, badBlock{h, trim(err.Error())})
					continue
				}
				b, err := parseBlockResults(body, h)
				if err != nil {
					d.Bad = append(d.Bad, badBlock{h, trim(err.Error())})
					continue
				}
				d.add(b)
			}
		}(parts[w])
	}
	var err error
feed:
	for h := from; h <= to; h++ {
		select {
		case heights <- h:
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(heights)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	out := map[string]*dayAgg{}
	for _, p := range parts {
		for k, d := range p {
			if out[k] == nil {
				out[k] = newDayAgg()
			}
			out[k].merge(d)
		}
	}
	for _, d := range out {
		d.sortLists()
	}
	return out, nil
}

func trim(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
