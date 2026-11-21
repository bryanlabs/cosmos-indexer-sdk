package taxapi

import (
	"container/heap"
	"fmt"
	"math"
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
// follows by default. Coins leaving a wallet at the limit end as beyond_hops.
// A private run may set another limit (flowMaxHopsCeiling at most).
const (
	flowMaxHops        = 3
	flowMaxHopsCeiling = 20
)

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
	flowWideDistributor   = "wide_distributor"
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
	// flowCatSlash is a validator slash. It burns stake, so it is neither
	// income nor a sale: the coins it takes end as burned.
	flowCatSlash = "slash"
)

// flowSlashDetail is the burned detail of coins a validator slash took.
const flowSlashDetail = "slash"

// flowSlashSub keys a slash ahead of every other movement in its block, since
// the chain slashes in begin block.
const flowSlashSub = math.MinInt

// flowUnbondingPeriod is the Hub's unbonding time. A double-sign slash names
// no infraction height, so it reaches every unbonding and redelegation entry
// begun within this long before it, which is the most the chain can take.
const flowUnbondingPeriod = 21 * 24 * time.Hour

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
	// Lot names the NFT a reserve auction bid, refund or settlement is for,
	// or the marketplace bid a deposit, refund or fill is for.
	Lot string
	// PaidFromBid says an nft_sale was paid with the buyer's marketplace bid
	// escrow (payFilledBids).
	PaidFromBid bool
	// Fraction is the share of the validator's bonded tokens a slash burned,
	// and DoubleSign says it punished a double sign rather than downtime.
	Fraction   decimal.Decimal
	DoubleSign bool
}

// flowHoldings is a wallet's native coin at the end of a day, in base units.
type flowHoldings struct {
	Source    CommissionFlowHoldingSource
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
	// stop is the fate of coins sent to addr, which the trace does not follow,
	// or an empty class for an ordinary wallet. [start, end) is the trace
	// window, which the wide distributor rule counts receivers over.
	stop(addr string, start, end time.Time) (flowStop, error)
	// distributor is the wide distributor stop for addr over [start, end), or
	// an empty class. It ends the transfers a receiving wallet (hop 0) pays
	// out, since that wallet is traced, not stopped.
	distributor(addr string, start, end time.Time) (flowStop, error)
	// known is the stop for addr from labels and address shape alone, with
	// no index read.
	known(addr string) flowStop
	ibcDestination(e flowEvent) string
	// slashes is every validator slash in [start, end), with Validator the
	// operator and Amount the tokens it burned from the validator.
	slashes(start, end time.Time) ([]flowEvent, error)
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
			if rest := x.Amount.Sub(share); rest.IsPositive() {
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

// burn removes fraction of every part, as a slash takes the same share of
// every coin bonded to the validator whatever the attribution rule.
func (p *flowPool) burn(fraction decimal.Decimal) []flowPart {
	if !fraction.IsPositive() {
		return nil
	}
	if fraction.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		out := p.parts
		p.parts = nil
		return out
	}
	var out []flowPart
	kept := p.parts[:0]
	for _, x := range p.parts {
		cut := x.Amount.Mul(fraction).Round(flowProRataPrecision)
		if cut.IsPositive() {
			out = append(out, flowPart{Origin: x.Origin, Amount: cut})
		}
		if rest := x.Amount.Sub(cut); rest.IsPositive() {
			kept = append(kept, flowPart{Origin: x.Origin, Amount: rest})
		}
	}
	p.parts = kept
	return out
}

// burnAmount removes amount, or all the pool holds, spread over every part.
func (p *flowPool) burnAmount(amount decimal.Decimal) []flowPart {
	total := p.total()
	if !total.IsPositive() || !amount.IsPositive() {
		return nil
	}
	return p.burn(decimal.Min(amount, total).DivRound(total, flowProRataPrecision*2))
}

type flowPools struct {
	liquid    *flowPool
	staked    map[string]*flowPool
	unbonding map[string]*flowPool
	locked    *flowPool
	// bids holds each open reserve auction bid by lot, with the origins of
	// the coins it escrowed, so a refund or settlement moves exactly those.
	bids map[string]*flowPool
}

func newFlowPools(rule int) *flowPools {
	return &flowPools{liquid: &flowPool{rule: rule}, staked: map[string]*flowPool{}, unbonding: map[string]*flowPool{}, locked: &flowPool{rule: rule}, bids: map[string]*flowPool{}}
}

// escrowed is everything the bank no longer holds for the wallet: deposits
// and open auction bids.
func (p *flowPools) escrowed() decimal.Decimal {
	t := p.locked.total()
	for _, b := range p.bids {
		t = t.Add(b.total())
	}
	return t
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
	t := p.liquid.total().Add(p.escrowed())
	for _, b := range p.staked {
		t = t.Add(b.total())
	}
	for _, b := range p.unbonding {
		t = t.Add(b.total())
	}
	return t
}

type flowWallet struct {
	Addr          string
	Hop           int
	Since         time.Time
	Known         bool
	OpeningSource CommissionFlowHoldingSource
	pools         [flowRules]*flowPools
	Overdraw      [flowRules]decimal.Decimal
	// entries are the unbondings and redelegations w began, which a later
	// slash of their source validator can reach.
	entries []flowEntry
}

// flowEntry is an unbonding (Dst empty) or a redelegation from Src.
type flowEntry struct {
	Key      flowKey
	Src, Dst string
	Amount   decimal.Decimal
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
	strict    bool
	attempted map[string]bool

	progress                    func(commissionFlowProgress)
	started                     time.Time
	progressSequence            int
	appliedQueuedEvents         int
	closingReadCompletedWallets int
	discoveryComplete           bool

	Wallets    map[string]*flowWallet
	order      []*flowWallet
	queue      flowQueue
	seq        int
	stops      map[string]flowStop
	sources    map[string]flowStop
	Fates      map[FlowFateKey]decimal.Decimal
	Commission map[string]decimal.Decimal
	Unresolved []string
	// maxHops is the hop limit; zero means flowMaxHops.
	maxHops int
	// Stops sums, per rule, the commission each transfer out of the traced
	// wallets carried to a stop (a labelled wallet, a busy wallet, the hop
	// limit), keyed by both ends, so every amount that ends untraced names
	// the wallet it stopped at.
	Stops map[FlowStopKey]decimal.Decimal
	// slashList is every validator slash in [From, AsOf).
	slashList []flowEvent
}

func newFlowTrace(validator string, from, asOf time.Time, load flowLoader) *flowTrace {
	return &flowTrace{
		Validator: validator, From: from, AsOf: asOf, load: load, started: time.Now(),
		Wallets: map[string]*flowWallet{}, stops: map[string]flowStop{}, sources: map[string]flowStop{}, attempted: map[string]bool{},
		Fates: map[FlowFateKey]decimal.Decimal{}, Commission: map[string]decimal.Decimal{}, Stops: map[FlowStopKey]decimal.Decimal{},
	}
}

func (t *flowTrace) hopLimit() int {
	if t.maxHops > 0 {
		return t.maxHops
	}
	return flowMaxHops
}

// FlowStopKey is where a transfer left the traced wallets: the sending
// wallet and its hop, the receiver, and the fate the receiver gave it.
type FlowStopKey struct {
	Rule   int
	Class  string
	Detail string
	From   string
	To     string
	Hop    int
}

// recordStop adds the commission parts carry to the stop from w to "to".
func (t *flowTrace) recordStop(r int, parts []flowPart, w *flowWallet, to string, stop flowStop) {
	amount := decimal.Zero
	for _, x := range parts {
		if x.Origin != flowOriginOther && x.Amount.IsPositive() {
			amount = amount.Add(x.Amount)
		}
	}
	if !amount.IsPositive() {
		return
	}
	k := FlowStopKey{Rule: r, Class: stop.Class, Detail: stop.Detail, From: w.Addr, To: to, Hop: w.Hop}
	t.Stops[k] = t.Stops[k].Add(amount)
}

// run traces from the receiving wallets (hop 0) over [From, AsOf).
func (t *flowTrace) run(group []string) error {
	slashes, err := t.load.slashes(t.From, t.AsOf)
	if err != nil {
		return err
	}
	t.slashList = slashes
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
		t.appliedQueuedEvents++
		if t.appliedQueuedEvents%1000 == 0 {
			t.reportProgress("events_applied", "", -1)
		}
	}
	t.settle()
	t.discoveryComplete = true
	t.reportProgress("discovery_complete", "", len(t.order))
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
	t.attempted[addr] = true
	start := flowDay(at)
	w := &flowWallet{Addr: addr, Hop: hop, Since: start}
	for r := 0; r < flowRules; r++ {
		w.pools[r] = newFlowPools(r)
		w.Overdraw[r] = decimal.Zero
	}
	t.reportProgress("opening_started", addr, -1)
	h, known, err := t.load.opening(addr, start.AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	if t.strict && !known {
		return fmt.Errorf("commission opening requires exact archive state")
	}
	t.reportProgress("opening_complete", addr, -1)
	w.Known = known
	w.OpeningSource = h.Source
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
	t.reportProgress("events_started", addr, -1)
	events, err := t.load.events(addr, start, t.AsOf)
	if err != nil {
		return err
	}
	events = t.withSlashes(events, h, start)
	t.Wallets[addr] = w
	t.order = append(t.order, w)
	t.reportProgress("events_loaded", addr, -1)
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
	t.reportProgress("wallet_loaded", addr, -1)
	return nil
}

// withSlashes adds to a wallet's events the slashes from start of every
// validator it could have stake with: those in its opening holdings and those
// its events name. The opening already reflects earlier slashes.
func (t *flowTrace) withSlashes(events []flowEvent, h flowHoldings, start time.Time) []flowEvent {
	if len(t.slashList) == 0 {
		return events
	}
	vals := map[string]bool{}
	for v := range h.Staked {
		vals[v] = true
	}
	for v := range h.Unbonding {
		vals[v] = true
	}
	for _, e := range events {
		if e.Validator != "" {
			vals[e.Validator] = true
		}
		if e.SrcValidator != "" {
			vals[e.SrcValidator] = true
		}
	}
	added := false
	for _, sl := range t.slashList {
		if vals[sl.Validator] && !sl.Key.Time.Before(start) {
			events = append(events, sl)
			added = true
		}
	}
	if added {
		sort.SliceStable(events, func(i, j int) bool { return events[i].Key.before(events[j].Key) })
	}
	return events
}

// slash burns e.Fraction of w's delegation to the slashed validator, and of
// the entries it reaches, as the staking module does: unbondings from that
// validator and redelegations away from it begun at or after the infraction.
// A downtime infraction is two blocks before the slash; a double sign reaches
// back one unbonding period (flowUnbondingPeriod). An entry loses the
// fraction of its starting amount, capped by what its pool still holds.
func (t *flowTrace) slash(w *flowWallet, e flowEvent) {
	t.each(w, func(r int, p *flowPools) {
		if b, ok := p.staked[e.Validator]; ok {
			t.record(r, b.burn(e.Fraction), flowBurned, flowSlashDetail)
		}
	})
	for _, en := range w.entries {
		if en.Src != e.Validator {
			continue
		}
		if e.DoubleSign {
			if en.Key.Time.Before(e.Key.Time.Add(-flowUnbondingPeriod)) {
				continue
			}
		} else if en.Key.Height < e.Key.Height-2 {
			continue
		}
		loss := en.Amount.Mul(e.Fraction)
		t.each(w, func(r int, p *flowPools) {
			pool := p.unbonding[en.Src]
			if en.Dst != "" {
				pool = p.staked[en.Dst]
			}
			if pool != nil {
				t.record(r, pool.burnAmount(loss), flowBurned, flowSlashDetail)
			}
		})
	}
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
	if e.Trigger == flowStreamExit && e.Lot != "" {
		// A finished stream spent everything its subscriber left in it on
		// the tokens it paid out, which the row names in another denom.
		t.each(w, func(r int, p *flowPools) {
			b := p.bids[e.Lot]
			if b == nil {
				return
			}
			parts := t.takeFrom(w, r, b, b.total(), false)
			delete(p.bids, e.Lot)
			t.record(r, parts, flowSwapped, apiSwap)
		})
		return nil
	}
	amt := e.Amount
	if !amt.IsPositive() {
		return nil
	}
	if (e.Trigger == tax.TriggerLimitFill || e.Trigger == tax.TriggerLimitFee) && e.Lot != "" && e.Dir == apiOut {
		// The report pays a maker fill, and what the book kept of a closed
		// buy order, from the order's lot.
		class := flowSpent
		if e.Category == apiSwap {
			class = flowSwapped
		}
		t.each(w, func(r int, p *flowPools) { t.record(r, t.takeBid(w, r, p, e.Lot, amt), class, e.Category) })
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
	case flowCatSlash:
		t.slash(w, e)
	case flowCatUndelegate:
		w.entries = append(w.entries, flowEntry{Key: e.Key, Src: e.Validator, Amount: amt})
		t.each(w, func(r int, p *flowPools) {
			p.bucket(p.unbonding, e.Validator, r).add(t.takeFrom(w, r, p.bucket(p.staked, e.Validator, r), amt, true))
		})
	case flowCatRedelegate:
		w.entries = append(w.entries, flowEntry{Key: e.Key, Src: e.SrcValidator, Dst: e.Validator, Amount: amt})
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
		if (e.Trigger == flowAuctionBid || e.Trigger == flowStreamSubscribe || e.Trigger == tax.TriggerLimitOrder) && e.Lot != "" {
			t.each(w, func(r int, p *flowPools) { p.bucket(p.bids, e.Lot, r).add(t.takeFrom(w, r, p.liquid, amt, true)) })
			return nil
		}
		t.each(w, func(r int, p *flowPools) { p.locked.add(t.takeFrom(w, r, p.liquid, amt, true)) })
	case flowCatProtocolWithdrawal, flowCatGovRefund:
		if (e.Trigger == flowAuctionRefund || e.Trigger == flowBidFilled || e.Trigger == flowStreamWithdraw || e.Trigger == flowStreamRefund || e.Trigger == tax.TriggerLimitCancel) && e.Lot != "" {
			// A refund larger than the bid the trace saw returns the rest as
			// other coins, as any other withdrawal does.
			t.each(w, func(r int, p *flowPools) { p.liquid.add(t.takeBid(w, r, p, e.Lot, amt)) })
			return nil
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
		if !flowEscrowedBid(e.Detail) && !e.PaidFromBid {
			t.each(w, func(r int, p *flowPools) { t.record(r, t.takeFrom(w, r, p.liquid, amt, true), flowSpent, e.Category) })
			return nil
		}
		// A reserve auction's winner and the buyer on an accepted
		// marketplace bid pay with the bid they escrowed, so the price
		// leaves that bid. A buy-now escrows nothing and pays from the
		// wallet.
		t.each(w, func(r int, p *flowPools) {
			b := p.bids[e.Lot]
			fromBid := decimal.Zero
			if b != nil {
				fromBid = decimal.Min(amt, b.total())
			}
			var parts []flowPart
			if fromBid.IsPositive() {
				parts = t.takeFrom(w, r, b, fromBid, false)
				if b.total().IsZero() {
					delete(p.bids, e.Lot)
				}
			}
			if rest := amt.Sub(fromBid); rest.IsPositive() {
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

// Reward triggers the parser gives auction and marketplace bids, their
// refunds and an accepted marketplace bid.
const (
	flowAuctionBid    = "auction_bid"
	flowAuctionRefund = "auction_refund"
	flowBidFilled     = "bid_filled"

	flowStreamSubscribe = tax.TriggerStreamSubscribe
	flowStreamWithdraw  = tax.TriggerStreamWithdraw
	flowStreamExit      = tax.TriggerStreamExit
	flowStreamRefund    = tax.TriggerStreamRefund
)

// flowStreamLot names the streamswap subscription a stream row is about:
// the stream contract and the stream id. Each wallet's pools hold their
// own lots, so the subscriber is not part of the name.
// flowLimitLot keys a limit order's resting coins by contract and order.
func flowLimitLot(e tax.TaxableEvent) string {
	contract, id := detailValue(e.Detail, "contract"), detailValue(e.Detail, "order_id")
	if contract == "" || id == "" {
		return ""
	}
	return contract + "/order/" + id
}

func flowStreamLot(e tax.TaxableEvent) string {
	contract, id := detailValue(e.Detail, "contract"), detailValue(e.Detail, "stream_id")
	if contract == "" || id == "" {
		return ""
	}
	return contract + "/stream/" + id
}

// takeBid removes amt from the bid on lot; past the bid it is coins of other
// origin, as takeFrom counts any shortfall.
func (t *flowTrace) takeBid(w *flowWallet, r int, p *flowPools, lot string, amt decimal.Decimal) []flowPart {
	parts := t.takeFrom(w, r, p.bucket(p.bids, lot, r), amt, false)
	if p.bids[lot].total().IsZero() {
		delete(p.bids, lot)
	}
	return parts
}

// flowBidLot names the bid an escrow row is about: a marketplace bid by its
// id, a reserve auction bid by the contract plus the collection and token,
// from the bid's detail or the sale's asset.
func flowBidLot(e tax.TaxableEvent) string {
	contract := detailValue(e.Detail, "contract")
	if contract == "" {
		return ""
	}
	if id := detailValue(e.Detail, "bid_id"); id != "" {
		return contract + "/bid/" + id
	}
	if e.Category == apiNFTSale {
		if e.Asset == "" {
			return ""
		}
		return contract + "/" + e.Asset
	}
	coll, tok := detailValue(e.Detail, "collection"), detailValue(e.Detail, "token_id")
	if coll == "" || tok == "" {
		return ""
	}
	return contract + "/" + coll + "/" + tok
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
	var stop flowStop
	if w.Hop == 0 && e.To != "" {
		// A receiving wallet that pays out to more than
		// flowFanoutMaxReceivers wallets distributes the commission it
		// holds; its payouts end there instead of opening one hop-1 wallet
		// per recipient. A labelled receiver keeps its own fate, read
		// without a query per recipient.
		src, err := t.sourceStop(w.Addr)
		if err != nil {
			return err
		}
		if src.Class != "" {
			if stop = t.load.known(e.To); stop.Class == "" {
				stop = src
			}
		}
	}
	if stop.Class == "" {
		var err error
		if stop, err = t.stopFor(e.To); err != nil {
			return err
		}
	}
	if stop.Class == "" && w.Hop >= t.hopLimit() {
		stop = flowStop{Class: flowBeyondHops}
	}
	if stop.Class != "" {
		if t.strict && stop.Class == flowUnresolved {
			return fmt.Errorf("commission destination is unresolved")
		}
		for r := 0; r < flowRules; r++ {
			t.recordStop(r, parts[r], w, e.To, stop)
			t.record(r, parts[r], stop.Class, stop.Detail)
		}
		return nil
	}
	cursor := e.Key
	if err := t.discover(e.To, w.Hop+1, e.Key.Time, &cursor); err != nil {
		if t.strict {
			return err
		}
		t.Unresolved = append(t.Unresolved, e.To)
		delete(t.Wallets, e.To)
		for r := 0; r < flowRules; r++ {
			t.recordStop(r, parts[r], w, e.To, flowStop{Class: flowUnresolved})
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
	s, err := t.load.stop(addr, t.From, t.AsOf)
	if err != nil {
		return flowStop{}, err
	}
	t.stops[addr] = s
	return s, nil
}

// sourceStop is the wide distributor stop for a receiving wallet's payouts,
// cached in t.sources.
func (t *flowTrace) sourceStop(addr string) (flowStop, error) {
	if s, ok := t.sources[addr]; ok {
		return s, nil
	}
	s, err := t.load.distributor(addr, t.From, t.AsOf)
	if err != nil {
		return flowStop{}, err
	}
	t.sources[addr] = s
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
			for _, lot := range sortedPoolKeys(p.bids) {
				t.record(r, p.bids[lot].parts, locked, detail)
			}
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
	return p.total().Sub(p.escrowed())
}
