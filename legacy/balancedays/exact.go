package balancedays

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/types/bech32"
)

// A chain whose stored history records every slash is replayed exactly: each
// validator's tokens and shares and each delegation's shares follow the
// arithmetic of the chain's staking module (Cosmos SDK v0.37 for
// cosmoshub-3), so no price range is needed.
var exactChains = map[string]bool{"cosmoshub-3": true}

// Slash fractions of cosmoshub-3, from the slashing parameters of its genesis
// file (unchanged in the next one).
var (
	fractionDowntime   = sdkmath.LegacyNewDecWithPrec(1, 4)
	fractionDoubleSign = sdkmath.LegacyNewDecWithPrec(5, 2)
)

// The staking module's validator update delay, in blocks.
const updateDelay = 1

type valState struct {
	tokens sdkmath.Int
	shares sdkmath.LegacyDec
}

type redEntry struct {
	delegator, destination string
	height                 int64
	completion             time.Time
	initial                sdkmath.Int
	sharesDst              sdkmath.LegacyDec
}

type slash struct {
	height     int64
	index      int
	when       time.Time
	operator   string
	power      int64
	fraction   sdkmath.LegacyDec
	infraction int64
	double     bool
}

func decFromRat(r *big.Rat) (sdkmath.LegacyDec, error) {
	scaled := new(big.Int).Mul(r.Num(), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	q, m := new(big.Int).QuoRem(scaled, r.Denom(), new(big.Int))
	if m.Sign() != 0 {
		return sdkmath.LegacyDec{}, fmt.Errorf("a stored share amount has more than 18 decimals")
	}
	return sdkmath.LegacyNewDecFromBigIntWithPrec(q, 18), nil
}

func ratFromDec(d sdkmath.LegacyDec) *big.Rat {
	return new(big.Rat).SetFrac(d.BigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

func (l *ledger) val(operator string) *valState {
	v := l.vals[operator]
	if v == nil {
		v = &valState{tokens: sdkmath.ZeroInt(), shares: sdkmath.LegacyZeroDec()}
		l.vals[operator] = v
	}
	return v
}

func (p *pair) dec() sdkmath.LegacyDec {
	if p.sh.IsNil() {
		return sdkmath.LegacyZeroDec()
	}
	return p.sh
}

func (p *pair) setDec(d sdkmath.LegacyDec) {
	p.sh = d
	r := ratFromDec(d)
	p.lo, p.hi = r, new(big.Rat).Set(r)
}

func (v *valState) tokensFromShares(shares sdkmath.LegacyDec) sdkmath.LegacyDec {
	if v.shares.IsZero() {
		return sdkmath.LegacyZeroDec()
	}
	return shares.MulInt(v.tokens).Quo(v.shares)
}

// delegate adds tokens to a delegation as the chain's AddTokensFromDel does.
func (l *ledger) delegate(w *wallet, address, validator string, amount sdkmath.Int) sdkmath.LegacyDec {
	v := l.val(validator)
	var issued sdkmath.LegacyDec
	switch {
	case v.shares.IsZero():
		issued = sdkmath.LegacyNewDecFromInt(amount)
	case v.tokens.IsZero():
		// The chain refuses this; a stored row that shows it is a history
		// this replay does not explain.
		l.exactAnomalies++
		issued = sdkmath.LegacyZeroDec()
	default:
		issued = v.shares.MulInt(amount).QuoInt(v.tokens)
	}
	v.tokens = v.tokens.Add(amount)
	v.shares = v.shares.Add(issued)
	p := w.pairOf(validator)
	p.setDec(p.dec().Add(issued))
	p.ops++
	l.join(address, validator)
	return issued
}

// undelegate takes a token amount from a delegation as ValidateUnbondAmount
// and unbond do, and returns the tokens the delegation released.
func (l *ledger) undelegate(w *wallet, validator string, amount sdkmath.Int) sdkmath.Int {
	v := l.val(validator)
	p := w.pairOf(validator)
	p.ops++
	if v.tokens.IsZero() {
		l.exactAnomalies++
		return sdkmath.ZeroInt()
	}
	shares := v.shares.MulInt(amount).QuoInt(v.tokens)
	truncated := v.shares.MulInt(amount).QuoTruncate(sdkmath.LegacyNewDecFromInt(v.tokens))
	held := p.dec()
	if truncated.GT(held) {
		l.exactAnomalies++
		if Trace != nil {
			Trace("exact unbond over %s amount=%s held=%s", short(validator), amount, held)
		}
	}
	if shares.GT(held) {
		shares = held
	}
	return l.unbond(validator, p, shares)
}

// unbond removes shares from a delegation and its validator as
// RemoveDelShares does.
func (l *ledger) unbond(validator string, p *pair, shares sdkmath.LegacyDec) sdkmath.Int {
	v := l.val(validator)
	l.touched[validator] = true
	p.setDec(p.dec().Sub(shares))
	remaining := v.shares.Sub(shares)
	var issued sdkmath.Int
	if remaining.IsZero() {
		issued = v.tokens
		v.tokens = sdkmath.ZeroInt()
	} else {
		issued = v.tokensFromShares(shares).TruncateInt()
		v.tokens = v.tokens.Sub(issued)
		if v.tokens.IsNegative() {
			l.exactAnomalies++
			v.tokens = sdkmath.ZeroInt()
		}
	}
	v.shares = remaining
	return issued
}

// join records that an address delegates to a validator, so a change of the
// validator's tokens or shares rechecks the address's figure at the day end.
func (l *ledger) join(address, validator string) {
	set := l.members[validator]
	if set == nil {
		set = map[string]bool{}
		l.members[validator] = set
	}
	set[address] = true
	l.touched[validator] = true
}

// recheck marks every address whose figure on a validator touched since the
// last day end changed.
func (l *ledger) recheck(dirty map[string]bool) {
	for validator := range l.touched {
		v := l.vals[validator]
		for address := range l.members[validator] {
			p := l.wallets[address].pairs[validator]
			if p == nil {
				continue
			}
			amount := new(big.Int)
			if v != nil {
				amount = v.tokensFromShares(p.dec()).TruncateInt().BigInt()
			}
			if p.last == nil || p.last.Cmp(amount) != 0 {
				p.last = amount
				dirty[address] = true
			}
		}
	}
	l.touched = map[string]bool{}
}

// applySlash takes a slash as the chain's Slash does: first from the
// unbonding and redelegation entries made at or after the infraction height
// that are not yet mature, the rest from the validator's tokens.
func (l *ledger) applySlash(s *slash, touch func(string) *wallet) {
	v := l.val(s.operator)
	amount := sdkmath.NewInt(s.power).Mul(sdkmath.NewInt(1000000))
	remaining := sdkmath.LegacyNewDecFromInt(amount).Mul(s.fraction).TruncateInt()
	if s.infraction < s.height {
		addresses := make([]string, 0)
		for address, w := range l.wallets {
			if len(w.entries[s.operator]) > 0 {
				addresses = append(addresses, address)
			}
		}
		sort.Strings(addresses)
		for _, address := range addresses {
			for _, e := range touch(address).entries[s.operator] {
				if e.height < s.infraction || !e.completion.After(s.when) || e.initial == nil {
					continue
				}
				cut := sdkmath.LegacyNewDecFromBigInt(e.initial).Mul(s.fraction).TruncateInt()
				remaining = remaining.Sub(cut)
				taken := cut.BigInt()
				if taken.Cmp(e.balance) > 0 {
					taken = new(big.Int).Set(e.balance)
				}
				e.balance = new(big.Int).Sub(e.balance, taken)
				l.slashedEntries++
			}
		}
		for _, r := range l.reds[s.operator] {
			if r.height < s.infraction || !r.completion.After(s.when) {
				continue
			}
			remaining = remaining.Sub(sdkmath.LegacyNewDecFromInt(r.initial).Mul(s.fraction).TruncateInt())
			shares := s.fraction.Mul(r.sharesDst)
			if shares.IsZero() {
				continue
			}
			w := touch(r.delegator)
			p := w.pairs[r.destination]
			if p == nil || p.dec().IsZero() {
				continue
			}
			if shares.GT(p.dec()) {
				shares = p.dec()
			}
			l.unbond(r.destination, p, shares)
			l.slashedRedelegations++
		}
	}
	kept := l.reds[s.operator][:0]
	for _, r := range l.reds[s.operator] {
		if r.completion.After(s.when) {
			kept = append(kept, r)
		}
	}
	l.reds[s.operator] = kept
	burn := sdkmath.MinInt(remaining, v.tokens)
	if burn.IsNegative() {
		burn = sdkmath.ZeroInt()
	}
	v.tokens = v.tokens.Sub(burn)
	l.touched[s.operator] = true
	l.slashesApplied++
}

// loadValidators reads a genesis file's validators in tokens and shares.
func loadValidators(ctx context.Context, db *sql.DB, chain string) (map[string]*valState, error) {
	out := map[string]*valState{}
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
		t, ok := sdkmath.NewIntFromString(tokens)
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("a stored validator token amount is not a whole number")
		}
		s, err := sdkmath.LegacyNewDecFromStr(shares)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("a stored validator share amount is not a number")
		}
		out[operator] = &valState{tokens: t, shares: s}
	}
	return out, finish(rows)
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

type evidenceRecord struct {
	Value struct {
		PubKey struct {
			Value string `json:"value"`
		} `json:"PubKey"`
		VoteA struct {
			Height string `json:"height"`
		} `json:"VoteA"`
	} `json:"value"`
}

// loadSlashes reads the slashes a chain's stored block events record, in
// order, each placed on its validator.
func loadSlashes(ctx context.Context, db *sql.DB, chain string, through int64, operators []string) ([]*slash, error) {
	byHash := map[string]string{}
	for _, operator := range operators {
		byHash[hashOf(operator)] = operator
	}
	place := func(consensus string) (string, error) {
		operator, ok := byHash[slashedOperators[hashOf(consensus)]]
		if !ok {
			return "", fmt.Errorf("a stored slash names a validator this build cannot place")
		}
		return operator, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT e.height, e.event_index, e.type, e.attributes::text, b.time
FROM legacy.block_events e JOIN legacy.blocks b ON b.chain_id = e.chain_id AND b.height = e.height
WHERE e.chain_id = $1 AND e.type IN ('slash', 'block_evidence') AND e.phase = 1 AND e.height <= $2
ORDER BY e.height, e.event_index`, chain, through)
	if err != nil {
		return nil, err
	}
	var out []*slash
	evidence := map[[2]any]int64{}
	type pending struct {
		s         *slash
		consensus string
	}
	var doubles []pending
	for rows.Next() {
		var height int64
		var index int
		var kind, text string
		var when time.Time
		if err := rows.Scan(&height, &index, &kind, &text, &when); err != nil {
			rows.Close()
			return nil, err
		}
		var attributes [][]string
		if err := json.Unmarshal([]byte(text), &attributes); err != nil {
			rows.Close()
			return nil, fmt.Errorf("a stored slash event is not readable")
		}
		values := map[string]string{}
		for _, a := range attributes {
			if len(a) == 2 {
				values[a[0]] = a[1]
			}
		}
		if kind == "block_evidence" {
			var e evidenceRecord
			if err := json.Unmarshal([]byte(values["json"]), &e); err != nil {
				rows.Close()
				return nil, fmt.Errorf("a stored evidence record is not readable")
			}
			key, err := base64.StdEncoding.DecodeString(e.Value.PubKey.Value)
			if err != nil || len(key) != 32 {
				rows.Close()
				return nil, fmt.Errorf("a stored evidence record has no consensus key")
			}
			sum := sha256.Sum256(key)
			consensus, err := bech32.ConvertAndEncode("cosmosvalcons", sum[:20])
			if err != nil {
				rows.Close()
				return nil, err
			}
			voted, err := strconv.ParseInt(e.Value.VoteA.Height, 10, 64)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("a stored evidence record has no height")
			}
			k := [2]any{height, consensus}
			if _, seen := evidence[k]; !seen {
				evidence[k] = voted
			}
			continue
		}
		reason := values["reason"]
		if reason == "" {
			// The jail that follows a double-sign slash.
			continue
		}
		power, err := strconv.ParseInt(values["power"], 10, 64)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("a stored slash has no power")
		}
		operator, err := place(values["address"])
		if err != nil {
			rows.Close()
			return nil, err
		}
		s := &slash{height: height, index: index, when: when, operator: operator, power: power}
		switch reason {
		case "missing_signature":
			s.fraction, s.infraction = fractionDowntime, height-updateDelay-1
		case "double_sign":
			s.fraction, s.double = fractionDoubleSign, true
			doubles = append(doubles, pending{s, values["address"]})
		default:
			rows.Close()
			return nil, fmt.Errorf("a stored slash has a reason this build does not know")
		}
		out = append(out, s)
	}
	if err := finish(rows); err != nil {
		return nil, err
	}
	for _, d := range doubles {
		voted, ok := evidence[[2]any{d.s.height, d.consensus}]
		if !ok {
			return nil, fmt.Errorf("a stored double-sign slash has no evidence in its block")
		}
		d.s.infraction = voted - updateDelay
	}
	return out, nil
}

// exactGate compares every validator's tokens and shares, and every
// delegation's shares, at the export height with the next genesis file.
func (l *ledger) exactGate(out *Result, closing map[string]*valState, closingShares map[[2]string]*big.Rat) {
	seen := map[[2]string]bool{}
	for address, w := range l.wallets {
		for validator, p := range w.pairs {
			key := [2]string{address, validator}
			seen[key] = true
			expected := closingShares[key]
			if expected == nil {
				expected = new(big.Rat)
			}
			computed := ratFromDec(p.dec())
			if computed.Sign() == 0 && expected.Sign() == 0 {
				continue
			}
			if computed.Cmp(expected) == 0 {
				out.SharesExact++
				continue
			}
			out.SharesDiffer++
			if Trace != nil {
				Trace("shares %s %s computed=%s expected=%s", short(address), short(validator), p.dec(), expected.FloatString(18))
			}
		}
	}
	for key, expected := range closingShares {
		if !seen[key] && expected.Sign() != 0 {
			out.SharesDiffer++
		}
	}
	names := map[string]bool{}
	for operator := range l.vals {
		names[operator] = true
	}
	for operator := range closing {
		names[operator] = true
	}
	for operator := range names {
		v := l.vals[operator]
		if v == nil {
			v = &valState{tokens: sdkmath.ZeroInt(), shares: sdkmath.LegacyZeroDec()}
		}
		c := closing[operator]
		if c == nil {
			c = &valState{tokens: sdkmath.ZeroInt(), shares: sdkmath.LegacyZeroDec()}
		}
		if v.tokens.Equal(c.tokens) && v.shares.Equal(c.shares) {
			out.ValidatorsExact++
			continue
		}
		out.ValidatorsDiffer++
		if Trace != nil {
			Trace("validator %s tokens=%s expected=%s shares=%s expected=%s", short(operator), v.tokens, c.tokens, v.shares, c.shares)
		}
	}
}
