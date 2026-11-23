package taxapi

import (
	"math/big"
	"math/bits"
	"sort"
)

// maxUnnamedPerBlock bounds the unmatched staking records of one block whose
// combinations settleUnnamedStakes tries; a block with more stays refused.
const maxUnnamedPerBlock = 12

// settleUnnamedStakes settles from chain state the staking records that do
// not name their delegator and that attributeStakes could not give to one
// delegator. unmatched holds each such record in the form it takes as the
// wallet's. For each block H holding them, newest first, the wallet's
// delegations at the end of H-1 and of H are read with delegationsAt. The
// change the wallet's own named staking events in H account for is taken
// off, and what is left must be explained by exactly one combination of the
// unmatched records: none of them (they are someone else's, and are dropped)
// or a single one (it is the wallet's and is returned as its event). Any
// other outcome, a read that fails, or no read at all leaves H unattributed,
// and H is returned so that every date before it is refused as before.
func settleUnnamedStakes(delegationsAt func(height int64) (map[string]*big.Int, error), addr, operator, bondDenom string, events, unmatched []ledgerEvent) ([]ledgerEvent, int64) {
	if len(unmatched) == 0 {
		return nil, 0
	}
	byHeight := map[int64][]ledgerEvent{}
	for _, ev := range unmatched {
		byHeight[ev.Height] = append(byHeight[ev.Height], ev)
	}
	heights := make([]int64, 0, len(byHeight))
	for h := range byHeight {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] > heights[j] })
	named := map[int64][]ledgerEvent{}
	for _, ev := range events {
		if _, ok := byHeight[ev.Height]; ok {
			named[ev.Height] = append(named[ev.Height], ev)
		}
	}
	var owned []ledgerEvent
	for _, h := range heights {
		got, ok := settleBlock(delegationsAt, addr, operator, bondDenom, h, named[h], byHeight[h])
		if !ok {
			return owned, h
		}
		owned = append(owned, got...)
	}
	return owned, 0
}

// settleBlock settles the unmatched records cands of block h, given the
// wallet's named events in h. It returns the records that are the wallet's
// (none or one) and whether h is settled.
func settleBlock(delegationsAt func(height int64) (map[string]*big.Int, error), addr, operator, bondDenom string, h int64, named, cands []ledgerEvent) ([]ledgerEvent, bool) {
	if delegationsAt == nil || len(cands) > maxUnnamedPerBlock {
		return nil, false
	}
	before, err := delegationsAt(h - 1)
	if err != nil {
		return nil, false
	}
	after, err := delegationsAt(h)
	if err != nil {
		return nil, false
	}
	// left is the change in the wallet's delegations over block h that its
	// named events do not account for; touched counts the events that moved
	// each validator, each worth a token or two of rounding.
	left, touched := map[string]*big.Int{}, map[string]int{}
	for v, d := range after {
		addInt(left, v, d)
	}
	for v, d := range before {
		subInt(left, v, d)
	}
	for _, ev := range named {
		for _, rec := range ev.records() {
			eff, mine, ok := stakeEffect(ev.Type, rec, addr, operator, bondDenom)
			if !ok {
				return nil, false
			}
			if !mine {
				continue
			}
			for v, d := range eff {
				subInt(left, v, d)
				touched[v]++
			}
		}
	}
	effects := make([]map[string]*big.Int, len(cands))
	for i, c := range cands {
		recs := c.records()
		if len(recs) != 1 {
			return nil, false
		}
		eff, mine, ok := stakeEffect(c.Type, recs[0], addr, operator, bondDenom)
		if !ok || !mine {
			return nil, false
		}
		effects[i] = eff
		for v := range eff {
			if _, ok := left[v]; !ok {
				left[v] = new(big.Int)
			}
		}
	}
	// Every combination of the records is tried; exactly one may explain
	// what is left on every validator.
	fits, fit := 0, uint(0)
	for mask := uint(0); mask < 1<<len(cands); mask++ {
		if explains(left, touched, effects, mask) {
			fits++
			fit = mask
		}
	}
	if fits != 1 || bits.OnesCount(fit) > 1 {
		return nil, false
	}
	if fit == 0 {
		return nil, true
	}
	return []ledgerEvent{cands[bits.TrailingZeros(fit)]}, true
}

// explains says whether the records in mask account for what is left on
// every validator, within the rounding of the events that moved it: a shown
// delegation is rounded down, so each event can leave it a token or two from
// its amount, and an untouched one can move by a token.
func explains(left map[string]*big.Int, touched map[string]int, effects []map[string]*big.Int, mask uint) bool {
	for v, l := range left {
		rest := new(big.Int).Set(l)
		n := touched[v]
		for i, eff := range effects {
			if mask&(1<<i) == 0 {
				continue
			}
			if d, ok := eff[v]; ok {
				rest.Sub(rest, d)
				n++
			}
		}
		if rest.Abs(rest).Cmp(big.NewInt(int64(1+2*n))) > 0 {
			return false
		}
	}
	return true
}

// stakeEffect is the change one staking record makes to the wallet's shown
// delegations, per validator, and whether the record is the wallet's. It
// reports false when the record is the wallet's but its effect cannot be
// read from it.
func stakeEffect(typ string, rec map[string]string, addr, operator, bondDenom string) (map[string]*big.Int, bool, bool) {
	switch typ {
	case "delegate", "unbond", "redelegate", "cancel_unbonding_delegation", "tokenize_shares":
		if rec["delegator"] != addr {
			return nil, false, true
		}
	case "create_validator":
		if operator == "" || rec["validator"] != operator {
			return nil, false, true
		}
	case "redeem_shares":
		// The record names share tokens, not the bond tokens delegated.
		return nil, rec["delegator"] == addr, rec["delegator"] != addr
	default:
		return nil, false, true
	}
	amount := rec["amount"]
	if typ == "unbond" && rec["requested"] != "" {
		// The shares taken are fixed by the tokens asked for.
		amount = rec["requested"]
	}
	coins, err := ledgerCoins(amount, bondDenom)
	if err != nil || len(coins) != 1 {
		return nil, true, false
	}
	n := bondAmount(coins, bondDenom)
	if n.Sign() <= 0 {
		return nil, true, false
	}
	neg := new(big.Int).Neg(n)
	switch typ {
	case "delegate", "cancel_unbonding_delegation", "create_validator":
		if rec["validator"] == "" {
			return nil, true, false
		}
		return map[string]*big.Int{rec["validator"]: n}, true, true
	case "unbond", "tokenize_shares":
		if rec["validator"] == "" {
			return nil, true, false
		}
		return map[string]*big.Int{rec["validator"]: neg}, true, true
	default:
		src, dst := rec["source_validator"], rec["destination_validator"]
		if src == "" || dst == "" || src == dst {
			return nil, true, false
		}
		return map[string]*big.Int{src: neg, dst: n}, true, true
	}
}
