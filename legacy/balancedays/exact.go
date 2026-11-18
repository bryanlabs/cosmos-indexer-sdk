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

// Every legacy chain is replayed exactly: each validator's tokens and shares
// and each delegation's shares follow the arithmetic of the chain's staking
// module, so no price range is needed. The stored block events of
// cosmoshub-1 and cosmoshub-2 do not hold their slashes, the share amounts
// their messages named or the movements that released no tokens; the caller
// adds those through ExtraSlashes, MessageShares, MessageTokens and
// ExtraMoves.
var exactChains = map[string]bool{"cosmoshub-1": true, "cosmoshub-2": true, "cosmoshub-3": true}

// untimedRedelegations are the chains whose stored redelegation rows carry
// no completion time.
var untimedRedelegations = map[string]bool{"cosmoshub-1": true, "cosmoshub-2": true}

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
	if s.power == 0 {
		// An added slash that names no power takes the validator's
		// consensus power from its tokens when the slash runs.
		s.power = v.tokens.Quo(sdkmath.NewInt(1000000)).Int64()
		if Trace != nil {
			Trace("added slash %s height=%d power=%d", short(s.operator), s.height, s.power)
		}
	}
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

// ForceExact replays a chain exactly although its stored block events do not
// hold its slashes, so that its validators can be compared with an archive.
var ForceExact = map[string]bool{}

// SlashRecord is a slash the stored block events of a chain do not hold:
// the height whose begin block ran it, the validator, the power it was
// charged on and whether it was a double sign.
type SlashRecord struct {
	Height int64
	// Operator is the sha256 of the validator's operator address, in hex.
	Operator string
	Power    int64
	Double   bool
	// Infraction is the height the slash reaches back to; zero means the
	// downtime rule (the height less the update delay less one).
	Infraction int64
}

// ExtraSlashes holds, per chain, slashes added to the stored ones.
var ExtraSlashes = map[string][]SlashRecord{}

// Checkpoint, when set, receives every validator's tokens and shares at
// each height that is a multiple of CheckpointEvery, after that height's
// last movement, in an exact replay.
var (
	Checkpoint      func(height int64, operator, tokens, shares string)
	CheckpointEvery int64 = 10000
)

func (l *ledger) checkpoint(height int64) {
	if Checkpoint == nil || !l.exact {
		return
	}
	operators := make([]string, 0, len(l.vals))
	for operator := range l.vals {
		operators = append(operators, operator)
	}
	sort.Strings(operators)
	for _, operator := range operators {
		v := l.vals[operator]
		Checkpoint(height, operator, v.tokens.String(), v.shares.String())
	}
}

func extraSlashes(ctx context.Context, db *sql.DB, chain string, operators []string) ([]*slash, error) {
	records := ExtraSlashes[chain]
	if len(records) == 0 {
		return nil, nil
	}
	byHash := map[string]string{}
	for _, operator := range operators {
		byHash[hashOf(operator)] = operator
	}
	out := make([]*slash, 0, len(records))
	for i, r := range records {
		operator, ok := byHash[r.Operator]
		if !ok {
			return nil, fmt.Errorf("an added slash names a validator this build cannot place")
		}
		var when time.Time
		if err := db.QueryRowContext(ctx, `SELECT time FROM legacy.blocks WHERE chain_id = $1 AND height = $2`, chain, r.Height).Scan(&when); err != nil {
			return nil, fmt.Errorf("an added slash names a height with no stored block")
		}
		s := &slash{height: r.Height, index: -1000 + i, when: when, operator: operator, power: r.Power, double: r.Double, infraction: r.Infraction}
		if r.Double {
			s.fraction = fractionDoubleSign
		} else {
			s.fraction = fractionDowntime
			if s.infraction == 0 {
				s.infraction = r.Height - updateDelay - 1
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// MessageShares holds, per chain, the shares each undelegation and
// redelegation message named, keyed by height, transaction and message.
// cosmoshub-1 messages named shares; its stored rows hold the tokens the
// chain paid out.
var MessageShares = map[string]map[[3]int64]string{}

// MessageTokens holds, per chain, the token amount each undelegation and
// redelegation message named, keyed by height, transaction and message.
var MessageTokens = map[string]map[[3]int64]string{}

func messageTokens(chain string) (map[[3]int64]sdkmath.Int, error) {
	named := MessageTokens[chain]
	if len(named) == 0 {
		return nil, nil
	}
	out := make(map[[3]int64]sdkmath.Int, len(named))
	for key, text := range named {
		n, ok := sdkmath.NewIntFromString(text)
		if !ok || n.IsNegative() {
			return nil, fmt.Errorf("a message token amount is not readable")
		}
		out[key] = n
	}
	return out, nil
}

func messageShares(chain string) (map[[3]int64]sdkmath.LegacyDec, error) {
	named := MessageShares[chain]
	if len(named) == 0 {
		return nil, nil
	}
	out := make(map[[3]int64]sdkmath.LegacyDec, len(named))
	for key, text := range named {
		d, err := sdkmath.LegacyNewDecFromStr(text)
		if err != nil {
			return nil, fmt.Errorf("a message share amount is not readable")
		}
		out[key] = d
	}
	return out, nil
}

// release unbonds what a stored undelegation or redelegation row moved:
// the shares its message named when the chain's messages named shares,
// otherwise the tokens the row holds.
func (l *ledger) release(w *wallet, m *move, value *big.Int) sdkmath.Int {
	shares, named := l.msgShares[[3]int64{m.height, int64(m.tx), int64(m.msg)}]
	if !named {
		if l.msgShares != nil {
			l.exactAnomalies++
			if Trace != nil {
				Trace("exact unbond with no message shares height=%d", m.height)
			}
		}
		amount, stated := l.msgTokens[[3]int64{m.height, int64(m.tx), int64(m.msg)}]
		if !stated {
			if l.msgTokens != nil {
				l.exactAnomalies++
				if Trace != nil {
					Trace("exact unbond with no message amount height=%d", m.height)
				}
			}
			return l.undelegate(w, m.validator, sdkmath.NewIntFromBigInt(value))
		}
		// The message named a token amount; the chain turned it into shares
		// and paid out what those shares were worth, which the stored row
		// holds and can be a uatom or two less.
		issued := l.undelegate(w, m.validator, amount)
		if issued.BigInt().Cmp(value) != 0 {
			l.exactAnomalies++
			if Trace != nil {
				Trace("exact unbond pays other than the stored row %s height=%d paid=%s stored=%s", short(m.validator), m.height, issued, value)
			}
		}
		return issued
	}
	p := w.pairOf(m.validator)
	p.ops++
	if held := p.dec(); shares.GT(held) {
		l.exactAnomalies++
		if Trace != nil {
			Trace("exact unbond over %s shares=%s held=%s", short(m.validator), shares, held)
		}
		shares = held
	}
	if l.val(m.validator).tokens.IsZero() {
		l.exactAnomalies++
		return sdkmath.ZeroInt()
	}
	issued := l.unbond(m.validator, p, shares)
	if issued.BigInt().Cmp(value) != 0 {
		l.exactAnomalies++
		if Trace != nil {
			Trace("exact unbond pays other than the stored row %s height=%d paid=%s stored=%s", short(m.validator), m.height, issued, value)
		}
	}
	return issued
}

// MoveRecord is an undelegation or redelegation the chain ran that the
// stored rows hold no row for: its message released no tokens (a uatom
// unbonded from a validator whose share price fell below one), so no taxable
// event was written, but its shares still left the delegation. Addresses are
// the sha256 of the address, in hex, placed against the replay's wallets and
// validators when the movement runs. Tokens is the amount the message named.
type MoveRecord struct {
	Height                                  int64
	Tx, Msg                                 int
	Kind, Delegator, Validator, Destination string
	Tokens, Completion                      string
}

// ExtraMoves holds, per chain, movements added to the stored rows, in
// height, transaction and message order.
var ExtraMoves = map[string][]MoveRecord{}

func extraMoves(ctx context.Context, db *sql.DB, chain string) ([]*move, error) {
	records := ExtraMoves[chain]
	out := make([]*move, 0, len(records))
	for i := range records {
		r := &records[i]
		if r.Kind != "undelegate" && r.Kind != "redelegate" {
			return nil, fmt.Errorf("an added movement is neither an undelegation nor a redelegation")
		}
		var when time.Time
		if err := db.QueryRowContext(ctx, `SELECT time FROM legacy.blocks WHERE chain_id = $1 AND height = $2`, chain, r.Height).Scan(&when); err != nil {
			return nil, fmt.Errorf("an added movement names a height with no stored block")
		}
		m := &move{height: r.Height, phase: 2, tx: r.Tx, msg: r.Msg, category: r.Kind, amount: "0", unit: denom, when: when, added: r}
		if n := len(out); n > 0 && (out[n-1].height > m.height || (out[n-1].height == m.height && (out[n-1].tx > m.tx || (out[n-1].tx == m.tx && out[n-1].msg >= m.msg)))) {
			return nil, fmt.Errorf("added movements are not in height, transaction and message order")
		}
		out = append(out, m)
	}
	return out, nil
}

// placeAdded names an added movement's delegator and validators: the
// delegator holds a delegation to the validator it moves from, so both are
// in the replay already.
func (l *ledger) placeAdded(m *move) error {
	r := m.added
	var delegator string
	for address := range l.wallets {
		if hashOf(address) == r.Delegator {
			delegator = address
			break
		}
	}
	validator := func(hash string) string {
		for operator := range l.vals {
			if hashOf(operator) == hash {
				return operator
			}
		}
		return ""
	}
	source := validator(r.Validator)
	if delegator == "" || source == "" {
		return fmt.Errorf("an added movement at height %d names a delegator or validator the replay does not hold", m.height)
	}
	var detail []byte
	var err error
	if r.Kind == "undelegate" {
		m.to, m.validator = delegator, source
		detail, err = json.Marshal(undelegation{Completion: r.Completion})
	} else {
		destination := validator(r.Destination)
		if destination == "" {
			return fmt.Errorf("an added redelegation at height %d names a destination the replay does not hold", m.height)
		}
		m.from = delegator
		detail, err = json.Marshal(redelegation{Source: source, Destination: destination, Completion: r.Completion})
	}
	if err != nil {
		return err
	}
	m.detail = string(detail)
	return nil
}
