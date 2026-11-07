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
	ID     uint64
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
	Unbonding   []unbondingEntry
	// Rates holds the tokens and delegator shares at the anchor of the
	// validators the history touches; a validator missing here is unknown.
	Rates map[string]validatorRate
}

// validatorRate is a validator's bonded tokens and delegator shares. A
// delegation is held as shares and shown as tokens rounded down, so on a
// validator whose tokens per share is not exactly one (it was slashed at some
// point) each delegation change moves the shown tokens by up to one base unit
// more or less than the event's amount, and the shares issued to every new
// delegation are rounded, which moves the rate slightly. A validator at
// exactly one stays there until it is slashed.
type validatorRate struct {
	Tokens *big.Int
	Shares decimal.Decimal
}

func (r validatorRate) unit() bool {
	return r.Tokens != nil && r.Shares.Equal(decimal.NewFromBigInt(r.Tokens, 0))
}

type ledgerInput struct {
	Address   string
	Operator  string // the wallet's cosmosvaloper form, for its own create_validator
	BondDenom string
	Anchor    ledgerAnchor
	Events    []ledgerEvent
	Fees      []ledgerFee
	Slashes   []ledgerSlash
}

// ledgerHoldings is the rebuilt state at one block, in base units.
type ledgerHoldings struct {
	Height      int64
	Liquid      map[string]*big.Int
	Delegations map[string]*big.Int
	Unbonding   map[string]*big.Int
	// Approximate names validators whose delegation or unbonding figure was
	// carried back across a slash, which the index records only as a fraction,
	// or across delegation changes on a validator whose tokens per share is
	// not one, which round by up to one base unit each.
	Approximate map[string]bool
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
	slashes := append([]ledgerSlash(nil), in.Slashes...)
	sort.SliceStable(slashes, func(i, j int) bool { return slashes[i].Height > slashes[j].Height })

	order := append([]int64(nil), targets...)
	sort.Slice(order, func(i, j int) bool { return order[i] > order[j] })

	liquid := copyInts(in.Anchor.Bank)
	dels := copyInts(in.Anchor.Delegations)
	slack := map[string]int64{}
	approx := map[string]bool{}
	ei, fi, si := 0, 0, 0
	out := make(map[int64]ledgerHoldings, len(order))
	for _, h := range order {
		if h > in.Anchor.Height {
			return nil, fmt.Errorf("target height %d is after the anchor %d", h, in.Anchor.Height)
		}
		// Undo everything after h, newest first. A slash happens in BeginBlock,
		// so within a block it is undone after that block's transactions.
		for {
			nextEvent := ei < len(events) && events[ei].Height > h
			nextSlash := si < len(slashes) && slashes[si].Height > h
			if !nextEvent && !nextSlash {
				break
			}
			if nextSlash && (!nextEvent || slashes[si].Height > events[ei].Height ||
				(slashes[si].Height == events[ei].Height && events[ei].Phase == phaseBegin)) {
				sl := slashes[si]
				si++
				if cur, ok := dels[sl.Validator]; ok && cur.Sign() > 0 {
					keep := decimal.NewFromInt(1).Sub(sl.Fraction)
					if keep.Sign() > 0 {
						before := decimal.NewFromBigInt(cur, 0).Div(keep).Round(0)
						dels[sl.Validator] = before.BigInt()
					}
					approx[sl.Validator] = true
				}
				continue
			}
			ev := events[ei]
			ei++
			if err := undoEvent(in, ev, liquid, dels, slack); err != nil {
				return nil, err
			}
		}
		for fi < len(fees) && fees[fi].Height > h {
			for _, c := range fees[fi].Coins {
				addInt(liquid, c.Denom, c.Amount.BigInt())
			}
			fi++
		}
		unb, unbApprox := unbondingAt(in, h)
		shown := copyInts(dels)
		// Other delegators' share issue on a validator below one token per
		// share moves its rate a little, so a delegation there can shift a few
		// base units with no event of the wallet's.
		drift := map[string]bool{}
		if h < in.Anchor.Height {
			for v, d := range shown {
				if d.Sign() != 0 && !in.Anchor.Rates[v].unit() {
					drift[v] = true
				}
			}
		}
		for v, n := range slack {
			approx[v] = true
			// A delegation the rounding carried just below zero was emptied.
			if d, ok := shown[v]; ok && d.Sign() < 0 && d.CmpAbs(big.NewInt(n)) <= 0 {
				shown[v] = new(big.Int)
			}
		}
		hold := ledgerHoldings{
			Height:      h,
			Liquid:      pruneZero(copyInts(liquid)),
			Delegations: pruneZero(shown),
			Unbonding:   pruneZero(unb),
			Approximate: map[string]bool{},
		}
		for v := range approx {
			hold.Approximate[v] = true
		}
		for v := range unbApprox {
			hold.Approximate[v] = true
		}
		for v := range drift {
			hold.Approximate[v] = true
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
// delegations, and adds to slack the base units each delegation may be off by
// from rounding.
func undoEvent(in ledgerInput, ev ledgerEvent, liquid, dels map[string]*big.Int, slack map[string]int64) error {
	unit := func(v string) bool { return in.Anchor.Rates[v].unit() }
	round := func(v string, n int64) {
		if n > 0 {
			slack[v] += n
		}
	}
	nonUnit := func(v string) int64 {
		if unit(v) {
			return 0
		}
		return 1
	}
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
			amt := bondAmount(coins, in.BondDenom)
			switch ev.Type {
			case "redeem_shares":
				// LSM names the tokenized shares redeemed, not the tokens.
				if amt.Sign() == 0 {
					if amt, err = redeemedTokens(in, rec["validator"], coins); err != nil {
						return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err)
					}
				}
				subInt(dels, rec["validator"], amt)
				round(rec["validator"], nonUnit(rec["validator"]))
			case "delegate", "cancel_unbonding_delegation":
				subInt(dels, rec["validator"], amt)
				round(rec["validator"], nonUnit(rec["validator"]))
			case "unbond", "tokenize_shares":
				addInt(dels, rec["validator"], amt)
				round(rec["validator"], nonUnit(rec["validator"]))
			case "redelegate":
				// The event carries the requested amount; the destination
				// receives the source shares' tokens rounded down.
				src, dst := rec["source_validator"], rec["destination_validator"]
				addInt(dels, src, amt)
				subInt(dels, dst, amt)
				round(src, nonUnit(src))
				round(dst, nonUnit(src)+nonUnit(dst))
			}
		case "create_validator":
			// The self-bond of a new validator names only the operator.
			if rec["validator"] == "" || rec["validator"] != in.Operator {
				continue
			}
			coins, err := ledgerCoins(rec["amount"], in.BondDenom)
			if err != nil {
				return fmt.Errorf("%s at height %d: %w", ev.Type, ev.Height, err)
			}
			subInt(dels, rec["validator"], bondAmount(coins, in.BondDenom))
			round(rec["validator"], nonUnit(rec["validator"]))
		}
	}
	return nil
}

// redeemedTokens values tokenized shares of validator at its anchor tokens
// per share, rounded down as the chain does; the rounding is counted as slack
// by the caller.
func redeemedTokens(in ledgerInput, validator string, coins sdk.Coins) (*big.Int, error) {
	r, ok := in.Anchor.Rates[validator]
	if !ok || r.Tokens == nil || r.Shares.Sign() <= 0 {
		return nil, ledgerUnknown{fmt.Sprintf("redeemed liquid staking shares of %s cannot be valued because the validator is not on chain at the anchor", validator)}
	}
	total := new(big.Int)
	for _, c := range coins {
		if strings.HasPrefix(c.Denom, validator+"/") {
			total.Add(total, c.Amount.BigInt())
		}
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("no tokenized shares of %s in %s", validator, coins)
	}
	return decimal.NewFromBigInt(total, 0).Mul(decimal.NewFromBigInt(r.Tokens, 0)).Div(r.Shares).Floor().BigInt(), nil
}

// ledgerUnknown means the index lacks what the rebuild needs.
type ledgerUnknown struct{ msg string }

func (e ledgerUnknown) Error() string { return e.msg }

// unbondingAt rebuilds the unbonding entries in flight at h, per validator:
// entries still open at the anchor that were created by h, plus what was
// cancelled from them after h, plus everything that completed after h minus
// the entries created after h that had also completed by the anchor.
func unbondingAt(in ledgerInput, h int64) (map[string]*big.Int, map[string]bool) {
	out := map[string]*big.Int{}
	approx := map[string]bool{}
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
	// the two blocks before the slash.
	for _, sl := range in.Slashes {
		if sl.Height > h && (sl.DoubleSign || sl.Height-2 <= h) {
			if v, ok := out[sl.Validator]; ok && v.Sign() > 0 {
				approx[sl.Validator] = true
			}
		}
	}
	return out, approx
}
