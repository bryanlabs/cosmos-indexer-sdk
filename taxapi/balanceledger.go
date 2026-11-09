package taxapi

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/shopspring/decimal"
)

// A historical balance is rebuilt by walking back from a recent anchor block
// T, whose state the node still serves, to the last block H of the requested
// day: every coin and staking movement the index recorded in (H, T] is undone.
// Only blocks after H need to be indexed, so a date is answerable as soon as
// the chain is indexed from that date to the tip.

// Ledger event phases inside one block, in execution order.
const (
	phaseBegin = 0
	phaseTx    = 1
	phaseEnd   = 2
)

type ledgerAttr struct{ Key, Value string }

// ledgerEvent is one indexed event that names the wallet, with its attributes
// in emitted order. Older SDK logs merge every event of a type within a
// message, so one event can hold several records.
type ledgerEvent struct {
	ID uint64
	// Msg is the message a transaction event belongs to, 0 for block events.
	Msg    uint64
	Height int64
	Time   time.Time
	Phase  int
	Type   string
	Attrs  []ledgerAttr
}

// ledgerFee is a fee the ante handler took from the wallet.
type ledgerFee struct {
	Height int64
	Coins  sdk.Coins
}

// ledgerSlash is a validator slash in the window, with the fraction of bonded
// tokens it burned.
type ledgerSlash struct {
	Height     int64
	Validator  string
	Fraction   decimal.Decimal
	DoubleSign bool
}

type unbondingEntry struct {
	Validator      string
	CreationHeight int64
	Balance        *big.Int
}

// ledgerAnchor is the wallet's state at the anchor block, read from the node.
type ledgerAnchor struct {
	Height      int64
	Time        time.Time
	Bank        map[string]*big.Int
	Delegations map[string]*big.Int
	// Shares is the wallet's delegator shares per validator; a delegation
	// missing here is taken as its tokens, which holds only on a validator at
	// one token per share.
	Shares    map[string]decimal.Decimal
	Unbonding []unbondingEntry
	// Rates holds the tokens and delegator shares at the anchor of the
	// validators the history touches; a validator missing here is unknown.
	Rates map[string]validatorRate
}

// validatorRate is a validator's bonded tokens and delegator shares. A
// delegation is held as shares and shown as its tokens rounded down.
type validatorRate struct {
	Tokens *big.Int
	Shares decimal.Decimal
}

func (r validatorRate) unit() bool {
	return r.Tokens != nil && r.Shares.Equal(decimal.NewFromBigInt(r.Tokens, 0))
}

// rate is the validator's tokens per share.
func (r validatorRate) rate() (decimal.Decimal, bool) {
	if r.Tokens == nil || r.Shares.Sign() <= 0 {
		return decimal.Zero, false
	}
	return decimal.NewFromBigInt(r.Tokens, 0).DivRound(r.Shares, rateDigits), true
}

// rateSample is a validator's tokens and shares at the end of a block, read
// from chain state. The end of every UTC day is recorded.
type rateSample struct {
	Height int64
	Rate   validatorRate
}

type ledgerInput struct {
	Address   string
	Operator  string // the wallet's cosmosvaloper form, for its own create_validator
	BondDenom string
	Anchor    ledgerAnchor
	Events    []ledgerEvent
	Fees      []ledgerFee
	Slashes   []ledgerSlash
	// Samples holds the recorded rates before the anchor of the validators
	// whose rate is not one, ascending by height.
	Samples map[string][]rateSample
}

// ledgerHoldings is the rebuilt state at one block, in base units.
type ledgerHoldings struct {
	Height      int64
	Liquid      map[string]*big.Int
	Delegations map[string]*big.Int
	Unbonding   map[string]*big.Int
	// DelegationError and UnbondingError bound, in base units, how far a
	// figure can be from the chain's. A delegation is rebuilt in shares; it is
	// off only where an event leaves the shares to rounding (an undelegation
	// names the tokens paid out, rounded down) or a slash falls between two
	// recorded rates. An unbonding entry is off when a slash reached it. A
	// validator missing from them is exact.
	DelegationError map[string]*big.Int
	UnbondingError  map[string]*big.Int
}

// ledgerInconsistent means the index and the anchor disagree: undoing the
// recorded movements produced a negative amount.
type ledgerInconsistent struct{ msg string }

func (e ledgerInconsistent) Error() string { return e.msg }

// records splits an event's attributes into one map per emitted record. A new
// record starts when a key repeats, which keeps merged logs and either
// attribute order (LSM emits amount before the address) apart.
func (e ledgerEvent) records() []map[string]string {
	var out []map[string]string
	cur := map[string]string{}
	for _, a := range e.Attrs {
		if _, dup := cur[a.Key]; dup {
			out = append(out, cur)
			cur = map[string]string{}
		}
		cur[a.Key] = a.Value
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func ledgerCoins(v, bondDenom string) (sdk.Coins, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if isDigits(v) {
		v += bondDenom
	}
	return sdk.ParseCoinsNormalized(v)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func addInt(m map[string]*big.Int, k string, v *big.Int) {
	if cur, ok := m[k]; ok {
		cur.Add(cur, v)
		return
	}
	m[k] = new(big.Int).Set(v)
}

func subInt(m map[string]*big.Int, k string, v *big.Int) {
	addInt(m, k, new(big.Int).Neg(v))
}

func copyInts(m map[string]*big.Int) map[string]*big.Int {
	out := make(map[string]*big.Int, len(m))
	for k, v := range m {
		out[k] = new(big.Int).Set(v)
	}
	return out
}

func pruneZero(m map[string]*big.Int) map[string]*big.Int {
	for k, v := range m {
		if v.Sign() == 0 {
			delete(m, k)
		}
	}
	return m
}

func bondAmount(coins sdk.Coins, bondDenom string) *big.Int {
	return coins.AmountOf(bondDenom).BigInt()
}

// rateDigits is the precision of rebuilt rates and shares; the chain keeps 18
// decimal places.
const rateDigits = 36

// rateSlack is how far, relative, a validator's tokens per share can fall
// outside a slash: by the 18th place rounding of a payout, about 1e-29.
var rateSlack = decimal.New(1, -24)

// shareEpsilon covers the chain truncating shares to 18 decimal places.
var shareEpsilon = decimal.New(1, -18)

// rateBook bounds a validator's tokens per share at any block of the window.
// Between slashes a validator's rate never falls: every share issue and
// redemption rounds in the validator's favour. So the newest recorded rate
// before a point, lowered by the full fraction of each slash in between, and
// the oldest recorded rate after it, raised the same way, bound it. A
// validator at exactly one token per share at the anchor with no slash in the
// window was at one throughout.
type rateBook struct {
	unit    map[string]bool
	samples map[string][]rateSample
	slashes map[string][]ledgerSlash
}

func newRateBook(in ledgerInput) rateBook {
	b := rateBook{unit: map[string]bool{}, samples: map[string][]rateSample{}, slashes: map[string][]ledgerSlash{}}
	for _, sl := range in.Slashes {
		b.slashes[sl.Validator] = append(b.slashes[sl.Validator], sl)
	}
	for v, ss := range in.Samples {
		for _, smp := range ss {
			if smp.Height < in.Anchor.Height {
				b.samples[v] = append(b.samples[v], smp)
			}
		}
	}
	for v, r := range in.Anchor.Rates {
		if r.unit() && len(b.slashes[v]) == 0 {
			b.unit[v] = true
		}
		b.samples[v] = append(b.samples[v], rateSample{in.Anchor.Height, r})
	}
	for _, ss := range b.samples {
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].Height < ss[j].Height })
	}
	return b
}

// at bounds v's rate at a point after the end of block lastBefore, before the
// end of block firstAfter, and after the slashes up to height cut.
func (b rateBook) at(v string, lastBefore, firstAfter, cut int64) (decimal.Decimal, decimal.Decimal, error) {
	one := decimal.NewFromInt(1)
	if b.unit[v] {
		return one, one, nil
	}
	ss := b.samples[v]
	i := sort.Search(len(ss), func(k int) bool { return ss[k].Height > lastBefore }) - 1
	j := sort.Search(len(ss), func(k int) bool { return ss[k].Height >= firstAfter })
	if i < 0 || j >= len(ss) {
		return one, one, ledgerUnknown{fmt.Sprintf("the tokens per share of %s around height %d are not recorded", v, cut)}
	}
	lo, okLo := ss[i].Rate.rate()
	hi, okHi := ss[j].Rate.rate()
	if !okLo || !okHi {
		return one, one, ledgerUnknown{fmt.Sprintf("%s has no delegator shares around height %d", v, cut)}
	}
	for _, sl := range b.slashes[v] {
		keep := one.Sub(sl.Fraction)
		if keep.Sign() <= 0 {
			return one, one, ledgerUnknown{fmt.Sprintf("slash of %s at height %d burned every token", v, sl.Height)}
		}
		if sl.Height > ss[i].Height && sl.Height <= cut {
			lo = lo.Mul(keep)
		}
		if sl.Height > cut && sl.Height <= ss[j].Height {
			hi = hi.DivRound(keep, rateDigits)
		}
	}
	if lo.GreaterThan(hi) && !lo.GreaterThan(hi.Mul(one.Add(rateSlack))) {
		// Token payouts are rounded to 18 places before they are rounded
		// down, which can lower the rate by a fraction of 1e-18 tokens.
		lo, hi = hi, lo
	}
	if lo.GreaterThan(hi) {
		return one, one, ledgerUnknown{fmt.Sprintf("the recorded tokens per share of %s fell by more than its slashes around height %d", v, cut)}
	}
	return lo, hi, nil
}

// sharesFor bounds the shares that tokens between tlo and thi bought or were
// paid out for on v in block h: the tokens over the rate, truncated to the
// chain's 18 places. Truncation is monotone, so truncating the quotients at
// the rate's bounds bounds the chain's figure.
func (b rateBook) sharesFor(v string, h int64, tlo, thi decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	if b.unit[v] {
		return tlo, thi, nil
	}
	rlo, rhi, err := b.at(v, h-1, h, h)
	if err != nil {
		return tlo, thi, err
	}
	return shareTrunc(tlo.DivRound(rhi, rateDigits)), shareTrunc(thi.DivRound(rlo, rateDigits)), nil
}

// shareTrunc truncates a computed quotient to the chain's 18 share places,
// first rounding off the last digits so a quotient that is exact on chain
// is not carried below itself by the division's own rounding.
func shareTrunc(q decimal.Decimal) decimal.Decimal {
	return q.Round(30).Truncate(18)
}

// tokensFor bounds the tokens shares were paid out at on v in block h, rounded
// down as the chain does.
func (b rateBook) tokensFor(v string, h int64, shares decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	if b.unit[v] {
		return shares.Floor(), shares.Floor(), nil
	}
	rlo, rhi, err := b.at(v, h-1, h, h)
	if err != nil {
		return shares, shares, err
	}
	return chainTokens(shares.Mul(rlo)), chainTokens(shares.Mul(rhi)), nil
}

// chainTokens is the whole tokens the chain shows for a share value: it
// rounds the product to 18 places (half to even) and then rounds down.
func chainTokens(x decimal.Decimal) decimal.Decimal {
	return x.RoundBank(18).Floor()
}

// shareSpan is the range a wallet's delegator shares on a validator are in.
type shareSpan struct{ lo, hi decimal.Decimal }

// shown is the delegation the chain shows for sp at the end of block h, its
// shares times the rate as chainTokens rounds it: the middle of the possible figures and
// how far the chain's can be from it.
func (b rateBook) shown(v string, h int64, sp shareSpan) (*big.Int, *big.Int, error) {
	rlo, rhi, err := b.at(v, h, h, h)
	if err != nil {
		return nil, nil, err
	}
	lo := sp.lo
	if lo.Sign() < 0 {
		lo = decimal.Zero
	}
	minT, maxT := chainTokens(lo.Mul(rlo)), chainTokens(sp.hi.Mul(rhi))
	if sp.hi.Sign() < 0 && sp.hi.GreaterThan(shareEpsilon.Mul(decimal.NewFromInt(-1000))) {
		// Truncation alone carried an emptied delegation just below zero.
		maxT = decimal.Zero
	}
	if maxT.Sign() < 0 {
		return maxT.BigInt(), new(big.Int), nil
	}
	mid := chainTokens(lo.Add(sp.hi).Div(decimal.NewFromInt(2)).Mul(rlo.Add(rhi).Div(decimal.NewFromInt(2))))
	if mid.LessThan(minT) {
		mid = minT
	}
	if mid.GreaterThan(maxT) {
		mid = maxT
	}
	bound := decimal.Max(mid.Sub(minT), maxT.Sub(mid))
	return mid.BigInt(), bound.BigInt(), nil
}

// walkBack rebuilds the wallet at each target height (all at or below the
// anchor). Targets are answered from the same event set.
func walkBack(in ledgerInput, targets []int64) (map[int64]ledgerHoldings, error) {
	events := append([]ledgerEvent(nil), in.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		if a.Phase != b.Phase {
			return a.Phase > b.Phase
		}
		return a.ID > b.ID
	})
	fees := append([]ledgerFee(nil), in.Fees...)
	sort.SliceStable(fees, func(i, j int) bool { return fees[i].Height > fees[j].Height })

	order := append([]int64(nil), targets...)
	sort.Slice(order, func(i, j int) bool { return order[i] > order[j] })

	book := newRateBook(in)
	liquid := copyInts(in.Anchor.Bank)
	shares := map[string]*shareSpan{}
	for v, d := range in.Anchor.Delegations {
		sh, ok := in.Anchor.Shares[v]
		if !ok {
			if !book.unit[v] {
				return nil, ledgerUnknown{fmt.Sprintf("the delegator shares on %s at the anchor are unknown", v)}
			}
			sh = decimal.NewFromBigInt(d, 0)
		}
		shares[v] = &shareSpan{sh, sh}
	}
	spent := bondSpentByMessage(in)
	ei, fi := 0, 0
	out := make(map[int64]ledgerHoldings, len(order))
	for _, h := range order {
		if h > in.Anchor.Height {
			return nil, fmt.Errorf("target height %d is after the anchor %d", h, in.Anchor.Height)
		}
		for ei < len(events) && events[ei].Height > h {
			if err := undoEvent(in, book, spent, events[ei], liquid, shares); err != nil {
				return nil, err
			}
			ei++
		}
		for fi < len(fees) && fees[fi].Height > h {
			for _, c := range fees[fi].Coins {
				addInt(liquid, c.Denom, c.Amount.BigInt())
			}
			fi++
		}
		unb, unbErr := unbondingAt(in, h)
		hold := ledgerHoldings{
			Height:          h,
			Liquid:          pruneZero(copyInts(liquid)),
			Delegations:     map[string]*big.Int{},
			Unbonding:       pruneZero(unb),
			DelegationError: map[string]*big.Int{},
			UnbondingError:  unbErr,
		}
		tiny := shareEpsilon.Mul(decimal.NewFromInt(1000))
		for v, sp := range shares {
			if sp.lo.Sign() <= 0 && sp.hi.LessThan(tiny) {
				// Emptied: no tokens at any rate, so no rate is needed.
				continue
			}
			amt, bound, err := book.shown(v, h, *sp)
			if err != nil {
				return nil, err
			}
			if amt.Sign() != 0 || bound.Sign() != 0 {
				hold.Delegations[v] = amt
			}
			if bound.Sign() != 0 {
				hold.DelegationError[v] = bound
			}
		}
		for _, m := range []map[string]*big.Int{hold.Liquid, hold.Delegations, hold.Unbonding} {
			for k, v := range m {
				if v.Sign() < 0 {
					return nil, ledgerInconsistent{fmt.Sprintf("rebuilt %s for %s at height %d is negative (%s)", k, in.Address, h, v)}
				}
			}
		}
		out[h] = hold
	}
	return out, nil
}

// undoEvent reverses one event's effect on the wallet's liquid balance and
// delegator shares.
func undoEvent(in ledgerInput, book rateBook, spent map[uint64]*big.Int, ev ledgerEvent, liquid map[string]*big.Int, shares map[string]*shareSpan) error {
	span := func(v string) *shareSpan {
		if sp, ok := shares[v]; ok {
			return sp
		}
		sp := &shareSpan{decimal.Zero, decimal.Zero}
		shares[v] = sp
		return sp
	}
	// add puts back shares between lo and hi the event took away.
	add := func(v string, lo, hi decimal.Decimal) {
		sp := span(v)
		sp.lo, sp.hi = sp.lo.Add(lo), sp.hi.Add(hi)
	}
	remove := func(v string, lo, hi decimal.Decimal) {
		sp := span(v)
		sp.lo, sp.hi = sp.lo.Sub(hi), sp.hi.Sub(lo)
	}
	one := decimal.NewFromInt(1)
	for _, rec := range ev.records() {
		switch ev.Type {
		case "coin_received", "coin_spent":
			who := rec["receiver"]
			if ev.Type == "coin_spent" {
				who = rec["spender"]
			}
			if who != in.Address {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err)
			}
			for _, c := range coins {
				if ev.Type == "coin_received" {
					subInt(liquid, c.Denom, c.Amount.BigInt())
				} else {
					addInt(liquid, c.Denom, c.Amount.BigInt())
				}
			}
		case "delegate", "unbond", "redelegate", "cancel_unbonding_delegation", "tokenize_shares", "redeem_shares":
			if rec["delegator"] != in.Address {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err)
			}
			amt := decimal.NewFromBigInt(bondAmount(coins, in.BondDenom), 0)
			v := rec["validator"]
			wrap := func(err error) error { return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err) }
			switch ev.Type {
			case "delegate", "cancel_unbonding_delegation":
				if n, err := decimal.NewFromString(rec["new_shares"]); err == nil && ev.Type == "delegate" {
					remove(v, n, n)
					continue
				}
				lo, hi, err := book.sharesFor(v, ev.Height, amt, amt)
				if err != nil {
					return wrap(err)
				}
				remove(v, lo, hi)
			case "unbond":
				// The event names the tokens paid out, the shares' value
				// rounded down.
				if book.unit[v] {
					add(v, amt, amt)
					continue
				}
				lo, hi, err := book.sharesFor(v, ev.Height, amt, amt.Add(one))
				if err != nil {
					return wrap(err)
				}
				add(v, lo, hi)
			case "tokenize_shares":
				// The event names the tokens asked for; the shares taken are
				// those tokens over the rate, truncated.
				lo, hi, err := book.sharesFor(v, ev.Height, amt, amt)
				if err != nil {
					return wrap(err)
				}
				add(v, lo, hi)
			case "redeem_shares":
				// LSM names the share tokens redeemed. They are paid out at
				// the validator's rate, rounded down, and the tokens are
				// delegated back to the wallet.
				tlo, thi := amt, amt
				if r, ok := spent[ev.Msg]; ok && amt.Sign() == 0 {
					// The wallet is paid the record's tokens and delegates
					// them back in the same message, so its spend names them.
					tlo = decimal.NewFromBigInt(r, 0)
					thi = tlo
				} else if amt.Sign() == 0 {
					z, err := redeemedShares(v, coins)
					if err != nil {
						return wrap(err)
					}
					if tlo, thi, err = book.tokensFor(v, ev.Height, z); err != nil {
						return wrap(err)
					}
				}
				lo, hi, err := book.sharesFor(v, ev.Height, tlo, thi)
				if err != nil {
					return wrap(err)
				}
				remove(v, lo, hi)
			case "redelegate":
				// The event names the tokens asked for; the destination
				// receives the source shares' value rounded down.
				src, dst := rec["source_validator"], rec["destination_validator"]
				lo, hi, err := book.sharesFor(src, ev.Height, amt, amt)
				if err != nil {
					return wrap(err)
				}
				add(src, lo, hi)
				paid := amt
				if !book.unit[src] {
					paid = amt.Sub(one)
				}
				if lo, hi, err = book.sharesFor(dst, ev.Height, paid, amt); err != nil {
					return wrap(err)
				}
				remove(dst, lo, hi)
			}
		case "create_validator":
			// The self-bond of a new validator names only the operator, and
			// a new validator issues one share per token.
			if rec["validator"] == "" || rec["validator"] != in.Operator {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err)
			}
			n := decimal.NewFromBigInt(bondAmount(coins, in.BondDenom), 0)
			remove(rec["validator"], n, n)
		}
	}
	return nil
}

// bondSpentByMessage totals the bond tokens the wallet spent in each message
// that redeemed liquid staking shares. A redemption pays the record's tokens
// out to the wallet and delegates them back from it, and the wallet spends
// nothing else in bond tokens within that message.
func bondSpentByMessage(in ledgerInput) map[uint64]*big.Int {
	redeems := map[uint64]bool{}
	for _, ev := range in.Events {
		if ev.Type == "redeem_shares" && ev.Msg != 0 {
			redeems[ev.Msg] = true
		}
	}
	out := map[uint64]*big.Int{}
	for _, ev := range in.Events {
		if ev.Type != "coin_spent" || !redeems[ev.Msg] {
			continue
		}
		for _, rec := range ev.records() {
			if rec["spender"] != in.Address {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				continue
			}
			if n := bondAmount(coins, in.BondDenom); n.Sign() > 0 {
				if out[ev.Msg] == nil {
					out[ev.Msg] = new(big.Int)
				}
				out[ev.Msg].Add(out[ev.Msg], n)
			}
		}
	}
	return out
}

// redeemedShares totals the tokenized shares of validator in coins.
func redeemedShares(validator string, coins sdk.Coins) (decimal.Decimal, error) {
	total := new(big.Int)
	for _, c := range coins {
		if strings.HasPrefix(c.Denom, validator+"/") {
			total.Add(total, c.Amount.BigInt())
		}
	}
	if total.Sign() == 0 {
		return decimal.Zero, fmt.Errorf("no tokenized shares of %s in %s", validator, coins)
	}
	return decimal.NewFromBigInt(total, 0), nil
}

// ledgerUnknown means the index lacks what the rebuild needs.
type ledgerUnknown struct{ msg string }

func (e ledgerUnknown) Error() string { return e.msg }

// unbondingAt rebuilds the unbonding entries in flight at h, per validator:
// entries still open at the anchor that were created by h, plus what was
// cancelled from them after h, plus everything that completed after h minus
// the entries created after h that had also completed by the anchor. The
// second map bounds the figures a slash after h reached.
func unbondingAt(in ledgerInput, h int64) (map[string]*big.Int, map[string]*big.Int) {
	out := map[string]*big.Int{}
	bounds := map[string]*big.Int{}
	for _, e := range in.Anchor.Unbonding {
		if e.CreationHeight <= h {
			addInt(out, e.Validator, e.Balance)
		}
	}
	type entryKey struct {
		validator string
		height    int64
	}
	created := map[entryKey]*big.Int{}
	completesByAnchor := map[entryKey]bool{}
	cancelled := map[entryKey]*big.Int{}
	for _, ev := range in.Events {
		if ev.Height <= h {
			continue
		}
		for _, rec := range ev.records() {
			if rec["delegator"] != in.Address {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				continue
			}
			amt := bondAmount(coins, in.BondDenom)
			switch ev.Type {
			case "unbond":
				k := entryKey{rec["validator"], ev.Height}
				if c, ok := created[k]; ok {
					c.Add(c, amt)
				} else {
					created[k] = new(big.Int).Set(amt)
				}
				if done, err := time.Parse(time.RFC3339Nano, rec["completion_time"]); err == nil && !done.After(in.Anchor.Time) {
					completesByAnchor[k] = true
				}
			case "cancel_unbonding_delegation":
				ch, err := strconv.ParseInt(rec["creation_height"], 10, 64)
				if err != nil {
					continue
				}
				if ch <= h {
					addInt(out, rec["validator"], amt)
				} else {
					k := entryKey{rec["validator"], ch}
					if c, ok := cancelled[k]; ok {
						c.Add(c, amt)
					} else {
						cancelled[k] = new(big.Int).Set(amt)
					}
				}
			case "complete_unbonding":
				addInt(out, rec["validator"], amt)
			}
		}
	}
	for k := range completesByAnchor {
		done := new(big.Int).Set(created[k])
		if c, ok := cancelled[k]; ok {
			done.Sub(done, c)
		}
		subInt(out, k.validator, done)
	}
	// A slash reaches unbonding entries created at or after its infraction
	// height: any entry for a double sign, and for downtime only entries from
	// the two blocks before the slash. It takes up to its fraction of the
	// entry's starting balance, which the rebuilt figure, read after the
	// slash, already lacks.
	one := decimal.NewFromInt(1)
	for _, sl := range in.Slashes {
		if sl.Height > h && (sl.DoubleSign || sl.Height-2 <= h) {
			if v, ok := out[sl.Validator]; ok && v.Sign() > 0 {
				keep := one.Sub(sl.Fraction)
				taken := decimal.NewFromBigInt(v, 0)
				if keep.Sign() > 0 {
					taken = taken.Mul(sl.Fraction).Div(keep).Ceil().Add(one)
				}
				addInt(bounds, sl.Validator, taken.BigInt())
			}
		}
	}
	return out, bounds
}
