// Package balancedays builds legacy.balance_days: what every address held at
// the end of each UTC day of a legacy chain, built forward from the chain's
// genesis file with every stored movement and charged fee up to the export
// height. No archive node is read.
//
// Holdings use the format of the live chain's daily balances: spendable
// coins, delegations and unbonding entries per validator, and, per
// validator, a bound on how far a figure can be from the chain's own. A row
// is written only on a day the holdings change; a day with no row has the
// holdings of the latest row before it.
//
// Spendable coins follow the rules of the books close exactly. A delegation
// is rebuilt in shares. The stored rows carry tokens, so a validator whose
// share price moved during the chain (a slash) leaves the price at each
// movement inside the range the two genesis files give; the figure shown is
// the middle of what that range allows and the bound covers both ends. A
// validator whose price did not move is exact up to the chain's own
// rounding of shares.
//
// A chain whose stored block events hold every slash (cosmoshub-3) is
// replayed exactly instead: validator tokens and shares and delegation shares
// follow the staking module's own arithmetic, slashes included, so every
// figure is the chain's and carries no bound.
//
// At the export height the result is compared, per address and per
// delegation, with the next chain's genesis file. Those comparisons are the
// gates: a chain whose history does not explain the next genesis is not
// written.
package balancedays

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/DefiantLabs/cosmos-indexer/legacy/books"
)

const denom = "uatom"

// The unbonding period of cosmoshub-1 to 3, from each genesis file.
const unbondingTime = 21 * 24 * time.Hour

// Components of legacy.reconciliation this build writes.
const (
	ComponentDelegations = "delegations"
	ComponentUnbonding   = "unbonding"
)

// Statuses of a reconciliation line this build writes.
const (
	StatusClosed   = "closed"
	StatusRounding = "closed_within_share_rounding"
	StatusBound    = "closed_within_slash_bound"
	StatusOpen     = "open"
)

// Row is one stored day of one address.
type Row struct {
	Address  string
	Day      time.Time
	Height   int64
	Holdings string
}

// Line is one reconciliation line: an address's total of a component in the
// next genesis file against the history.
type Line struct {
	Address, Component string
	Expected, Computed *big.Int
	Status             string
}

// Gate counts one comparison with the next genesis file, per delegator and
// validator pair.
type Gate struct {
	Pairs, Exact, Rounding, WithinBound, Outside int
	// OutsideAmount is the sum of the differences of the pairs outside.
	OutsideAmount *big.Int
	// PairsBounded counts pairs whose figure carries a bound at the export
	// height, and MaxBound the largest.
	PairsBounded int
	MaxBound     *big.Int
}

// Result is the outcome of a build. Rows and lines are for writing; the
// rest is counts and totals only.
type Result struct {
	Chain, Next      string
	Through          int64
	Days, DaysStored int
	Rows             int64
	Wallets          int
	EventRows, Fees  int64
	// AddedMoves counts the undelegations and redelegations replayed from
	// ExtraMoves, which the stored rows hold no row for.
	AddedMoves     int64
	ChangeRows     int64
	OtherDenomRows int64
	ModuleAccounts int

	// Spendable balance gate.
	LiquidCompared, LiquidExact, LiquidDeposit, LiquidChange, LiquidOpen int
	// Comparison with the spendable lines legacy.reconciliation holds.
	StoredLiquidLines, StoredLiquidEqual int

	Delegations, Unbonding Gate
	// Validators whose share price differs between the two genesis files,
	// validators absent from the next one, and of those, validators that
	// lost tokens, whose last price is estimated from the loss.
	ValidatorsPriceMoved, ValidatorsRemoved, ValidatorsRemovedSlashed int
	// Jailed validators whose price is the same in both genesis files, so
	// a slash shows only if every delegation left before the price reset.
	ValidatorsJailedSamePrice int
	// Delegations on a validator whose price moved, bounded backwards from
	// the next genesis file: movements where that narrowed the range, where
	// the two ranges missed by no more than the chain's rounding, and where
	// they missed by more (a history the next file does not explain).
	AnchoredPairs, AnchorNarrowed, AnchorRounding, AnchorConflicts int

	// Completed unbondings: matched exactly against tracked entries,
	// matched to an entry made in the same block, paid less than tracked
	// (a slash reached them), or not matched.
	CompletesExact, CompletesSameBlock, CompletesSlashed, CompletesUnmatched int
	// Undelegations from a validator that was not bonded, paid in the same
	// message.
	CompletesInstant       int
	CompletesSlashedAmount *big.Int
	// Completed unbondings matched to entries other than the ones due by the
	// latest completion time the replay knew: an entry made while its
	// validator was unbonding completes with the validator, before that time.
	CompletesEarly int
	// Completions paid up to a token per entry less than tracked, the
	// chain's rounding of a token amount through shares.
	CompletesRounding int
	// An exact replay: whether the chain was replayed in the arithmetic of
	// its staking module, the stored slashes it applied, the unbonding and
	// redelegation entries those reached, the validators whose tokens and
	// shares at the export height equal the next genesis file and those that
	// differ, and movements the chain would have refused.
	ExactReplay                                          bool
	SlashesApplied, SlashedEntries, SlashedRedelegations int
	ValidatorsExact, ValidatorsDiffer, ExactAnomalies    int
	// Delegations whose shares at the export height equal the next genesis
	// file, and those that differ.
	SharesExact, SharesDiffer int
	// Day ends at which an address held less than nothing.
	NegativeDayEnds int

	Lines []Line
}

// Passed reports whether the history explains the next genesis file: every
// spendable balance closes (exactly, or as a recorded genesis change or a
// governance account the next chain did not carry), and every delegation and
// unbonding is exact, within the chain's share rounding, or within its slash
// bound.
func (r *Result) Passed() bool {
	return r.LiquidOpen == 0 && r.Delegations.Outside == 0 && r.Unbonding.Outside == 0 && r.CompletesUnmatched == 0 &&
		r.NegativeDayEnds == 0 && r.AnchorConflicts == 0 && r.ValidatorsDiffer == 0 && r.SharesDiffer == 0 && r.ExactAnomalies == 0
}

type rate struct {
	start, end *big.Rat
	moved      bool
	removed    bool
	// estimated is true for a removed validator whose last price comes from
	// the tokens it lost.
	estimated bool
	// floor, when set, is a lower price the validator may have had between
	// its two genesis values: it was jailed, so a slash may have taken
	// tokens before every delegation left it and its price began again at
	// one token a share.
	floor *big.Rat
}

// at gives the share price range at a height. Before the export height a
// moved price is anywhere between its two genesis values. At the export
// height the next genesis file gives the price, except for a validator it no
// longer holds, whose last price is only estimated.
func (r *rate) at(final bool) (lo, hi *big.Rat) {
	if final && !r.removed {
		return r.end, r.end
	}
	if !r.moved {
		return r.start, r.start
	}
	lo, hi = r.end, r.start
	if r.start.Cmp(r.end) < 0 {
		lo, hi = r.start, r.end
	}
	if r.floor != nil && r.floor.Cmp(lo) < 0 {
		lo = r.floor
	}
	return new(big.Rat).Mul(lo, drift[0]), new(big.Rat).Mul(hi, drift[1])
}

// drift widens a moved price for the rounding every movement leaves in the
// validator (up to a token each, which nudges the price up between slashes):
// one part in 10^8 either way.
var drift = [2]*big.Rat{big.NewRat(99999999, 100000000), big.NewRat(100000001, 100000000)}

type pair struct {
	lo, hi *big.Rat
	ops    int
	// sh is the delegation's shares in an exact replay, and last the token
	// figure its address was last checked at.
	sh   sdkmath.LegacyDec
	last *big.Int
}

type entry struct {
	balance    *big.Int
	completion time.Time
	height     int64
	// stated is true when the stored row gave the completion time; without
	// it the time is the latest the chain allows, and a validator that was
	// unbonding paid earlier.
	stated bool
	// slack is how much less than the stored amount the chain may have put
	// in the entry: it turns a token amount into shares and back, and keeps
	// the remainder when a share is not worth exactly one token.
	slack int64
	// initial is what an exact replay put in the entry when it was made,
	// which a later slash takes its share of.
	initial *big.Int
}

type wallet struct {
	liquid  *big.Int
	pairs   map[string]*pair
	entries map[string][]*entry
	module  bool
	stored  string
	written bool
}

type ledger struct {
	// timedRedelegations dates a redelegation the stored row gives no
	// completion time from its block time (Hubs 1 and 2).
	timedRedelegations bool
	// msgShares holds the shares an undelegation or redelegation message
	// named, for a chain whose messages named shares rather than tokens.
	msgShares map[[3]int64]sdkmath.LegacyDec
	// msgTokens holds the token amount an undelegation or redelegation
	// message named, where the chain's messages named tokens.
	msgTokens map[[3]int64]sdkmath.Int
	wallets   map[string]*wallet
	rates     map[string]*rate
	// trails, when set, collects what each movement did to each delegation
	// on a validator whose price moved (first pass).
	trails map[[2]string][]contribution
	// anchors, when set, bounds those delegations backwards from the next
	// genesis file (second pass).
	anchors map[[2]string]*anchor
	// instant holds what a message paid out at once, before the stored rows
	// show the undelegation it paid: the chain completes an undelegation from
	// a validator that is not bonded in the same message.
	instant                        map[instantKey]*big.Int
	anchorNarrowed, anchorRounding int
	anchorConflicts                int

	// An exact replay keeps each validator's tokens and shares, the
	// redelegations from each validator a slash can reach, the delegators of
	// each validator, and the validators changed since the last day end.
	exact                                bool
	vals                                 map[string]*valState
	reds                                 map[string][]*redEntry
	members                              map[string]map[string]bool
	touched                              map[string]bool
	exactAnomalies, slashesApplied       int
	slashedEntries, slashedRedelegations int
}

type instantKey struct {
	delegator, validator string
	height               int64
	tx, msg              int
}

// contribution is what one movement did to a delegation's shares: at least
// lo and at most hi.
type contribution struct{ lo, hi *big.Rat }

// anchor bounds a delegation after each of its movements from the shares the
// next genesis file gives and what the later movements did.
type anchor struct {
	end          *big.Rat
	sufLo, sufHi []*big.Rat
	seen         int
}

func newAnchor(end *big.Rat, trail []contribution) *anchor {
	n := len(trail)
	a := &anchor{end: end, sufLo: make([]*big.Rat, n+1), sufHi: make([]*big.Rat, n+1)}
	a.sufLo[n], a.sufHi[n] = new(big.Rat), new(big.Rat)
	for k := n - 1; k >= 0; k-- {
		a.sufLo[k] = new(big.Rat).Add(a.sufLo[k+1], trail[k].lo)
		a.sufHi[k] = new(big.Rat).Add(a.sufHi[k+1], trail[k].hi)
	}
	return a
}

func (l *ledger) at(address string) *wallet {
	w := l.wallets[address]
	if w == nil {
		w = &wallet{liquid: new(big.Int), pairs: map[string]*pair{}, entries: map[string][]*entry{}}
		l.wallets[address] = w
	}
	return w
}

func (l *ledger) rateOf(validator string) *rate {
	r := l.rates[validator]
	if r == nil {
		// A validator created during the chain starts at one token a share.
		one := big.NewRat(1, 1)
		r = &rate{start: one, end: one, removed: true}
		l.rates[validator] = r
	}
	return r
}

func (w *wallet) pairOf(validator string) *pair {
	p := w.pairs[validator]
	if p == nil {
		p = &pair{lo: new(big.Rat), hi: new(big.Rat)}
		w.pairs[validator] = p
	}
	return p
}

// bond adds tokens to (sign 1) or takes them from (sign -1) a delegation,
// at a price somewhere in the validator's range.
func (l *ledger) bond(w *wallet, address, validator string, sign int, tokens *big.Int) {
	r := l.rateOf(validator)
	lo, hi := r.at(false)
	p := w.pairOf(validator)
	amount := new(big.Rat).SetInt(tokens)
	few := new(big.Rat).Quo(amount, hi)
	many := new(big.Rat).Quo(amount, lo)
	c := contribution{lo: few, hi: many}
	if sign < 0 {
		c = contribution{lo: new(big.Rat).Neg(many), hi: new(big.Rat).Neg(few)}
	}
	p.lo.Add(p.lo, c.lo)
	p.hi.Add(p.hi, c.hi)
	if p.lo.Sign() < 0 {
		p.lo.SetInt64(0)
	}
	p.ops++
	if !r.moved {
		return
	}
	key := [2]string{address, validator}
	if l.trails != nil {
		l.trails[key] = append(l.trails[key], c)
	}
	if a := l.anchors[key]; a != nil {
		l.narrow(key, p, a, hi)
	}
}

// narrow intersects a delegation's shares after a movement with what the
// next genesis file and the later movements allow. Where the two ranges miss
// each other by no more than a token per movement (the chain's rounding),
// the backward figure is kept; a wider miss is a history the next genesis
// file does not explain.
func (l *ledger) narrow(key [2]string, p *pair, a *anchor, price *big.Rat) {
	a.seen++
	if a.seen >= len(a.sufLo) {
		l.anchorConflicts++
		return
	}
	lo := new(big.Rat).Sub(a.end, a.sufHi[a.seen])
	if lo.Sign() < 0 {
		lo.SetInt64(0)
	}
	hi := new(big.Rat).Sub(a.end, a.sufLo[a.seen])
	if hi.Sign() < 0 {
		hi.SetInt64(0)
	}
	least, most := p.lo, p.hi
	if lo.Cmp(least) > 0 {
		least = lo
	}
	if hi.Cmp(most) < 0 {
		most = hi
	}
	if least.Cmp(most) <= 0 {
		if least != p.lo || most != p.hi {
			l.anchorNarrowed++
		}
		p.lo, p.hi = new(big.Rat).Set(least), new(big.Rat).Set(most)
		return
	}
	gap := new(big.Rat).Mul(new(big.Rat).Sub(least, most), price)
	// Every movement of the delegation, before and after this one, may be
	// a token off.
	if gap.Cmp(new(big.Rat).SetInt64(int64(p.ops+len(a.sufLo)-a.seen))) <= 0 {
		l.anchorRounding++
		if p.lo.Cmp(hi) > 0 {
			p.lo, p.hi = new(big.Rat).Set(hi), new(big.Rat).Set(hi)
		} else {
			p.lo, p.hi = new(big.Rat).Set(lo), new(big.Rat).Set(lo)
		}
		return
	}
	l.anchorConflicts++
	if Trace != nil {
		Trace("anchor conflict %s %s movement=%d forward=%s..%s backward=%s..%s", short(key[0]), short(key[1]), a.seen,
			p.lo.FloatString(6), p.hi.FloatString(6), lo.FloatString(6), hi.FloatString(6))
	}
}

func floorRat(r *big.Rat) *big.Int {
	q := new(big.Int)
	m := new(big.Int)
	q.DivMod(r.Num(), r.Denom(), m)
	return q
}

func ceilRat(r *big.Rat) *big.Int {
	q := floorRat(r)
	if new(big.Rat).SetInt(q).Cmp(r) != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// shown is a delegation's figure and bound at a day end.
func (l *ledger) shown(validator string, p *pair, final bool) (amount, bound *big.Int) {
	if l.exact {
		v := l.vals[validator]
		if v == nil {
			return new(big.Int), new(big.Int)
		}
		return v.tokensFromShares(p.dec()).TruncateInt().BigInt(), new(big.Int)
	}
	lo, hi := l.rateOf(validator).at(final)
	least := floorRat(new(big.Rat).Mul(p.lo, lo))
	most := new(big.Rat).Mul(p.hi, hi)
	if p.lo.Cmp(p.hi) == 0 && lo.Cmp(hi) == 0 {
		return floorRat(most), new(big.Int)
	}
	if least.Sign() < 0 {
		least.SetInt64(0)
	}
	top := ceilRat(most)
	if top.Sign() <= 0 {
		return new(big.Int), new(big.Int)
	}
	middle := floorRat(new(big.Rat).Quo(new(big.Rat).Add(new(big.Rat).Mul(p.lo, lo), most), big.NewRat(2, 1)))
	if middle.Sign() < 0 {
		middle.SetInt64(0)
	}
	spread := new(big.Int).Sub(middle, least)
	if up := new(big.Int).Sub(top, middle); up.Cmp(spread) > 0 {
		spread = up
	}
	return middle, spread
}

// entryBound is how much a slash could have taken from an unbonding entry:
// the share of the validator's price that moved during the chain.
func (l *ledger) entryBound(validator string, balance *big.Int) *big.Int {
	r := l.rateOf(validator)
	if l.exact || !r.moved || balance.Sign() == 0 {
		return new(big.Int)
	}
	lo, hi := r.at(false)
	lost := new(big.Rat).Sub(big.NewRat(1, 1), new(big.Rat).Quo(lo, hi))
	bound := ceilRat(lost.Mul(lost, new(big.Rat).SetInt(balance)))
	if bound.Cmp(balance) > 0 {
		bound.Set(balance)
	}
	return bound
}

type stored struct {
	Liquid          map[string]string `json:"liquid,omitempty"`
	Delegations     map[string]string `json:"delegations,omitempty"`
	Unbonding       map[string]string `json:"unbonding,omitempty"`
	DelegationError map[string]string `json:"delegation_error,omitempty"`
	UnbondingError  map[string]string `json:"unbonding_error,omitempty"`
}

func put(m *map[string]string, key string, value *big.Int) {
	if value.Sign() == 0 {
		return
	}
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[key] = value.String()
}

// holdings encodes a wallet's day end the way the live chain's daily
// balances are stored. encoding/json sorts map keys, so equal holdings
// encode to equal text.
func (l *ledger) holdings(w *wallet, final bool) (string, bool, error) {
	var s stored
	negative := w.liquid.Sign() < 0
	put(&s.Liquid, denom, w.liquid)
	for validator, p := range w.pairs {
		amount, bound := l.shown(validator, p, final)
		put(&s.Delegations, validator, amount)
		put(&s.DelegationError, validator, bound)
		if p.hi.Sign() < 0 {
			negative = true
			if Trace != nil {
				r := l.rateOf(validator)
				Trace("negative pair %s shares=%s..%s ops=%d moved=%t start=%s", short(validator), p.lo.FloatString(9), p.hi.FloatString(9), p.ops, r.moved, r.start.FloatString(12))
			}
		}
	}
	for validator, entries := range w.entries {
		sum, bound := new(big.Int), new(big.Int)
		for _, e := range entries {
			sum.Add(sum, e.balance)
			bound.Add(bound, l.entryBound(validator, e.balance))
			bound.Add(bound, big.NewInt(e.slack))
		}
		put(&s.Unbonding, validator, sum)
		put(&s.UnbondingError, validator, bound)
	}
	text, err := json.Marshal(s)
	return string(text), negative, err
}

type dayEnd struct {
	day  time.Time
	last int64
}

type genesis struct {
	liquid      map[string]*big.Int
	module      map[string]bool
	delegations map[[2]string]*big.Int
	unbonding   map[[2]string]*big.Int
}

func whole(text string) (*big.Int, error) {
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("a stored amount is not a whole number")
	}
	return value, nil
}

func fraction(text string) (*big.Rat, error) {
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("a stored share amount is not a number")
	}
	return value, nil
}

func finish(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

func loadGenesis(ctx context.Context, db *sql.DB, chain string) (*genesis, error) {
	g := &genesis{liquid: map[string]*big.Int{}, module: map[string]bool{}, delegations: map[[2]string]*big.Int{}, unbonding: map[[2]string]*big.Int{}}
	rows, err := db.QueryContext(ctx, `SELECT address, kind, liquid::text FROM legacy.genesis_accounts WHERE chain_id = $1`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var address, kind, liquid string
		if err := rows.Scan(&address, &kind, &liquid); err != nil {
			rows.Close()
			return nil, err
		}
		amount, err := whole(liquid)
		if err != nil {
			rows.Close()
			return nil, err
		}
		g.liquid[address] = amount
		g.module[address] = kind == "module"
	}
	if err := finish(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT delegator, validator, tokens::text FROM legacy.genesis_delegations WHERE chain_id = $1`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var delegator, validator, tokens string
		if err := rows.Scan(&delegator, &validator, &tokens); err != nil {
			rows.Close()
			return nil, err
		}
		amount, err := whole(tokens)
		if err != nil {
			rows.Close()
			return nil, err
		}
		g.delegations[[2]string{delegator, validator}] = amount
	}
	if err := finish(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT delegator, validator, sum(balance)::text FROM legacy.genesis_unbondings WHERE chain_id = $1 GROUP BY 1, 2`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var delegator, validator, balance string
		if err := rows.Scan(&delegator, &validator, &balance); err != nil {
			rows.Close()
			return nil, err
		}
		amount, err := whole(balance)
		if err != nil {
			rows.Close()
			return nil, err
		}
		g.unbonding[[2]string{delegator, validator}] = amount
	}
	return g, finish(rows)
}

func loadRates(ctx context.Context, db *sql.DB, chain string) (map[string]*big.Rat, error) {
	out := map[string]*big.Rat{}
	rows, err := db.QueryContext(ctx, `SELECT operator, tokens::text, delegator_shares::text FROM legacy.genesis_validators WHERE chain_id = $1`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var operator, tokens, shares string
		if err := rows.Scan(&operator, &tokens, &shares); err != nil {
			rows.Close()
			return nil, err
		}
		t, err := fraction(tokens)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s, err := fraction(shares)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if s.Sign() == 0 {
			// No shares, so no delegation can be priced; one token a share.
			out[operator] = big.NewRat(1, 1)
			continue
		}
		out[operator] = new(big.Rat).Quo(t, s)
	}
	return out, finish(rows)
}

// loadJailed reads which validators a genesis file holds as jailed.
func loadJailed(ctx context.Context, db *sql.DB, chain string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT operator FROM legacy.genesis_validators WHERE chain_id = $1 AND jailed`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var operator string
		if err := rows.Scan(&operator); err != nil {
			rows.Close()
			return nil, err
		}
		out[operator] = true
	}
	return out, finish(rows)
}

// downtimeSlash is the share of its tokens a validator lost when it was
// jailed for missing blocks on cosmoshub-1 to 3.
var downtimeSlash = big.NewRat(1, 10000)

// loadShares reads a genesis file's delegations in shares.
func loadShares(ctx context.Context, db *sql.DB, chain string) (map[[2]string]*big.Rat, error) {
	out := map[[2]string]*big.Rat{}
	rows, err := db.QueryContext(ctx, `SELECT delegator, validator, shares::text FROM legacy.genesis_delegations WHERE chain_id = $1`, chain)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var delegator, validator, text string
		if err := rows.Scan(&delegator, &validator, &text); err != nil {
			rows.Close()
			return nil, err
		}
		value, err := fraction(text)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[[2]string{delegator, validator}] = value
	}
	return out, finish(rows)
}

// flow is what came into a validator's tokens (its genesis tokens, then
// delegations and redelegations to it) and what is left of it after what
// went out (undelegations and redelegations from it), from the stored rows.
type flow struct {
	inflow, net *big.Int
	rows        int64
}

// lost reports a validator that ended with fewer tokens than the stored rows
// leave it, by more than a token per row of rounding.
func (f *flow) lost() bool {
	return f.inflow.Sign() > 0 && f.net.Cmp(big.NewInt(f.rows)) > 0
}

func loadFlows(ctx context.Context, db *sql.DB, chain string, through int64) (map[string]*flow, error) {
	out := map[string]*flow{}
	rows, err := db.QueryContext(ctx, `SELECT validator, sum(greatest(amount, 0))::text, sum(amount)::text, count(*) FROM (
SELECT operator AS validator, tokens AS amount FROM legacy.genesis_validators WHERE chain_id = $1
UNION ALL SELECT validator_address, amount::numeric FROM legacy.taxable_events
 WHERE chain_id = $1 AND height <= $2 AND denom = $3 AND category = 'delegate'
UNION ALL SELECT validator_address, -amount::numeric FROM legacy.taxable_events
 WHERE chain_id = $1 AND height <= $2 AND denom = $3 AND category = 'undelegate'
UNION ALL SELECT detail::jsonb->>'destination_validator', amount::numeric FROM legacy.taxable_events
 WHERE chain_id = $1 AND height <= $2 AND denom = $3 AND category = 'redelegate'
UNION ALL SELECT detail::jsonb->>'source_validator', -amount::numeric FROM legacy.taxable_events
 WHERE chain_id = $1 AND height <= $2 AND denom = $3 AND category = 'redelegate'
) f WHERE validator IS NOT NULL AND validator <> '' GROUP BY validator`, chain, through, denom)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var validator, inflow, net string
		var count int64
		if err := rows.Scan(&validator, &inflow, &net, &count); err != nil {
			rows.Close()
			return nil, err
		}
		in, err := whole(inflow)
		if err != nil {
			rows.Close()
			return nil, err
		}
		left, err := whole(net)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[validator] = &flow{inflow: in, net: left, rows: count}
	}
	return out, finish(rows)
}

type move struct {
	height                      int64
	phase, tx, msg              int
	category, amount, unit      string
	from, to, validator, detail string
	when                        time.Time
	fee                         bool
	// added is the movement file record this move was made from; its
	// addresses are placed when it runs.
	added *MoveRecord
}

type redelegation struct {
	Source      string `json:"source_validator"`
	Destination string `json:"destination_validator"`
	Completion  string `json:"completion_time"`
}

type undelegation struct {
	Completion string `json:"completion_time"`
}

// Build replays a chain and hands every changed day end to sink, in day
// order. It returns the gates; the caller decides whether to keep what sink
// received.
func Build(ctx context.Context, db *sql.DB, chain string, sink func(Row) error) (*Result, error) {
	out := &Result{Chain: chain, CompletesSlashedAmount: new(big.Int),
		Delegations: Gate{OutsideAmount: new(big.Int), MaxBound: new(big.Int)},
		Unbonding:   Gate{OutsideAmount: new(big.Int), MaxBound: new(big.Int)}}
	var era int
	var first int64
	var through sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT era, first_height, export_height FROM legacy.eras WHERE chain_id = $1`, chain).
		Scan(&era, &first, &through); err != nil {
		return nil, fmt.Errorf("the chain is not one of the legacy ledger")
	}
	if !through.Valid {
		return nil, fmt.Errorf("the chain has no export height")
	}
	out.Through = through.Int64
	if err := db.QueryRowContext(ctx, `SELECT chain_id FROM legacy.eras WHERE era = $1`, era+1).Scan(&out.Next); err != nil {
		return nil, fmt.Errorf("the chain after this one is not known")
	}
	var stored int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM legacy.blocks WHERE chain_id = $1 AND height BETWEEN $2 AND $3`, chain, first, out.Through).
		Scan(&stored); err != nil {
		return nil, err
	}
	if stored != out.Through-first+1 {
		return nil, fmt.Errorf("not every height up to the export height is stored")
	}

	var days []dayEnd
	rows, err := db.QueryContext(ctx, `SELECT (time AT TIME ZONE 'UTC')::date::text, max(height) FROM legacy.blocks
WHERE chain_id = $1 AND height <= $2 GROUP BY 1 ORDER BY 1`, chain, out.Through)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day string
		var last int64
		if err := rows.Scan(&day, &last); err != nil {
			rows.Close()
			return nil, err
		}
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(days) > 0 && last <= days[len(days)-1].last {
			rows.Close()
			return nil, fmt.Errorf("block times do not rise with height")
		}
		days = append(days, dayEnd{day: parsed, last: last})
	}
	if err := finish(rows); err != nil {
		return nil, err
	}
	if len(days) == 0 || days[len(days)-1].last != out.Through {
		return nil, fmt.Errorf("the export height has no stored block time")
	}
	out.Days = len(days)

	opening, err := loadGenesis(ctx, db, chain)
	if err != nil {
		return nil, err
	}
	closing, err := loadGenesis(ctx, db, out.Next)
	if err != nil {
		return nil, err
	}
	if len(opening.liquid) == 0 || len(closing.liquid) == 0 {
		return nil, fmt.Errorf("the genesis files of the chain and of the chain after it must both be loaded")
	}
	startRates, err := loadRates(ctx, db, chain)
	if err != nil {
		return nil, err
	}
	endRates, err := loadRates(ctx, db, out.Next)
	if err != nil {
		return nil, err
	}

	flows, err := loadFlows(ctx, db, chain, out.Through)
	if err != nil {
		return nil, err
	}
	jailed, err := loadJailed(ctx, db, chain)
	if err != nil {
		return nil, err
	}
	jailedLater, err := loadJailed(ctx, db, out.Next)
	if err != nil {
		return nil, err
	}
	for operator := range jailedLater {
		jailed[operator] = true
	}
	openingShares, err := loadShares(ctx, db, chain)
	if err != nil {
		return nil, err
	}
	closingShares, err := loadShares(ctx, db, out.Next)
	if err != nil {
		return nil, err
	}
	exact := exactChains[chain] || ForceExact[chain]
	msgShares, err := messageShares(chain)
	if err != nil {
		return nil, err
	}
	msgTokens, err := messageTokens(chain)
	if err != nil {
		return nil, err
	}
	var openingVals, closingVals map[string]*valState
	var slashes []*slash
	if exact {
		if openingVals, err = loadValidators(ctx, db, chain); err != nil {
			return nil, err
		}
		if closingVals, err = loadValidators(ctx, db, out.Next); err != nil {
			return nil, err
		}
		known := map[string]bool{}
		for operator := range openingVals {
			known[operator] = true
		}
		for operator := range closingVals {
			known[operator] = true
		}
		for operator := range flows {
			known[operator] = true
		}
		operators := make([]string, 0, len(known))
		for operator := range known {
			operators = append(operators, operator)
		}
		if slashes, err = loadSlashes(ctx, db, chain, out.Through, operators); err != nil {
			return nil, err
		}
		added, err := extraSlashes(ctx, db, chain, operators)
		if err != nil {
			return nil, err
		}
		slashes = append(slashes, added...)
		sort.SliceStable(slashes, func(i, j int) bool {
			if slashes[i].height != slashes[j].height {
				return slashes[i].height < slashes[j].height
			}
			return slashes[i].index < slashes[j].index
		})
	}
	newLedger := func() *ledger {
		l := &ledger{wallets: map[string]*wallet{}, rates: map[string]*rate{}, instant: map[instantKey]*big.Int{}}
		for operator, start := range startRates {
			end, kept := endRates[operator]
			r := &rate{start: start, end: start, removed: !kept}
			if kept {
				r.end = end
			}
			l.rates[operator] = r
		}
		for operator, end := range endRates {
			if _, known := l.rates[operator]; !known {
				l.rates[operator] = &rate{start: big.NewRat(1, 1), end: end}
			}
		}
		for operator := range flows {
			if _, known := l.rates[operator]; !known {
				one := big.NewRat(1, 1)
				l.rates[operator] = &rate{start: one, end: one, removed: true}
			}
		}
		for operator, r := range l.rates {
			if r.removed {
				// The next genesis file no longer holds the validator, so its
				// last price comes from the tokens it lost: what came in less
				// what went out, against what came in.
				if f := flows[operator]; f != nil && f.lost() {
					loss := new(big.Rat).SetFrac(f.net, f.inflow)
					r.end = new(big.Rat).Mul(r.start, new(big.Rat).Sub(big.NewRat(1, 1), loss))
					r.estimated = true
				}
			}
			r.moved = r.start.Cmp(r.end) != 0
			if !r.moved && jailed[operator] {
				r.floor = new(big.Rat).Mul(r.start, new(big.Rat).Sub(big.NewRat(1, 1), downtimeSlash))
				r.moved = true
			}
		}
		if exact {
			l.exact = true
			l.timedRedelegations = untimedRedelegations[chain] || (ForceExact[chain] && !exactChains[chain])
			l.msgShares = msgShares
			l.msgTokens = msgTokens
			l.vals = map[string]*valState{}
			for operator, v := range openingVals {
				l.vals[operator] = &valState{tokens: v.tokens, shares: v.shares}
			}
			l.reds = map[string][]*redEntry{}
			l.members = map[string]map[string]bool{}
			l.touched = map[string]bool{}
		}
		return l
	}
	{
		l := newLedger()
		for _, r := range l.rates {
			if r.moved {
				out.ValidatorsPriceMoved++
			}
			if r.removed {
				out.ValidatorsRemoved++
			}
			if r.estimated {
				out.ValidatorsRemovedSlashed++
			}
			if r.floor != nil {
				out.ValidatorsJailedSamePrice++
			}
		}
	}
	pass := func(l *ledger, out *Result, emit bool, sink func(Row) error) error {
		dirty := map[string]bool{}
		for address, amount := range opening.liquid {
			w := l.at(address)
			w.liquid.Add(w.liquid, amount)
			w.module = opening.module[address] || closing.module[address]
			dirty[address] = true
		}
		for address := range closing.module {
			if closing.module[address] {
				l.at(address).module = true
			}
		}
		for key, value := range openingShares {
			w := l.at(key[0])
			p := w.pairOf(key[1])
			p.lo.Set(value)
			p.hi.Set(value)
			if l.exact {
				shares, err := decFromRat(value)
				if err != nil {
					return err
				}
				p.setDec(shares)
				l.join(key[0], key[1])
			}
			l.rateOf(key[1])
			dirty[key[0]] = true
		}
		{
			rows, err := db.QueryContext(ctx, `SELECT delegator, validator, creation_height, completion_time, balance::text FROM legacy.genesis_unbondings
	WHERE chain_id = $1 ORDER BY delegator, validator, entry`, chain)
			if err != nil {
				return err
			}
			for rows.Next() {
				var delegator, validator, balance string
				var height int64
				var completion time.Time
				if err := rows.Scan(&delegator, &validator, &height, &completion, &balance); err != nil {
					rows.Close()
					return err
				}
				amount, err := whole(balance)
				if err != nil {
					rows.Close()
					return err
				}
				w := l.at(delegator)
				w.entries[validator] = append(w.entries[validator], &entry{balance: amount, completion: completion, height: -1, stated: true})
				dirty[delegator] = true
			}
			if err := finish(rows); err != nil {
				return err
			}
		}

		day := 0
		flush := func(index int) error {
			final := index == len(days)-1
			if l.exact {
				l.recheck(dirty)
			}
			if final && !l.exact {
				for address, w := range l.wallets {
					for validator := range w.pairs {
						if l.rateOf(validator).moved {
							dirty[address] = true
						}
					}
				}
			}
			addresses := make([]string, 0, len(dirty))
			for address := range dirty {
				addresses = append(addresses, address)
			}
			sort.Strings(addresses)
			wrote := false
			for _, address := range addresses {
				w := l.wallets[address]
				if w == nil || w.module {
					continue
				}
				text, negative, err := l.holdings(w, final)
				if err != nil {
					return err
				}
				if negative {
					out.NegativeDayEnds++
					if Trace != nil && emit {
						Trace("negative %s day=%s holdings=%s", short(address), days[index].day.Format("2006-01-02"), redact(text))
					}
				}
				if !emit || text == w.stored || (!w.written && text == "{}") {
					continue
				}
				if !w.written {
					out.Wallets++
				}
				w.stored, w.written = text, true
				if err := sink(Row{Address: address, Day: days[index].day, Height: days[index].last, Holdings: text}); err != nil {
					return err
				}
				out.Rows++
				wrote = true
			}
			if wrote {
				out.DaysStored++
			}
			dirty = map[string]bool{}
			return nil
		}
		nextCheck := CheckpointEvery
		advance := func(height int64) error {
			for CheckpointEvery > 0 && nextCheck < height && nextCheck <= out.Through {
				l.checkpoint(nextCheck)
				nextCheck += CheckpointEvery
			}
			for days[day].last < height {
				if len(dirty) > 0 || len(l.touched) > 0 {
					if err := flush(day); err != nil {
						return err
					}
				}
				day++
			}
			return nil
		}

		held, burned, tracked := books.GovernanceAccounts(chain)
		events, err := db.QueryContext(ctx, `SELECT height, phase, tx_index, msg_index, category, amount, denom, from_addr, to_addr, validator_address, detail, "timestamp"
	FROM legacy.taxable_events WHERE chain_id = $1 AND height <= $2 ORDER BY height, phase, tx_index, msg_index, sub_index`, chain, out.Through)
		if err != nil {
			return err
		}
		defer events.Close()
		fees, err := db.QueryContext(ctx, `SELECT height, fee, fee_payer FROM legacy.tx_inclusions
	WHERE chain_id = $1 AND height <= $2 AND fee_charged AND fee <> '' ORDER BY height, tx_index`, chain, out.Through)
		if err != nil {
			return err
		}
		defer fees.Close()
		read := func(rows *sql.Rows, fee bool) (*move, error) {
			if !rows.Next() {
				return nil, rows.Err()
			}
			m := &move{fee: fee}
			if fee {
				return m, rows.Scan(&m.height, &m.amount, &m.from)
			}
			return m, rows.Scan(&m.height, &m.phase, &m.tx, &m.msg, &m.category, &m.amount, &m.unit, &m.from, &m.to, &m.validator, &m.detail, &m.when)
		}
		event, err := read(events, false)
		if err != nil {
			return err
		}
		added, err := extraMoves(ctx, db, chain)
		if err != nil {
			return err
		}
		nextAdded := 0
		// A stored row runs before an added movement of the same message.
		before := func(a, b *move) bool {
			if a.height != b.height {
				return a.height < b.height
			}
			if a.phase != b.phase {
				return a.phase < b.phase
			}
			if a.tx != b.tx {
				return a.tx < b.tx
			}
			return a.msg < b.msg
		}
		charge, err := read(fees, true)
		if err != nil {
			return err
		}
		touch := func(address string) *wallet {
			dirty[address] = true
			return l.at(address)
		}
		// A block's slashes run in its begin block, before its fees and
		// messages.
		next := 0
		slashUntil := func(height int64, phase int) error {
			for next < len(slashes) && (slashes[next].height < height || (slashes[next].height == height && phase >= 2)) {
				if err := advance(slashes[next].height); err != nil {
					return err
				}
				l.applySlash(slashes[next], touch)
				next++
			}
			return nil
		}
		for event != nil || charge != nil || nextAdded < len(added) {
			head := event
			if nextAdded < len(added) && (head == nil || before(added[nextAdded], head)) {
				head = added[nextAdded]
			}
			// A block's fees are charged before its messages run.
			if charge != nil && (head == nil || charge.height <= head.height) {
				if err := slashUntil(charge.height, 2); err != nil {
					return err
				}
				if err := advance(charge.height); err != nil {
					return err
				}
				amounts, units, err := books.FeeCoins(charge.amount)
				if err != nil {
					return err
				}
				for i, amount := range amounts {
					switch {
					case amount.Sign() == 0:
					case units[i] != denom:
						out.OtherDenomRows++
					default:
						out.Fees++
						w := touch(charge.from)
						w.liquid.Sub(w.liquid, amount)
					}
				}
				if charge, err = read(fees, true); err != nil {
					return err
				}
				continue
			}
			m := head
			if m.added != nil {
				if err := l.placeAdded(m); err != nil {
					return err
				}
			}
			if err := slashUntil(m.height, m.phase); err != nil {
				return err
			}
			if err := advance(m.height); err != nil {
				return err
			}
			if err := l.apply(m, out, touch, held, burned, tracked); err != nil {
				return err
			}
			if m != event {
				nextAdded++
				out.EventRows--
				out.AddedMoves++
				continue
			}
			if event, err = read(events, false); err != nil {
				return err
			}
		}
		if err := slashUntil(out.Through+1, 0); err != nil {
			return err
		}
		for CheckpointEvery > 0 && nextCheck <= out.Through {
			l.checkpoint(nextCheck)
			nextCheck += CheckpointEvery
		}
		for key, paid := range l.instant {
			out.CompletesUnmatched++
			if Trace != nil {
				Trace("complete unmatched %s %s height=%d paid=%s with no undelegation in the message", short(key.delegator), short(key.validator), key.height, paid)
			}
		}
		for day < len(days)-1 {
			if len(dirty) > 0 || len(l.touched) > 0 {
				if err := flush(day); err != nil {
					return err
				}
			}
			day++
		}
		if err := flush(len(days) - 1); err != nil {
			return err
		}
		return nil
	}
	if exact {
		l := newLedger()
		out.ExactReplay = true
		if err := pass(l, out, true, sink); err != nil {
			return nil, err
		}
		if err := l.gates(ctx, db, out, closing); err != nil {
			return nil, err
		}
		l.exactGate(out, closingVals, closingShares)
		out.SlashesApplied, out.SlashedEntries, out.SlashedRedelegations = l.slashesApplied, l.slashedEntries, l.slashedRedelegations
		out.ExactAnomalies = l.exactAnomalies
		if l.slashesApplied != len(slashes) {
			out.ExactAnomalies++
		}
		return out, nil
	}
	forward := newLedger()
	forward.trails = map[[2]string][]contribution{}
	if err := pass(forward, out, false, nil); err != nil {
		return nil, err
	}
	if err := forward.gates(ctx, db, out, closing); err != nil {
		return nil, err
	}
	// The second pass writes the days, with every delegation on a validator
	// whose price moved bounded backwards from the next genesis file too.
	second := newLedger()
	second.anchors = map[[2]string]*anchor{}
	for key, trail := range forward.trails {
		end := closingShares[key]
		if end == nil {
			end = new(big.Rat)
		}
		second.anchors[key] = newAnchor(end, trail)
	}
	shown := &Result{Chain: chain, Next: out.Next, Through: out.Through, CompletesSlashedAmount: new(big.Int),
		Delegations: Gate{OutsideAmount: new(big.Int), MaxBound: new(big.Int)},
		Unbonding:   Gate{OutsideAmount: new(big.Int), MaxBound: new(big.Int)}}
	if err := pass(second, shown, true, sink); err != nil {
		return nil, err
	}
	out.Rows, out.Wallets, out.DaysStored, out.NegativeDayEnds = shown.Rows, shown.Wallets, shown.DaysStored, shown.NegativeDayEnds
	out.AnchoredPairs, out.AnchorNarrowed, out.AnchorRounding, out.AnchorConflicts =
		len(second.anchors), second.anchorNarrowed, second.anchorRounding, second.anchorConflicts
	for key, a := range second.anchors {
		if a.seen != len(a.sufLo)-1 {
			out.AnchorConflicts++
			if Trace != nil {
				Trace("anchor count %s %s first=%d second=%d", short(key[0]), short(key[1]), len(a.sufLo)-1, a.seen)
			}
		}
	}
	return out, nil
}

func (l *ledger) apply(m *move, out *Result, touch func(string) *wallet, held, burned string, tracked bool) error {
	if m.category == books.Prop29Category {
		// What the next genesis file did; nothing moved on this chain.
		out.ChangeRows++
		return nil
	}
	rule, known := books.LiquidRule(m.category)
	if !known {
		return fmt.Errorf("a stored row has a category this build has no rule for")
	}
	if m.unit != denom {
		out.OtherDenomRows++
		return nil
	}
	value, err := whole(m.amount)
	if err != nil {
		return err
	}
	out.EventRows++
	paidTo, err := books.PaidTo(m.category, m.detail)
	if err != nil {
		return err
	}
	if paidTo != "" {
		w := touch(paidTo)
		w.liquid.Add(w.liquid, value)
		return nil
	}
	for _, side := range []struct {
		address string
		sign    int
	}{{m.from, rule[0]}, {m.to, rule[1]}} {
		if side.sign == 0 || side.address == "" {
			continue
		}
		w := touch(side.address)
		if side.sign > 0 {
			w.liquid.Add(w.liquid, value)
		} else {
			w.liquid.Sub(w.liquid, value)
		}
	}
	if deposit, moves := books.DepositRule(m.category); moves && tracked {
		for _, side := range []struct {
			address string
			sign    int
		}{{held, deposit[0]}, {burned, deposit[1]}} {
			if side.sign == 0 {
				continue
			}
			w := touch(side.address)
			if side.sign > 0 {
				w.liquid.Add(w.liquid, value)
			} else {
				w.liquid.Sub(w.liquid, value)
			}
		}
	}
	switch m.category {
	case "delegate":
		if m.validator == "" || m.from == "" {
			return fmt.Errorf("a delegation row names no validator or delegator")
		}
		if l.exact {
			l.delegate(touch(m.from), m.from, m.validator, sdkmath.NewIntFromBigInt(value))
			return nil
		}
		l.bond(touch(m.from), m.from, m.validator, 1, value)
	case "undelegate":
		if m.validator == "" || m.to == "" {
			return fmt.Errorf("an undelegation row names no validator or delegator")
		}
		w := touch(m.to)
		released := value
		if l.exact {
			released = l.release(w, m, value).BigInt()
		} else {
			l.bond(w, m.to, m.validator, -1, value)
		}
		key := instantKey{m.to, m.validator, m.height, m.tx, m.msg}
		if paid := l.instant[key]; paid != nil && paid.Cmp(released) == 0 {
			delete(l.instant, key)
			out.CompletesInstant++
			return nil
		}
		completion := m.when.Add(unbondingTime)
		stated := false
		if m.detail != "" {
			var u undelegation
			if json.Unmarshal([]byte(m.detail), &u) != nil {
				return fmt.Errorf("an undelegation row carries a detail that is not readable")
			}
			if u.Completion != "" {
				parsed, err := time.Parse(time.RFC3339Nano, u.Completion)
				if err != nil {
					return fmt.Errorf("an undelegation row carries a completion time that is not readable")
				}
				completion = parsed
				stated = true
			}
		}
		if l.exact {
			w.entries[m.validator] = append(w.entries[m.validator], &entry{balance: released, completion: completion, height: m.height, stated: stated,
				initial: new(big.Int).Set(released)})
			return nil
		}
		var slack int64
		if lo, hi := l.rateOf(m.validator).at(false); lo.Cmp(big.NewRat(1, 1)) != 0 || hi.Cmp(big.NewRat(1, 1)) != 0 {
			slack = 1
		}
		w.entries[m.validator] = append(w.entries[m.validator], &entry{balance: value, completion: completion, height: m.height, stated: stated, slack: slack})
	case "redelegate":
		var r redelegation
		if json.Unmarshal([]byte(m.detail), &r) != nil || r.Source == "" || r.Destination == "" || m.from == "" {
			return fmt.Errorf("a redelegation row names no source or destination validator")
		}
		w := touch(m.from)
		if l.exact {
			mm := *m
			mm.validator = r.Source
			released := l.release(w, &mm, value)
			if released.IsZero() {
				l.exactAnomalies++
				return nil
			}
			issued := l.delegate(w, m.from, r.Destination, released)
			if r.Completion == "" {
				if !l.timedRedelegations {
					return fmt.Errorf("a redelegation row carries no completion time")
				}
				// Hubs 1 and 2 store no completion time: the chain set it to
				// the block time plus the unbonding time.
				r.Completion = m.when.Add(unbondingTime).Format(time.RFC3339Nano)
			}
			if strings.HasPrefix(r.Completion, "0001-") {
				// From a validator that was not bonded: no entry.
				return nil
			}
			completion, err := time.Parse(time.RFC3339Nano, r.Completion)
			if err != nil {
				return fmt.Errorf("a redelegation row carries a completion time that is not readable")
			}
			l.reds[r.Source] = append(l.reds[r.Source], &redEntry{delegator: m.from, destination: r.Destination, height: m.height,
				completion: completion, initial: released, sharesDst: issued})
			return nil
		}
		l.bond(w, m.from, r.Source, -1, value)
		l.bond(w, m.from, r.Destination, 1, value)
	case "unbonding_complete":
		if m.validator == "" || m.to == "" {
			return fmt.Errorf("a completed unbonding row names no validator or delegator")
		}
		l.complete(touch(m.to), m, value, out)
	}
	return nil
}

// complete takes the entries a completed unbonding paid out. The chain pays
// every entry of the delegator and validator that is due at once. An entry
// whose completion time the stored rows did not give is due at the latest 21
// days after it was made, earlier when its validator was unbonding, and at
// once when its validator was not bonded. An entry may hold up to its slack
// less than tracked.
func (l *ledger) complete(w *wallet, m *move, value *big.Int, out *Result) {
	entries := w.entries[m.validator]
	fits := func(set []bool) (bool, bool) {
		sum, slack, any := new(big.Int), int64(0), false
		for i, take := range set {
			if take {
				sum.Add(sum, entries[i].balance)
				slack += entries[i].slack
				any = true
			}
		}
		over := new(big.Int).Sub(sum, value)
		return any && over.Sign() >= 0 && over.Cmp(big.NewInt(slack)) <= 0, over.Sign() == 0
	}
	take := func(set []bool) {
		rest := []*entry{}
		for i, e := range entries {
			if !set[i] {
				rest = append(rest, e)
			}
		}
		w.entries[m.validator] = rest
	}
	due := make([]bool, len(entries))
	for i, e := range entries {
		due[i] = !e.completion.After(m.when)
	}
	if ok, exact := fits(due); ok {
		out.CompletesExact++
		if !exact {
			out.CompletesRounding++
		}
		take(due)
		return
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].height == m.height && entries[i].balance.Cmp(value) == 0 {
			out.CompletesSameBlock++
			set := make([]bool, len(entries))
			set[i] = true
			take(set)
			return
		}
	}
	// Some other set of entries: an entry due by its latest time must be
	// paid unless its time was only estimated, and an entry that is not due
	// yet can be paid early only when its time was estimated. Of the sets
	// that fit, the one that leaves the fewest of those rules aside wins,
	// then the one with the oldest entries.
	if n := len(entries); n > 0 && n <= 16 {
		var best []bool
		bestCost := -1
		for mask := 1; mask < 1<<n; mask++ {
			set := make([]bool, n)
			cost, allowed := 0, true
			for i := range entries {
				set[i] = mask&(1<<i) != 0
				if set[i] != due[i] {
					if entries[i].stated {
						allowed = false
						break
					}
					cost++
				}
			}
			if !allowed {
				continue
			}
			if ok, _ := fits(set); ok && (bestCost < 0 || cost < bestCost) {
				best, bestCost = set, cost
			}
		}
		if best != nil {
			out.CompletesEarly++
			take(best)
			return
		}
	}
	sum := new(big.Int)
	for i, e := range entries {
		if due[i] {
			sum.Add(sum, e.balance)
		}
	}
	key := instantKey{m.to, m.validator, m.height, m.tx, m.msg}
	if m.phase == 2 && l.instant[key] == nil {
		// Paid at once by the undelegation the next stored row of this
		// message shows.
		l.instant[key] = new(big.Int).Set(value)
		return
	}
	if sum.Sign() > 0 && sum.Cmp(value) > 0 && l.rateOf(m.validator).moved {
		out.CompletesSlashed++
		out.CompletesSlashedAmount.Add(out.CompletesSlashedAmount, new(big.Int).Sub(sum, value))
		take(due)
		return
	}
	out.CompletesUnmatched++
	if Trace != nil {
		var held []string
		for _, e := range entries {
			held = append(held, fmt.Sprintf("%s@%d due %s stated=%t", e.balance, e.height, e.completion.UTC().Format(time.RFC3339), e.stated))
		}
		Trace("complete unmatched %s %s height=%d phase=%d time=%s paid=%s entries=[%s]", short(m.to), short(m.validator), m.height, m.phase,
			m.when.UTC().Format(time.RFC3339Nano), value, strings.Join(held, "; "))
	}
	take(due)
}

func (g *Gate) count(difference, bound *big.Int, ops int) string {
	g.Pairs++
	if bound.Sign() > 0 {
		g.PairsBounded++
		if bound.Cmp(g.MaxBound) > 0 {
			g.MaxBound.Set(bound)
		}
	}
	size := new(big.Int).Abs(difference)
	switch {
	case size.Sign() == 0:
		g.Exact++
		return StatusClosed
	case size.Cmp(big.NewInt(int64(ops))) <= 0:
		g.Rounding++
		return StatusRounding
	case size.Cmp(new(big.Int).Add(bound, big.NewInt(int64(ops)))) <= 0:
		g.WithinBound++
		return StatusBound
	default:
		g.Outside++
		g.OutsideAmount.Add(g.OutsideAmount, size)
		return StatusOpen
	}
}

var statusRank = map[string]int{StatusClosed: 0, StatusRounding: 1, StatusBound: 2, StatusOpen: 3}

// gates compares the state at the export height with the next genesis file.
func (l *ledger) gates(ctx context.Context, db *sql.DB, out *Result, closing *genesis) error {
	change := map[string]*big.Int{}
	rows, err := db.QueryContext(ctx, `SELECT from_addr, to_addr, amount FROM legacy.taxable_events
WHERE chain_id = $1 AND category = $2`, out.Chain, books.Prop29Category)
	if err != nil {
		return err
	}
	for rows.Next() {
		var from, to, amount string
		if err := rows.Scan(&from, &to, &amount); err != nil {
			rows.Close()
			return err
		}
		value, err := whole(amount)
		if err != nil {
			rows.Close()
			return err
		}
		if from != "" {
			if change[from] == nil {
				change[from] = new(big.Int)
			}
			change[from].Sub(change[from], value)
		}
		if to != "" {
			if change[to] == nil {
				change[to] = new(big.Int)
			}
			change[to].Add(change[to], value)
		}
	}
	if err := finish(rows); err != nil {
		return err
	}
	held, burned, tracked := books.GovernanceAccounts(out.Chain)
	computedLiquid := map[string]*big.Int{}
	addresses := make([]string, 0, len(l.wallets))
	for address := range l.wallets {
		addresses = append(addresses, address)
	}
	for address := range closing.liquid {
		if l.wallets[address] == nil {
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		w := l.wallets[address]
		if (w != nil && w.module) || closing.module[address] {
			out.ModuleAccounts++
			continue
		}
		computed := new(big.Int)
		if w != nil {
			computed.Set(w.liquid)
		}
		computedLiquid[address] = computed
		expected, inClosing := closing.liquid[address]
		if expected == nil {
			expected = new(big.Int)
		}
		out.LiquidCompared++
		difference := new(big.Int).Sub(expected, computed)
		switch {
		case difference.Sign() == 0:
			out.LiquidExact++
		case tracked && !inClosing && (address == held || address == burned):
			// The next chain kept governance deposits in a module account
			// and did not carry these two accounts.
			out.LiquidDeposit++
		case change[address] != nil && change[address].Cmp(difference) == 0:
			out.LiquidChange++
		default:
			out.LiquidOpen++
		}
	}
	lines, err := db.QueryContext(ctx, `SELECT address, computed::text FROM legacy.reconciliation WHERE chain_id = $1 AND component = 'liquid'`, out.Chain)
	if err != nil {
		return err
	}
	for lines.Next() {
		var address, computed string
		if err := lines.Scan(&address, &computed); err != nil {
			lines.Close()
			return err
		}
		out.StoredLiquidLines++
		value, ok := new(big.Rat).SetString(computed)
		if ok && computedLiquid[address] != nil && value.Cmp(new(big.Rat).SetInt(computedLiquid[address])) == 0 {
			out.StoredLiquidEqual++
		}
	}
	if err := finish(lines); err != nil {
		return err
	}

	type total struct {
		expected, computed *big.Int
		status             string
	}
	totals := map[[2]string]*total{}
	add := func(address, component string, expected, computed *big.Int, status string) {
		key := [2]string{address, component}
		t := totals[key]
		if t == nil {
			t = &total{expected: new(big.Int), computed: new(big.Int), status: StatusClosed}
			totals[key] = t
		}
		t.expected.Add(t.expected, expected)
		t.computed.Add(t.computed, computed)
		if statusRank[status] > statusRank[t.status] {
			t.status = status
		}
	}
	seen := map[[2]string]bool{}
	for _, address := range addresses {
		w := l.wallets[address]
		if w == nil || w.module {
			continue
		}
		for validator, p := range w.pairs {
			key := [2]string{address, validator}
			seen[key] = true
			amount, bound := l.shown(validator, p, true)
			expected := closing.delegations[key]
			if expected == nil {
				expected = new(big.Int)
			}
			if amount.Sign() == 0 && bound.Sign() == 0 && expected.Sign() == 0 {
				continue
			}
			status := out.Delegations.count(new(big.Int).Sub(expected, amount), bound, p.ops)
			if Trace != nil && status != StatusClosed {
				r := l.rateOf(validator)
				Trace("delegation %s %s status=%s expected=%s computed=%s bound=%s ops=%d moved=%t removed=%t start=%s end=%s shares=%s..%s",
					short(address), short(validator), status, expected, amount, bound, p.ops, r.moved, r.removed,
					r.start.FloatString(9), r.end.FloatString(9), p.lo.FloatString(6), p.hi.FloatString(6))
			}
			add(address, ComponentDelegations, expected, amount, status)
		}
	}
	for key, expected := range closing.delegations {
		if seen[key] || closing.module[key[0]] {
			continue
		}
		status := out.Delegations.count(expected, new(big.Int), 0)
		add(key[0], ComponentDelegations, expected, new(big.Int), status)
	}
	seen = map[[2]string]bool{}
	for _, address := range addresses {
		w := l.wallets[address]
		if w == nil || w.module {
			continue
		}
		for validator, entries := range w.entries {
			key := [2]string{address, validator}
			seen[key] = true
			sum, bound := new(big.Int), new(big.Int)
			for _, e := range entries {
				sum.Add(sum, e.balance)
				bound.Add(bound, l.entryBound(validator, e.balance))
				bound.Add(bound, big.NewInt(e.slack))
			}
			expected := closing.unbonding[key]
			if expected == nil {
				expected = new(big.Int)
			}
			if sum.Sign() == 0 && expected.Sign() == 0 {
				continue
			}
			status := out.Unbonding.count(new(big.Int).Sub(expected, sum), bound, 0)
			add(address, ComponentUnbonding, expected, sum, status)
		}
	}
	for key, expected := range closing.unbonding {
		if seen[key] || closing.module[key[0]] {
			continue
		}
		status := out.Unbonding.count(expected, new(big.Int), 0)
		add(key[0], ComponentUnbonding, expected, new(big.Int), status)
	}
	keys := make([][2]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a][1] != keys[b][1] {
			return keys[a][1] < keys[b][1]
		}
		return keys[a][0] < keys[b][0]
	})
	for _, key := range keys {
		t := totals[key]
		out.Lines = append(out.Lines, Line{Address: key[0], Component: key[1], Expected: t.expected, Computed: t.computed, Status: t.status})
	}
	return nil
}

// Trace, when set, receives one line per pair that does not close exactly,
// with addresses shortened to a hash.
var Trace func(format string, args ...any)

// redact replaces the addresses in a holdings text with their hashes.
func redact(text string) string {
	return addressPattern.ReplaceAllStringFunc(text, short)
}

var addressPattern = regexp.MustCompile(`cosmos(valoper)?1[0-9a-z]{38,}`)

func short(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:4])
}

// Write replaces a chain's rows in legacy.balance_days inside tx.
func Write(ctx context.Context, tx *sql.Tx, chain string, rows []Row) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM legacy.balance_days WHERE chain_id = $1`, chain); err != nil {
		return err
	}
	return Append(ctx, tx, chain, rows)
}

// Append inserts rows into legacy.balance_days inside tx.
func Append(ctx context.Context, tx *sql.Tx, chain string, rows []Row) error {
	const batch = 2000
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		var text strings.Builder
		text.WriteString(`INSERT INTO legacy.balance_days (chain_id, address, day, height, holdings) VALUES `)
		args := []any{chain}
		for i, row := range rows[start:end] {
			if i > 0 {
				text.WriteString(",")
			}
			n := len(args)
			fmt.Fprintf(&text, "($1, $%d, $%d::date, $%d, $%d::jsonb)", n+1, n+2, n+3, n+4)
			args = append(args, row.Address, row.Day.Format("2006-01-02"), row.Height, row.Holdings)
		}
		if _, err := tx.ExecContext(ctx, text.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// Record replaces the chain's delegation and unbonding lines in
// legacy.reconciliation inside tx.
func Record(ctx context.Context, tx *sql.Tx, result *Result) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM legacy.reconciliation WHERE chain_id = $1 AND component IN ($2, $3)`,
		result.Chain, ComponentDelegations, ComponentUnbonding); err != nil {
		return err
	}
	const batch = 2000
	for start := 0; start < len(result.Lines); start += batch {
		end := start + batch
		if end > len(result.Lines) {
			end = len(result.Lines)
		}
		var text strings.Builder
		text.WriteString(`INSERT INTO legacy.reconciliation (chain_id, address, component, expected, computed, difference, status) VALUES `)
		args := []any{result.Chain}
		for i, line := range result.Lines[start:end] {
			if i > 0 {
				text.WriteString(",")
			}
			n := len(args)
			fmt.Fprintf(&text, "($1, $%d, $%d, $%d::numeric, $%d::numeric, $%d::numeric - $%d::numeric, $%d)", n+1, n+2, n+3, n+4, n+3, n+4, n+5)
			args = append(args, line.Address, line.Component, line.Expected.String(), line.Computed.String(), line.Status)
		}
		if _, err := tx.ExecContext(ctx, text.String(), args...); err != nil {
			return err
		}
	}
	return nil
}
