package taxapi

import (
	"container/heap"
	"sort"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// The commission flow tracer follows one validator's withdrawn commission
// through the wallets that received it, and up to flowMaxHops wallets beyond,
// and says where each month's commission is at the as-of date: still held,
// restaked, sent to an exchange, sent over IBC, spent on fees, and so on.
//
// Once commission lands in a wallet that holds other coins, every outflow is
// a mix, so the answer depends on an attribution rule. The tracer runs two
// side by side and stores both: first in first out, where the oldest coins in
// a pool leave first, and pro-rata, where every outflow carries each origin
// in proportion to the pool. Coins that are not this validator's commission
// have the empty origin and are never reported.

const (
	flowRuleFIFO = iota
	flowRuleProRata
	flowRules
)

var flowRuleNames = [flowRules]string{"fifo", "pro_rata"}

// flowMaxHops is how many wallets past the receiving wallets the tracer
// follows. Coins leaving a wallet at this depth end as beyond_hops.
const flowMaxHops = 3

// Fate classes. A terminal class is recorded when coins leave the traced
// wallets; the others describe where coins sit at the as-of date.
const (
	flowHeld              = "held"
	flowRestakedOwn       = "restaked_own"
	flowRestakedOther     = "restaked_other"
	flowUnbonding         = "unbonding"
	flowLocked            = "locked"
	flowHeldDownstream    = "held_downstream"
	flowStakedDownstream  = "staked_downstream"
	flowUnbondDownstream  = "unbonding_downstream"
	flowLockedDownstream  = "locked_downstream"
	flowExchange          = "exchange"
	flowExchangeLike      = "exchange_like"
	flowIBCOut            = "ibc_out"
	flowFees              = "fees"
	flowSwapped           = "swapped"
	flowSpent             = "spent"
	flowTokenized         = "tokenized"
	flowBurned            = "burned"
	flowCommunityPool     = "community_pool"
	flowModule            = "module"
	flowContract          = "contract_or_ica"
	flowBeyondHops        = "beyond_hops"
	flowBusyWallet        = "busy_wallet"
	flowUnresolved        = "unresolved"
	flowOriginOther       = ""
	flowDust              = "0.000001"
	flowProRataPrecision  = 12
	flowViaUnbondingClass = "+unbonded"
)

// Categories the tracer moves coins for, beyond the shared vocabulary.
const (
	flowCatIBCOut             = string(tax.CategoryIBCOut)
	flowCatDelegate           = string(tax.CategoryDelegate)
	flowCatUndelegate         = string(tax.CategoryUndelegate)
	flowCatRedelegate         = string(tax.CategoryRedelegate)
	flowCatCancelUnbonding    = string(tax.CategoryCancelUnbonding)
	flowCatUnbondingComplete  = string(tax.CategoryUnbondingComplete)
	flowCatTokenize           = string(tax.CategoryTokenizeShares)
	flowCatRedeem             = string(tax.CategoryRedeemShares)
	flowCatProtocolDeposit    = string(tax.CategoryProtocolDeposit)
	flowCatProtocolWithdrawal = string(tax.CategoryProtocolWithdrawal)
	flowCatGovDeposit         = string(tax.CategoryGovDeposit)
	flowCatGovRefund          = string(tax.CategoryGovDepositRefund)
	flowCatGovBurn            = string(tax.CategoryGovDepositBurn)
)

var flowDustAmount = decimal.RequireFromString(flowDust)

// flowKey orders events across wallets: block time, height, then the
// message and its leg. Fees carry message 0 so they come first in a block.
type flowKey struct {
	Time    time.Time
	Height  int64
	Message uint
	Sub     int
}

func (k flowKey) before(o flowKey) bool {
	if !k.Time.Equal(o.Time) {
		return k.Time.Before(o.Time)
	}
	if k.Height != o.Height {
		return k.Height < o.Height
	}
	if k.Message != o.Message {
		return k.Message < o.Message
	}
	return k.Sub < o.Sub
}

// flowEvent is one movement of the native coin as one wallet sees it, in
// base units.
type flowEvent struct {
	Key          flowKey
	Category     string
	Dir          string
	Amount       decimal.Decimal
	From, To     string
	Validator    string
	SrcValidator string
	Trigger      string
	Detail       string
}

// flowHoldings is a wallet's native coin at the end of a day, in base units.
type flowHoldings struct {
	Liquid    decimal.Decimal
	Staked    map[string]decimal.Decimal
	Unbonding map[string]decimal.Decimal
}

func (h flowHoldings) total() decimal.Decimal {
	t := h.Liquid
	for _, v := range h.Staked {
		t = t.Add(v)
	}
	for _, v := range h.Unbonding {
		t = t.Add(v)
	}
	return t
}

// flowStop names the fate of coins sent to an address the tracer does not
// follow. An empty Class means the address is an ordinary wallet to trace.
type flowStop struct {
	Class  string
	Detail string
}

// flowLoader is everything the tracer reads. The database driver implements
// it; tests use a constructed ledger.
type flowLoader interface {
	// opening is addr's holdings at the end of day. known is false when the
	// index cannot rebuild that day; the wallet then starts empty.
	opening(addr string, day time.Time) (h flowHoldings, known bool, err error)
	events(addr string, start, end time.Time) ([]flowEvent, error)
	stop(addr string) (flowStop, error)
	ibcDestination(e flowEvent) string
}

type flowPart struct {
	Origin string
	Amount decimal.Decimal
}

// flowPool is one bucket of coins with their origins.
type flowPool struct {
	rule  int
	parts []flowPart
}

func (p *flowPool) total() decimal.Decimal {
	t := decimal.Zero
	for _, x := range p.parts {
		t = t.Add(x.Amount)
	}
	return t
}

func (p *flowPool) add(parts []flowPart) {
	for _, x := range parts {
		if !x.Amount.IsPositive() {
			continue
		}
		if p.rule == flowRuleProRata {
			merged := false
			for i := range p.parts {
				if p.parts[i].Origin == x.Origin {
					p.parts[i].Amount = p.parts[i].Amount.Add(x.Amount)
					merged = true
					break
				}
			}
			if !merged {
				p.parts = append(p.parts, x)
			}
			continue
		}
		if n := len(p.parts); n > 0 && p.parts[n-1].Origin == x.Origin {
			p.parts[n-1].Amount = p.parts[n-1].Amount.Add(x.Amount)
			continue
		}
		p.parts = append(p.parts, x)
	}
}

// take removes amount from the pool and returns what it carried. When the
// pool holds less, the shortfall is returned as coins of unknown origin.
func (p *flowPool) take(amount decimal.Decimal) (out []flowPart, short decimal.Decimal) {
	if !amount.IsPositive() {
		return nil, decimal.Zero
	}
	total := p.total()
	if amount.GreaterThan(total) {
		short = amount.Sub(total)
		out = append(out, p.parts...)
		p.parts = nil
		if short.GreaterThanOrEqual(flowDustAmount) {
			out = append(out, flowPart{Origin: flowOriginOther, Amount: short})
		}
		return out, short
	}
	if p.rule == flowRuleProRata {
		left := amount
		kept := p.parts[:0]
		for i, x := range p.parts {
			share := amount.Mul(x.Amount).DivRound(total, flowProRataPrecision)
			if i == len(p.parts)-1 || share.GreaterThan(left) {
				share = decimal.Min(left, x.Amount)
			}
			left = left.Sub(share)
			if share.IsPositive() {
				out = append(out, flowPart{Origin: x.Origin, Amount: share})
			}
			if rest := x.Amount.Sub(share); rest.GreaterThanOrEqual(flowDustAmount) {
				kept = append(kept, flowPart{Origin: x.Origin, Amount: rest})
			}
		}
		p.parts = kept
		return out, decimal.Zero
	}
	left := amount
	for len(p.parts) > 0 && left.IsPositive() {
		head := &p.parts[0]
		if head.Amount.LessThanOrEqual(left) {
			out = append(out, *head)
			left = left.Sub(head.Amount)
			p.parts = p.parts[1:]
			continue
		}
		out = append(out, flowPart{Origin: head.Origin, Amount: left})
		head.Amount = head.Amount.Sub(left)
		left = decimal.Zero
	}
	return out, decimal.Zero
}

type flowPools struct {
	liquid    *flowPool
	staked    map[string]*flowPool
	unbonding map[string]*flowPool
	locked    *flowPool
}

func newFlowPools(rule int) *flowPools {
	return &flowPools{liquid: &flowPool{rule: rule}, staked: map[string]*flowPool{}, unbonding: map[string]*flowPool{}, locked: &flowPool{rule: rule}}
}

func (p *flowPools) bucket(m map[string]*flowPool, validator string, rule int) *flowPool {
	if b, ok := m[validator]; ok {
		return b
	}
	b := &flowPool{rule: rule}
	m[validator] = b
	return b
}

func (p *flowPools) total() decimal.Decimal {
	t := p.liquid.total().Add(p.locked.total())
	for _, b := range p.staked {
		t = t.Add(b.total())
	}
	for _, b := range p.unbonding {
		t = t.Add(b.total())
	}
	return t
}

type flowWallet struct {
	Addr     string
	Hop      int
	Since    time.Time
	Known    bool
	pools    [flowRules]*flowPools
	Overdraw [flowRules]decimal.Decimal
	// bids is how much of the locked coins each auction contract holds as
	// this wallet's open bids, so a settlement pays only from its own bid.
	bids map[string]decimal.Decimal
}

// FlowFateKey is one line of a validator's result: coins of one commission
// month that one rule puts in one class.
type FlowFateKey struct {
	Rule         int
	Month        string
	Class        string
	Detail       string
	ViaUnbonding bool
}

type flowQueued struct {
	key    flowKey
	seq    int
	wallet *flowWallet
	event  flowEvent
}

type flowQueue []flowQueued

func (q flowQueue) Len() int { return len(q) }
func (q flowQueue) Less(i, j int) bool {
	if q[i].key == q[j].key {
		return q[i].seq < q[j].seq
	}
	return q[i].key.before(q[j].key)
}
func (q flowQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *flowQueue) Push(x any)   { *q = append(*q, x.(flowQueued)) }
func (q *flowQueue) Pop() any {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[:n-1]
	return x
}

// flowTrace is one validator's trace.
type flowTrace struct {
	Validator string
	From      time.Time
	AsOf      time.Time // exclusive
	load      flowLoader

	Wallets    map[string]*flowWallet
	order      []*flowWallet
	queue      flowQueue
	seq        int
	stops      map[string]flowStop
	Fates      map[FlowFateKey]decimal.Decimal
	Commission map[string]decimal.Decimal
	Unresolved []string
}

func newFlowTrace(validator string, from, asOf time.Time, load flowLoader) *flowTrace {
	return &flowTrace{
		Validator: validator, From: from, AsOf: asOf, load: load,
		Wallets: map[string]*flowWallet{}, stops: map[string]flowStop{},
		Fates: map[FlowFateKey]decimal.Decimal{}, Commission: map[string]decimal.Decimal{},
	}
}

// run traces from the receiving wallets (hop 0) over [From, AsOf).
func (t *flowTrace) run(group []string) error {
	for _, addr := range group {
		if err := t.discover(addr, 0, t.From, nil); err != nil {
			return err
		}
	}
	for t.queue.Len() > 0 {
		q := heap.Pop(&t.queue).(flowQueued)
		if err := t.apply(q.wallet, q.event, true); err != nil {
			return err
		}
	}
	t.settle()
	return nil
}

func flowDay(at time.Time) time.Time {
	u := at.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// discover starts tracing addr from the start of the day of at. Its events
// before cursor are replayed into its own pools at once: no coins of this
// validator had reached it yet, so they only set how much other coin it
// holds. A nil cursor queues every event.
func (t *flowTrace) discover(addr string, hop int, at time.Time, cursor *flowKey) error {
	start := flowDay(at)
	w := &flowWallet{Addr: addr, Hop: hop, Since: start}
	for r := 0; r < flowRules; r++ {
		w.pools[r] = newFlowPools(r)
		w.Overdraw[r] = decimal.Zero
	}
	h, known, err := t.load.opening(addr, start.AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	w.Known = known
	if known {
		for r := 0; r < flowRules; r++ {
			p := w.pools[r]
			p.liquid.add([]flowPart{{Amount: h.Liquid}})
			for v, a := range h.Staked {
				p.bucket(p.staked, v, r).add([]flowPart{{Amount: a}})
			}
			for v, a := range h.Unbonding {
				p.bucket(p.unbonding, v, r).add([]flowPart{{Amount: a}})
			}
		}
	}
	events, err := t.load.events(addr, start, t.AsOf)
	if err != nil {
		return err
	}
	t.Wallets[addr] = w
	t.order = append(t.order, w)
	for _, e := range events {
		if cursor != nil && e.Key.before(*cursor) {
			if err := t.apply(w, e, false); err != nil {
				return err
			}
			continue
		}
		t.seq++
		heap.Push(&t.queue, flowQueued{key: e.Key, seq: t.seq, wallet: w, event: e})
	}
	return nil
}

func flowMonth(at time.Time) string { return at.UTC().Format(apiMonthLayout) }

func tagged(parts [flowRules][]flowPart) bool {
	for _, ps := range parts {
		for _, x := range ps {
			if x.Origin != flowOriginOther && x.Amount.IsPositive() {
				return true
			}
		}
	}
	return false
}

// each runs fn once per rule on that rule's pools.
func (t *flowTrace) each(w *flowWallet, fn func(r int, p *flowPools)) {
	for r := 0; r < flowRules; r++ {
		fn(r, w.pools[r])
	}
}

func (t *flowTrace) takeFrom(w *flowWallet, r int, pool *flowPool, amount decimal.Decimal, counted bool) []flowPart {
	out, short := pool.take(amount)
	if counted && short.GreaterThanOrEqual(flowDustAmount) {
		w.Overdraw[r] = w.Overdraw[r].Add(short)
	}
	return out
}

func (t *flowTrace) record(r int, parts []flowPart, class, detail string) {
	for _, x := range parts {
		if x.Origin == flowOriginOther || !x.Amount.IsPositive() {
			continue
		}
		month, via := splitFlowOrigin(x.Origin)
		k := FlowFateKey{Rule: r, Month: month, Class: class, Detail: detail, ViaUnbonding: via}
		t.Fates[k] = t.Fates[k].Add(x.Amount)
	}
}

func splitFlowOrigin(origin string) (string, bool) {
	if n := len(origin) - len(flowViaUnbondingClass); n > 0 && origin[n:] == flowViaUnbondingClass {
		return origin[:n], true
	}
	return origin, false
}

func markUnbonded(parts []flowPart) []flowPart {
	for i, x := range parts {
		if x.Origin != flowOriginOther {
			if _, via := splitFlowOrigin(x.Origin); !via {
				parts[i].Origin = x.Origin + flowViaUnbondingClass
			}
		}
	}
	return parts
}

// apply moves one event's coins between w's pools. In a replay (live false)
// outflows only leave w: nothing they carry belongs to this validator, and a
// traced receiver already counted them as other coins.
func (t *flowTrace) apply(w *flowWallet, e flowEvent, live bool) error {
	amt := e.Amount
	if !amt.IsPositive() {
		return nil
	}
	in := e.Dir != apiOut
	switch e.Category {
	case apiCommission:
		origin := flowOriginOther
		if e.Validator == t.Validator {
			origin = flowMonth(e.Key.Time)
			t.Commission[origin] = t.Commission[origin].Add(amt)
		}
		t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Origin: origin, Amount: amt}}) })
	case apiTransfer:
		if e.From == e.To {
			return nil
		}
		if in {
			if live {
				if _, traced := t.Wallets[e.From]; traced {
					return nil
				}
			}
			t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Amount: amt}}) })
			return nil
		}
		var parts [flowRules][]flowPart
		t.each(w, func(r int, p *flowPools) { parts[r] = t.takeFrom(w, r, p.liquid, amt, true) })
		if live {
			return t.route(w, parts, e)
		}
	case flowCatDelegate:
		t.each(w, func(r int, p *flowPools) {
			p.bucket(p.staked, e.Validator, r).add(t.takeFrom(w, r, p.liquid, amt, true))
		})
	case flowCatUndelegate:
		t.each(w, func(r int, p *flowPools) {
			p.bucket(p.unbonding, e.Validator, r).add(t.takeFrom(w, r, p.bucket(p.staked, e.Validator, r), amt, true))
		})
	case flowCatRedelegate:
		t.each(w, func(r int, p *flowPools) {
			p.bucket(p.staked, e.Validator, r).add(t.takeFrom(w, r, p.bucket(p.staked, e.SrcValidator, r), amt, true))
		})
	case flowCatCancelUnbonding:
		t.each(w, func(r int, p *flowPools) {
			p.bucket(p.staked, e.Validator, r).add(t.takeFrom(w, r, p.bucket(p.unbonding, e.Validator, r), amt, true))
		})
	case flowCatUnbondingComplete:
		t.each(w, func(r int, p *flowPools) {
			p.liquid.add(markUnbonded(t.takeFrom(w, r, p.bucket(p.unbonding, e.Validator, r), amt, true)))
		})
	case flowCatTokenize:
		t.each(w, func(r int, p *flowPools) {
			t.record(r, t.takeFrom(w, r, p.bucket(p.staked, e.Validator, r), amt, true), flowTokenized, e.Validator)
		})
	case flowCatRedeem:
		t.each(w, func(r int, p *flowPools) { p.bucket(p.staked, e.Validator, r).add([]flowPart{{Amount: amt}}) })
	case flowCatProtocolDeposit, flowCatGovDeposit:
		if in {
			t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Amount: amt}}) })
			return nil
		}
		if e.Trigger == flowAuctionBid {
			w.addBid(e.To, amt)
		}
		t.each(w, func(r int, p *flowPools) { p.locked.add(t.takeFrom(w, r, p.liquid, amt, true)) })
	case flowCatProtocolWithdrawal, flowCatGovRefund:
		if e.Trigger == flowAuctionRefund {
			w.takeBid(e.From, amt)
		}
		t.each(w, func(r int, p *flowPools) { p.liquid.add(t.takeFrom(w, r, p.locked, amt, false)) })
	case flowCatGovBurn:
		t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.locked, amt, false), flowBurned, "") })
	case flowCatIBCOut:
		if in {
			t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Amount: amt}}) })
			return nil
		}
		dest := t.load.ibcDestination(e)
		t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.liquid, amt, true), flowIBCOut, dest) })
	case apiFee:
		t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.liquid, amt, true), flowFees, "") })
	case apiNFTSale:
		if in {
			t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Amount: amt}}) })
			return nil
		}
		if !flowEscrowedBid(e.Detail) {
			t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.liquid, amt, true), flowSpent, e.Category) })
			return nil
		}
		// A reserve auction's winner pays with its bid, escrowed with that
		// contract as a protocol_deposit, so the price leaves the locked coins
		// up to that bid. A buy-now escrows nothing and pays from the wallet.
		fromBid := w.takeBid(detailValue(e.Detail, "contract"), amt)
		t.each(w, func(r int, p *flowPools) {
			fromLocked := decimal.Min(fromBid, p.locked.total())
			parts := t.takeFrom(w, r, p.locked, fromLocked, false)
			if rest := amt.Sub(fromLocked); rest.IsPositive() {
				parts = append(parts, t.takeFrom(w, r, p.liquid, rest, true)...)
			}
			t.record(r, parts, flowSpent, e.Category)
		})
	default:
		// Rewards, IBC receipts, income and the bought side of a swap add
		// coins of other origin; any other outflow is a spend.
		if in {
			t.each(w, func(r int, p *flowPools) { p.liquid.add([]flowPart{{Amount: amt}}) })
			return nil
		}
		class := flowSpent
		if e.Category == apiSwap {
			class = flowSwapped
		}
		t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.liquid, amt, true), class, e.Category) })
	}
	return nil
}

// Reward triggers the parser gives auction bids and outbid refunds.
const (
	flowAuctionBid    = "auction_bid"
	flowAuctionRefund = "auction_refund"
)

func (w *flowWallet) addBid(contract string, amt decimal.Decimal) {
	if w.bids == nil {
		w.bids = map[string]decimal.Decimal{}
	}
	w.bids[contract] = w.bids[contract].Add(amt)
}

// takeBid removes up to amt of w's open bids with contract and returns how
// much there was.
func (w *flowWallet) takeBid(contract string, amt decimal.Decimal) decimal.Decimal {
	got := decimal.Min(amt, w.bids[contract])
	if got.IsPositive() {
		w.bids[contract] = w.bids[contract].Sub(got)
	}
	return decimal.Max(got, decimal.Zero)
}

// flowEscrowedBid reports whether an nft_sale settled a Stargaze reserve
// auction, whose bids the parser records as protocol_deposit rows
// (auction_bid); a settled bid is paid from that escrow.
func flowEscrowedBid(detail string) bool {
	return detailValue(detail, "sale") == "auction" && detailValue(detail, "protocol") == "Stargaze"
}

// route sends the coins w paid out in a transfer to their receiver: into a
// traced wallet, to a fate when the receiver ends the trace, or into a newly
// discovered wallet one hop further.
func (t *flowTrace) route(w *flowWallet, parts [flowRules][]flowPart, e flowEvent) error {
	if dst, ok := t.Wallets[e.To]; ok {
		for r := 0; r < flowRules; r++ {
			dst.pools[r].liquid.add(parts[r])
		}
		return nil
	}
	if !tagged(parts) {
		return nil
	}
	stop, err := t.stopFor(e.To)
	if err != nil {
		return err
	}
	if stop.Class == "" && w.Hop >= flowMaxHops {
		stop = flowStop{Class: flowBeyondHops}
	}
	if stop.Class != "" {
		for r := 0; r < flowRules; r++ {
			t.record(r, parts[r], stop.Class, stop.Detail)
		}
		return nil
	}
	cursor := e.Key
	if err := t.discover(e.To, w.Hop+1, e.Key.Time, &cursor); err != nil {
		t.Unresolved = append(t.Unresolved, e.To)
		delete(t.Wallets, e.To)
		for r := 0; r < flowRules; r++ {
			t.record(r, parts[r], flowUnresolved, "")
		}
		t.stops[e.To] = flowStop{Class: flowUnresolved}
		return nil
	}
	dst := t.Wallets[e.To]
	for r := 0; r < flowRules; r++ {
		dst.pools[r].liquid.add(parts[r])
	}
	return nil
}

func (t *flowTrace) stopFor(addr string) (flowStop, error) {
	if addr == "" {
		return flowStop{Class: flowUnresolved, Detail: "no receiver"}, nil
	}
	if s, ok := t.stops[addr]; ok {
		return s, nil
	}
	s, err := t.load.stop(addr)
	if err != nil {
		return flowStop{}, err
	}
	t.stops[addr] = s
	return s, nil
}

// settle records where the coins still in traced wallets sit at the as-of.
func (t *flowTrace) settle() {
	for _, w := range t.order {
		for r := 0; r < flowRules; r++ {
			p := w.pools[r]
			held, staked, unbonding, locked := flowHeld, flowRestakedOwn, flowUnbonding, flowLocked
			detail := ""
			if w.Hop > 0 {
				held, staked, unbonding, locked = flowHeldDownstream, flowStakedDownstream, flowUnbondDownstream, flowLockedDownstream
				detail = "hop " + strconv.Itoa(w.Hop)
			}
			t.record(r, p.liquid.parts, held, detail)
			t.record(r, p.locked.parts, locked, detail)
			for _, v := range sortedPoolKeys(p.staked) {
				class, d := staked, detail
				if w.Hop == 0 && v != t.Validator {
					class, d = flowRestakedOther, v
				}
				t.record(r, p.staked[v].parts, class, d)
			}
			for _, v := range sortedPoolKeys(p.unbonding) {
				t.record(r, p.unbonding[v].parts, unbonding, detail)
			}
		}
	}
}

func sortedPoolKeys(m map[string]*flowPool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// simulated is what the chain's balance of w should show: everything but the
// coins locked in a protocol or governance deposit, which the bank no longer
// holds for w.
func (w *flowWallet) simulated() decimal.Decimal {
	p := w.pools[flowRuleFIFO]
	return p.total().Sub(p.locked.total())
}
