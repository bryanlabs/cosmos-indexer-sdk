package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AddressLabel names an address the commission tracer stops at. Source says
// where the name came from (a derivation, a heuristic with its version, or
// a licensed dataset) and Evidence what it rests on, so every figure that
// uses a label can say why.
type AddressLabel struct {
	Chain     string `gorm:"primaryKey"`
	Address   string `gorm:"primaryKey"`
	Name      string
	Kind      string
	Source    string
	Evidence  string
	CreatedAt time.Time
}

func (AddressLabel) TableName() string { return "tax_address_labels" }

// Label kinds. Every kind but validator_operator ends a trace.
const (
	labelExchange          = "exchange"
	labelExchangeLike      = "exchange_like"
	labelModule            = "module"
	labelIBCEscrow         = "ibc_escrow"
	labelValidatorOperator = "validator_operator"
	// Kinds from the derived and published label set (OPS-849). Each stops
	// a trace in its own class; none counts as an exchange.
	labelInterchainAccount = "interchain_account"
	labelGrantRecipient    = "grant_recipient"
	labelBridgeEscrow      = "bridge_escrow"
	labelBridgeContract    = "bridge_contract"
	labelDexPool           = "dex_pool"
	labelContract          = "contract"
)

// labelKindsKnown are the kinds labelStop has a class for. A run refuses a
// label of any other kind rather than guess its fate.
var labelKindsKnown = map[string]bool{
	labelExchange: true, labelExchangeLike: true, labelModule: true, labelIBCEscrow: true, labelValidatorOperator: true,
	labelInterchainAccount: true, labelGrantRecipient: true, labelBridgeEscrow: true, labelBridgeContract: true, labelDexPool: true, labelContract: true,
	labelBusyWallet: true,
}

// IBCChannel maps a Hub channel to the chain on its other end, read from the
// channel's client state on the node.
type IBCChannel struct {
	Chain     string `gorm:"primaryKey"`
	Port      string `gorm:"primaryKey"`
	Channel   string `gorm:"primaryKey"`
	ClientID  string
	ChainID   string
	FetchedAt time.Time
}

func (IBCChannel) TableName() string { return "tax_ibc_channels" }

// CommissionFlow is one line of a validator's traced commission.
type CommissionFlow struct {
	Chain        string `gorm:"primaryKey"`
	Validator    string `gorm:"primaryKey"`
	Rule         string `gorm:"primaryKey"`
	Month        string `gorm:"primaryKey"`
	Class        string `gorm:"primaryKey"`
	Detail       string `gorm:"primaryKey"`
	ViaUnbonding bool   `gorm:"primaryKey"`
	AmountBase   string `gorm:"type:numeric"`
}

func (CommissionFlow) TableName() string { return "tax_commission_flows" }

// CommissionFlowRun is the last trace of one validator and how well it
// reconciled with the chain.
type CommissionFlowRun struct {
	Chain     string    `gorm:"primaryKey"`
	Validator string    `gorm:"primaryKey"`
	FromDay   time.Time `gorm:"type:date"`
	AsOf      time.Time `gorm:"type:date"`
	Status    string
	Reason    string
	// Receivers are the wallets the commission was paid to.
	Receivers int
	// Wallets counts every wallet traced, receivers included.
	Wallets int
	// OpeningUnknown counts traced wallets whose holdings on the day before
	// tracing started could not be rebuilt; they started empty.
	OpeningUnknown int
	Unresolved     int
	// Overdraw is, per rule, the coin traced wallets spent beyond what the
	// trace held for them, in base units. DriftBase is the sum over traced
	// wallets of how far the simulated as-of holdings are from the chain's.
	Overdraw   string `gorm:"type:jsonb"`
	DriftBase  string `gorm:"type:numeric"`
	Commission string `gorm:"type:jsonb"`
	Untracked  string `gorm:"type:jsonb"`
	ComputedAt time.Time
}

func (CommissionFlowRun) TableName() string { return "tax_commission_flow_runs" }

// CommissionFlowsMigrate creates the commission flow tables.
func CommissionFlowsMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&AddressLabel{}, &IBCChannel{}, &CommissionFlow{}, &CommissionFlowRun{})
}

// hubModuleNames are the module accounts a Hub wallet can pay. A module
// address is the first 20 bytes of the sha256 of its name.
var hubModuleNames = []string{
	"distribution", "gov", "bonded_tokens_pool", "not_bonded_tokens_pool", "fee_collector",
	"mint", "transfer", "provider", "consumer_rewards_pool", "feemarket-fee-collector", apiLiquid,
}

func moduleAddress(name string) string {
	sum := sha256.Sum256([]byte(name))
	addr, _ := bech32.ConvertAndEncode("cosmos", sum[:20])
	return addr
}

// ics20EscrowAddress is the account a transfer channel escrows coins in.
func ics20EscrowAddress(port, channel string) string {
	pre := append([]byte("ics20-1"), 0)
	sum := sha256.Sum256(append(pre, []byte(port+"/"+channel)...))
	addr, _ := bech32.ConvertAndEncode("cosmos", sum[:20])
	return addr
}

func operatorAccount(valoper string) string {
	hrp, bz, err := bech32.DecodeAndConvert(valoper)
	if err != nil || hrp != flowValoperHRP {
		return ""
	}
	addr, _ := bech32.ConvertAndEncode("cosmos", bz)
	return addr
}

// flowEscrowChannels is how many transfer channels get an escrow label.
const flowEscrowChannels = 2000

// seedAddressLabels stores the labels the index can derive itself. It never
// overwrites a label from another source.
func derivedFlowLabels(chain string, validators []string) []AddressLabel {
	now := nowUTC()
	var rows []AddressLabel
	for _, m := range hubModuleNames {
		rows = append(rows, AddressLabel{Chain: chain, Address: moduleAddress(m), Name: m, Kind: labelModule, Source: "derived:module-account", Evidence: "sha256(module name)", CreatedAt: now})
	}
	for i := 0; i < flowEscrowChannels; i++ {
		ch := fmt.Sprintf("channel-%d", i)
		rows = append(rows, AddressLabel{Chain: chain, Address: ics20EscrowAddress("transfer", ch), Name: "transfer/" + ch, Kind: labelIBCEscrow, Source: "derived:ics20-escrow", Evidence: "ICS-20 escrow address of transfer/" + ch, CreatedAt: now})
	}
	for _, v := range validators {
		if a := operatorAccount(v); a != "" {
			rows = append(rows, AddressLabel{Chain: chain, Address: a, Name: v, Kind: labelValidatorOperator, Source: "derived:operator-account", Evidence: "account bytes of " + v, CreatedAt: now})
		}
	}
	return rows
}

func (s *Server) seedAddressLabels(chain string, validators []string) error {
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(derivedFlowLabels(chain, validators), 500).Error
}

// Memo-deposit heuristic: an exchange's deposit address receives from many
// different senders, each tagging the transfer with a short account code.
const (
	flowMemoHeuristic   = "heuristic:memo-deposit/v1"
	flowMemoSample      = 3000
	flowMemoMinSenders  = 100
	flowMemoMinLen      = 4
	flowMemoMaxLen      = 40
	flowMemoDigitsCheck = "0123456789"
)

// Wide distributor rule: a wallet that sent the native coin to more than
// flowFanoutMaxReceivers distinct wallets over the trace window pays out to a
// crowd (a reward, payroll or airdrop distributor, or an operator account
// that shares commission with its delegators). Commission that reaches it is
// reported as distributed there, not followed pro-rata into every recipient.
// No wallet in the stored traces sends to more than about 26,000 wallets,
// and traces that pass through those finish; the operator account that
// stopped four traces sends to about 380,000. 50,000 stops that account and
// changes no stored result.
const (
	flowFanoutHeuristic    = "heuristic:wide-distributor-50k/v1"
	flowFanoutMaxReceivers = 50000
	labelWideDistributor   = "wide_distributor"
)

// Deposit-sweep rule: an exchange's hot wallet collects from deposit
// addresses, one per customer, each of which forwards what it receives to the
// hot wallet and does nothing else. A busy wallet whose sampled senders are
// mostly such pass-through wallets is labelled exchange-like. Measured
// 2026-11-21 on the busiest unlabelled receivers: 160 to 184 of 200 senders
// pass through for the clearest hot wallets, 7 of 200 for a wallet that is
// not one. Busy wallets that fail it and the wide distributor rule stay
// busy_wallet, with what was measured kept in the trace's evidence.
const (
	flowSweepHeuristic = "heuristic:deposit-sweep/v2"
	flowSweepSample    = 3000
	flowSweepSenders   = 200
	flowSweepPerSender = 200
	// A pass-through wallet sent on at least this share of what other
	// wallets sent it in the window, and never more.
	flowSweepForwardMin = "0.9"
	// At least this many senders, and at least half of those examined,
	// must pass through.
	flowSweepMinDeposits = 20
	labelBusyWallet      = "busy_wallet"
	flowBusyMeasured     = "measured:busy-wallet/v1"
)

var flowMemoShape = regexp.MustCompile(`^[0-9A-Za-z_-]+$`)

func depositMemo(memo string) bool {
	memo = strings.TrimSpace(memo)
	return len(memo) >= flowMemoMinLen && len(memo) <= flowMemoMaxLen && flowMemoShape.MatchString(memo) && strings.ContainsAny(memo, flowMemoDigitsCheck)
}

// fanoutReceivers counts the distinct wallets addr sent the native coin to in
// [start, end), stopping at limit+1, so a wide wallet costs one bounded read.
// category is wrapped so the planner walks idx_tax_from for this one sender
// instead of intersecting it with the large category index.
func (s *Server) fanoutReceivers(addr string, start, end time.Time, limit int) (int, error) {
	var n int
	err := s.db.Raw(`
SELECT count(*) FROM (
	SELECT DISTINCT to_addr FROM taxable_events
	WHERE from_addr = ? AND category || '' = ? AND denom = ? AND to_addr <> '' AND to_addr <> ?
		AND timestamp >= ? AND timestamp < ? AND coalesce(reward_trigger, '') <> ?
	LIMIT ?) receivers`, addr, apiTransfer, s.native.Denom, addr, start, end, tax.TriggerWithdrawAddress, limit+1).Scan(&n).Error
	return n, err
}

// sweepDepositSenders samples the distinct wallets that sent addr the native
// coin in [start, end) and counts those that pass through to it: in bounded
// reads of their window, every movement they made is a transfer to addr, and
// it carried between flowSweepForwardMin and all of what other wallets sent
// them.
func (s *Server) sweepDepositSenders(addr string, start, end time.Time) (examined, deposits int, err error) {
	var row struct {
		Examined int
		Deposits int
	}
	err = s.db.Raw(`
WITH senders AS (
	SELECT DISTINCT from_addr FROM (
		SELECT from_addr FROM taxable_events
		WHERE to_addr = @addr AND category || '' = @transfer AND denom = @denom AND from_addr <> '' AND from_addr <> @addr
			AND timestamp >= @start AND timestamp < @end AND coalesce(reward_trigger, '') <> @withdraw
		LIMIT @sample) r
	LIMIT @senders),
moves AS (
	SELECT s.from_addr,
		coalesce(sum(o.amount::numeric) FILTER (WHERE o.category = @transfer AND o.to_addr = @addr), 0) AS swept,
		count(*) FILTER (WHERE NOT (o.category = @transfer AND o.to_addr = @addr)) AS other
	FROM senders s
	CROSS JOIN LATERAL (
		SELECT category, to_addr, amount FROM taxable_events
		WHERE from_addr = s.from_addr AND denom = @denom AND timestamp >= @start AND timestamp < @end
		LIMIT @per) o
	GROUP BY s.from_addr),
funded AS (
	SELECT m.swept, (
		SELECT coalesce(sum(i.amount::numeric), 0) FROM (
			SELECT amount FROM taxable_events
			WHERE to_addr = m.from_addr AND denom = @denom AND from_addr <> @addr AND timestamp >= @start AND timestamp < @end
			LIMIT @per) i) AS received
	FROM moves m WHERE m.other = 0)
SELECT (SELECT count(*) FROM senders) AS examined,
	(SELECT count(*) FROM funded WHERE received > 0 AND swept >= received * CAST(@forward AS numeric) AND swept <= received) AS deposits`,
		map[string]any{"addr": addr, "transfer": apiTransfer, "denom": s.native.Denom, "start": start, "end": end, "withdraw": tax.TriggerWithdrawAddress,
			"sample": flowSweepSample, "senders": flowSweepSenders, "per": flowSweepPerSender, "forward": flowSweepForwardMin}).Scan(&row).Error
	return row.Examined, row.Deposits, err
}

// memoDepositSenders counts the distinct senders that paid addr with a
// deposit-style memo, in a sample of its receipts.
func (s *Server) memoDepositSenders(addr string) (int, error) {
	var rows []struct {
		FromAddr string
		Memo     string
	}
	if err := s.db.Raw(`
SELECT te.from_addr, t.memo FROM taxable_events te
JOIN messages m ON m.id = te.message_id
JOIN txes t ON t.id = m.tx_id
WHERE te.to_addr = ? AND te.category IN ('transfer', 'ibc_in') AND t.memo <> '' AND coalesce(te.reward_trigger, '') <> ?
LIMIT ?`, addr, tax.TriggerWithdrawAddress, flowMemoSample).Scan(&rows).Error; err != nil {
		return 0, err
	}
	senders := map[string]bool{}
	for _, r := range rows {
		if r.FromAddr != "" && depositMemo(r.Memo) {
			senders[r.FromAddr] = true
		}
	}
	return len(senders), nil
}

// flowSkipSwap is the trigger on an IBC send whose memo asks Skip Go to swap.
const (
	flowSkipSwap   = "skip_swap"
	flowValoperHRP = "cosmosvaloper"
)

// dbFlowLoader reads a trace's wallets from the index.
type dbFlowLoader struct {
	s      *Server
	chain  string
	node   *ledgerNode
	labels map[string]AddressLabel
	mu     *sync.Mutex
	// wide holds the wide distributors this loader found, for the evidence.
	// They are never stored as address labels, since they depend on the
	// trace window.
	wide map[string]AddressLabel
	// fanoutLimit overrides flowFanoutMaxReceivers in tests.
	fanoutLimit int
	chans       map[string]string
	// slashed caches the window's validator slashes, by window.
	slashed map[[2]time.Time][]flowEvent
	// bonds caches the chain's self-bonds, by operator account.
	bonds   map[string][]flowEvent
	private bool
	// maxHops is the run's hop limit; zero means flowMaxHops.
	maxHops int
	// labelFile is the digest of the private label file this run added.
	labelFile string
	archive   *flowArchive
	progress  func(commissionFlowProgress)
	// prefetchN is how many wallets each trace may read ahead; zero reads
	// only when asked. pre is the trace's own read-ahead.
	prefetchN int
	pre       *flowPrefetch
}

func (l *dbFlowLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	if j := l.pre.wait(addr); j != nil && j.heldOK && j.day.Equal(day) {
		return l.holdingsWith(addr, day, &j.held)
	}
	return l.holdingsAt(addr, day)
}

func (l *dbFlowLoader) holdingsAt(addr string, day time.Time) (flowHoldings, bool, error) {
	return l.holdingsWith(addr, day, nil)
}

// holdingsWith is holdingsAt with db, when set, as the index read already
// made for this wallet and day.
func (l *dbFlowLoader) holdingsWith(addr string, day time.Time, db *flowHoldingRead) (flowHoldings, bool, error) {
	if l.archive != nil && l.chain == apiMainnet {
		if h, ok := l.archive.cachedHoldings(addr, day); ok {
			if l.private && h.Source.Kind != "archive_exact" {
				return h, false, fmt.Errorf("commission archive holding provenance missing")
			}
			return h, true, nil
		}
	}
	var h flowHoldings
	var source CommissionFlowHoldingSource
	var err error
	if db != nil {
		h, source = db.h, db.source
	} else {
		h, source, err = l.flowHoldingsAt(addr, day)
	}
	known := source.Kind != ""
	if err != nil || known || l.archive == nil {
		h.Source = source
		return h, known, err
	}
	h, err = l.archive.holdingsAt(l.s, l.chain, addr, day)
	if err == nil {
		if l.private && h.Source.Kind != "archive_exact" {
			return h, false, fmt.Errorf("commission archive holding provenance missing")
		}
	}
	return h, err == nil, err
}

func (s *Server) flowHoldingsAt(chain, addr string, day time.Time, strict bool) (flowHoldings, CommissionFlowHoldingSource, error) {
	bals, err := s.HistoricalBalances(chain, addr, []time.Time{day})
	if err != nil {
		return flowHoldings{}, CommissionFlowHoldingSource{}, err
	}
	if len(bals) != 1 {
		return flowHoldings{}, CommissionFlowHoldingSource{}, fmt.Errorf("commission historical holding result missing")
	}
	return flowHistoricalHoldings(bals[0], s.native.Denom, strict)
}

func flowHistoricalHoldings(b HistoricalBalance, denom string, strict bool) (flowHoldings, CommissionFlowHoldingSource, error) {
	h := flowHoldings{Liquid: decimal.Zero, Staked: map[string]decimal.Decimal{}, Unbonding: map[string]decimal.Decimal{}}
	if (strict && b.Height <= 0) || (b.Status != balanceExact && (strict || b.Status != balanceApproximate)) {
		return h, CommissionFlowHoldingSource{}, nil
	}
	for _, line := range b.Lines {
		if strict && line.Denom == denom && line.Approximate {
			return h, CommissionFlowHoldingSource{}, nil
		}
	}
	for _, line := range b.Lines {
		if line.Denom != denom {
			continue
		}
		v, err := decimal.NewFromString(line.AmountBase)
		if err != nil {
			return h, CommissionFlowHoldingSource{}, err
		}
		switch line.Bucket {
		case apiLiquid:
			h.Liquid = h.Liquid.Add(v)
		case apiStaked:
			h.Staked[line.Validator] = h.Staked[line.Validator].Add(v)
		case apiUnbonding:
			h.Unbonding[line.Validator] = h.Unbonding[line.Validator].Add(v)
		}
	}
	kind := "ledger_exact"
	if b.Status == balanceApproximate {
		kind = "ledger_approximate"
	}
	return h, CommissionFlowHoldingSource{Kind: kind, Height: b.Height}, nil
}

func (l *dbFlowLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	out, err := l.prefetchedEvents(addr, start, end)
	if err != nil {
		return nil, err
	}
	bonds, err := l.selfBonds()
	if err != nil {
		return nil, err
	}
	return withSelfBonds(out, bonds[addr], start, end), nil
}

// prefetchedEvents is flowEvents for addr, from the read-ahead when it read
// this wallet and window.
func (l *dbFlowLoader) prefetchedEvents(addr string, start, end time.Time) ([]flowEvent, error) {
	if j := l.pre.wait(addr); j != nil {
		l.pre.forget(addr, j)
		if j.eventsOK && j.day.AddDate(0, 0, 1).Equal(start) && j.end.Equal(end) {
			l.s.mergePrefetchReceipts(j.reader)
			return j.events, nil
		}
	}
	return flowRead(func() ([]flowEvent, error) { return l.s.flowEvents(l.chain, addr, start, end) })
}

// flowHoldingsAt is the index's holdings for addr at the end of day, read
// again when Postgres aborts the read.
func (l *dbFlowLoader) flowHoldingsAt(addr string, day time.Time) (flowHoldings, CommissionFlowHoldingSource, error) {
	type read struct {
		h      flowHoldings
		source CommissionFlowHoldingSource
	}
	r, err := flowRead(func() (read, error) {
		h, source, err := l.s.flowHoldingsAt(l.chain, addr, day, l.private)
		return read{h, source}, err
	})
	return r.h, r.source, err
}

// flowEvents is addr's native coin movements in [start,end), as the report
// sees them: classified events, block events and fees.
func (s *Server) flowEvents(chain, addr string, start, end time.Time) ([]flowEvent, error) {
	input, err := s.readCurrentChainInput(chain, []string{addr}, start, end)
	if err != nil {
		return nil, err
	}
	rows := input.Wallets[0]
	rows.Events = withoutWithdrawAddressMoves(rows.Events)
	credited, err := s.flowReturnCredits(addr, rows.Events)
	if err != nil {
		return nil, err
	}
	out := commissionFlowEventsFromRows(rows, addr, s.native.Denom, credited)
	paid, err := s.flowRewardPayouts(chain, addr, start, end)
	if err != nil {
		return nil, err
	}
	out = reconcileRewardPayouts(addr, out, paid)
	proceeds, err := s.flowSaleProceeds(addr, rows.Events)
	if err != nil {
		return nil, err
	}
	out = netSaleProceeds(out, addr, proceeds)
	out = payFilledBids(out, addr)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.before(out[j].Key) })
	return out, nil
}

// isWithdrawAddressMove reports the parser's transfer of a delegator's
// reward to the withdraw address the chain paid it to. The trace already
// takes reward coins from what the distribution module actually paid each
// wallet (flowRewardPayouts), so these rows would count the same coins
// again, and they are left out of every flow read.
func isWithdrawAddressMove(e tax.TaxableEvent) bool {
	return e.RewardTrigger == tax.TriggerWithdrawAddress && e.Category == apiTransfer
}

func withoutWithdrawAddressMoves(events []tax.TaxableEvent) []tax.TaxableEvent {
	out := make([]tax.TaxableEvent, 0, len(events))
	for _, e := range events {
		if !isWithdrawAddressMove(e) {
			out = append(out, e)
		}
	}
	return out
}

func commissionFlowEventsFromRows(rows walletRows, addr, native string, credited map[flowCredit]bool) []flowEvent {
	out := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees)+len(rows.CommissionCorrections))
	for _, e := range rows.Events {
		if isWithdrawAddressMove(e) {
			continue
		}
		if flowSingleHopNative(e, native) && !credited[flowCredit{e.MessageID, e.Amount + native}] {
			continue
		}
		if fe, ok := flowEventFromRow(e, addr, native); ok {
			out = append(out, fe)
		}
	}
	for _, f := range rows.Fees {
		if f.Denom != native {
			continue
		}
		v, err := decimal.NewFromString(f.Amount)
		if err != nil {
			continue
		}
		key := flowKey{Time: f.Ts.UTC(), Height: 0}
		if f.commissionKey != nil {
			key = *f.commissionKey
		}
		out = append(out, flowEvent{Key: key, Category: apiFee, Dir: apiOut, Amount: v, From: addr})
	}
	out = append(out, rows.CommissionCorrections...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.before(out[j].Key) })
	return out
}

// flowRewardPayout is the native reward amount the chain paid one wallet in
// one message: from the distribution module, or from a tokenize-share
// record's account settling that record's reward.
type flowRewardPayout struct {
	Key    flowKey
	Amount decimal.Decimal
}

// flowRewardPayouts is what the chain actually paid addr as reward in
// [start,end), by message. Reward rows name the delegator even when the
// chain paid its withdraw address, so the trace takes reward coins from
// these payouts instead. Most are paid by the distribution module; a
// tokenize-share record's reward is paid by the record's own account
// (shareRecordPayoutsFrom).
func (s *Server) flowRewardPayouts(chain, addr string, start, end time.Time) (map[uint]flowRewardPayout, error) {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	var rows []flowTransferAttr
	if err := s.db.Raw(`
WITH r AS MATERIALIZED (
  SELECT DISTINCT r.message_event_id FROM message_event_attributes r
  JOIN message_event_attribute_keys kr ON kr.id = r.message_event_attribute_key_id AND kr.key = 'recipient'
  WHERE left(r.value, 100) = left(@addr, 100) AND r.value = @addr),
ev AS MATERIALIZED (
  SELECT me.id, me.message_id, b.height, b.time_stamp FROM r
  JOIN message_events me ON me.id = r.message_event_id
  JOIN message_event_types t ON t.id = me.message_event_type_id AND t.type = 'transfer'
  JOIN messages m ON m.id = me.message_id
  JOIN txes x ON x.id = m.tx_id
  JOIN blocks b ON b.id = x.block_id
  WHERE b.chain_id IN @ids AND b.time_stamp >= @start AND b.time_stamp < @end)
SELECT ev.id AS event_id, ev.message_id, ev.height, ev.time_stamp, a.index, k.key, a.value
FROM ev
JOIN message_event_attributes a ON a.message_event_id = ev.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
ORDER BY ev.id, a.index`,
		map[string]any{"addr": addr, "ids": ids, "start": start, "end": end}).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("reward payouts: %w", err)
	}
	dist := moduleAddress("distribution")
	paid := rewardPayoutsFrom(rows, addr, dist, s.native.Denom)
	candidates := shareRewardCandidates(rows, addr, dist, s.native.Denom)
	if len(candidates) == 0 {
		return paid, nil
	}
	events, err := s.flowShareRewardEvents(candidates)
	if err != nil {
		return nil, err
	}
	for m, p := range shareRecordPayoutsFrom(rows, events, addr, s.native.Denom) {
		q := paid[m]
		q.Key = p.Key
		q.Amount = q.Amount.Add(p.Amount)
		paid[m] = q
	}
	return paid, nil
}

// flowTransferAttr is one attribute of a transfer event, in emitted order.
type flowTransferAttr struct {
	EventID   uint
	MessageID uint
	Height    int64
	TimeStamp time.Time
	Index     uint64
	Key       string
	Value     string
}

// rewardPayoutsFrom sums, by message, the native coins sender paid addr.
// Older SDK logs merge every transfer of a message into one event holding
// several recipient, sender, amount triples, so attributes are paired by
// position: each triple starts at a recipient.
func rewardPayoutsFrom(rows []flowTransferAttr, addr, sender, denom string) map[uint]flowRewardPayout {
	out := map[uint]flowRewardPayout{}
	eachTransfer(rows, func(cur flowTransferAttr, rcpt, from, amount string) {
		if rcpt == addr && from == sender {
			addRewardPayout(out, cur, nativeCoinAmount(amount, denom))
		}
	})
	return out
}

// eachTransfer calls fn with every recipient, sender, amount triple in rows,
// paired by position, and the last attribute row read for it.
func eachTransfer(rows []flowTransferAttr, fn func(cur flowTransferAttr, rcpt, from, amount string)) {
	var rcpt, from, amount string
	var cur flowTransferAttr
	flush := func() {
		if rcpt != "" || from != "" || amount != "" {
			fn(cur, rcpt, from, amount)
		}
		rcpt, from, amount = "", "", ""
	}
	for i, r := range rows {
		if i > 0 && r.EventID != rows[i-1].EventID {
			flush()
		}
		cur = r
		switch r.Key {
		case "recipient":
			flush()
			rcpt = r.Value
		case "sender":
			from = r.Value
		case "amount":
			amount = r.Value
		}
	}
	flush()
}

func addRewardPayout(out map[uint]flowRewardPayout, cur flowTransferAttr, v decimal.Decimal) {
	if !v.IsPositive() {
		return
	}
	p := out[cur.MessageID]
	p.Key = flowKey{Time: cur.TimeStamp.UTC(), Height: cur.Height, Message: cur.MessageID, Sub: flowPayoutSub}
	p.Amount = p.Amount.Add(v)
	out[cur.MessageID] = p
}

// shareRewardCandidates is the messages in which addr was paid native coin
// by an account other than the distribution module: the only messages where
// a tokenize-share record can have paid it a reward.
func shareRewardCandidates(rows []flowTransferAttr, addr, dist, denom string) []uint {
	seen := map[uint]bool{}
	var out []uint
	eachTransfer(rows, func(cur flowTransferAttr, rcpt, from, amount string) {
		if rcpt == addr && from != dist && from != addr && !seen[cur.MessageID] && nativeCoinAmount(amount, denom).IsPositive() {
			seen[cur.MessageID] = true
			out = append(out, cur.MessageID)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

const (
	flowEventShareReward     = "withdraw_tokenize_share_reward"
	flowEventWithdrawRewards = "withdraw_rewards"
)

// flowShareEventAttr is one attribute of a withdraw_tokenize_share_reward or
// withdraw_rewards event, in emitted order.
type flowShareEventAttr struct {
	EventID   uint
	MessageID uint
	Type      string
	Index     uint64
	Key       string
	Value     string
}

// flowShareRewardEvents reads the withdraw_tokenize_share_reward and
// withdraw_rewards events of those of ids that settled a tokenize-share
// record's reward. Each read is a per-row lookup (LATERAL with OFFSET 0), so a
// wallet with many candidate messages cannot turn it into a scan of the event
// tables.
func (s *Server) flowShareRewardEvents(ids []uint) ([]flowShareEventAttr, error) {
	var rows []flowShareEventAttr
	for start := 0; start < len(ids); start += 5000 {
		chunk := ids[start:min(start+5000, len(ids))]
		var part []flowShareEventAttr
		if err := s.db.Raw(`
WITH ev AS MATERIALIZED (
  SELECT me.id, m.id AS message_id, t.type FROM messages m
  JOIN LATERAL (SELECT id, message_event_type_id FROM message_events WHERE message_id = m.id OFFSET 0) me ON true
  JOIN message_event_types t ON t.id = me.message_event_type_id AND t.type IN ('withdraw_tokenize_share_reward', 'withdraw_rewards')
  WHERE m.id IN ?)
SELECT ev.id AS event_id, ev.message_id, ev.type, a.index, k.key, a.value
FROM ev
JOIN LATERAL (SELECT message_event_attribute_key_id, index, value FROM message_event_attributes WHERE message_event_id = ev.id OFFSET 0) a ON true
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE ev.message_id IN (SELECT message_id FROM ev WHERE type = 'withdraw_tokenize_share_reward')
ORDER BY ev.id, a.index`, chunk).Scan(&part).Error; err != nil {
			return nil, fmt.Errorf("share record reward events: %w", err)
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// shareRecordPayoutsFrom is, by message, the native reward tokenize-share
// records paid addr. The x/liquid module settles a record's reward in two
// steps: the record's account withdraws it from the distribution module
// (withdraw_rewards with that account as delegator) and pays it on to the
// owner's withdraw address, naming that address and the amount in
// withdraw_tokenize_share_reward. A redeem by another wallet settles the
// record the same way. So a transfer to addr counts only in a message whose
// withdraw_tokenize_share_reward names addr, only from an account that
// withdrew rewards in that same message, and never past the settled amount.
// Accounts are known only from these events, never from their address.
func shareRecordPayoutsFrom(transfers []flowTransferAttr, events []flowShareEventAttr, addr, denom string) map[uint]flowRewardPayout {
	settled := map[uint]decimal.Decimal{}
	payers := map[uint]map[string]bool{}
	var payee string
	for i, e := range events {
		if i > 0 && e.EventID != events[i-1].EventID {
			payee = ""
		}
		switch {
		case e.Type == flowEventShareReward && e.Key == "withdraw_address":
			payee = e.Value
		case e.Type == flowEventShareReward && e.Key == "amount":
			if payee == addr {
				settled[e.MessageID] = settled[e.MessageID].Add(nativeCoinAmount(e.Value, denom))
			}
			payee = ""
		case e.Type == flowEventWithdrawRewards && e.Key == "delegator" && e.Value != addr:
			if payers[e.MessageID] == nil {
				payers[e.MessageID] = map[string]bool{}
			}
			payers[e.MessageID][e.Value] = true
		}
	}
	out := map[uint]flowRewardPayout{}
	eachTransfer(transfers, func(cur flowTransferAttr, rcpt, from, amount string) {
		if rcpt == addr && settled[cur.MessageID].IsPositive() && payers[cur.MessageID][from] {
			addRewardPayout(out, cur, nativeCoinAmount(amount, denom))
		}
	})
	for m, p := range out {
		p.Amount = decimal.Min(p.Amount, settled[m])
		out[m] = p
	}
	return out
}

// flowPayoutSub orders a reward payout that has no reward row after the
// message's own rows.
const flowPayoutSub = 1 << 30

// nativeCoinAmount is denom's amount in a coins string such as "5uatom,7ibc/X".
func nativeCoinAmount(coins, denom string) decimal.Decimal {
	for _, c := range strings.Split(coins, ",") {
		n, ok := strings.CutSuffix(strings.TrimSpace(c), denom)
		if !ok || n == "" || strings.Trim(n, "0123456789") != "" {
			continue
		}
		if v, err := decimal.NewFromString(n); err == nil {
			return v
		}
	}
	return decimal.Zero
}

// reconcileRewardPayouts makes addr's reward inflows match what the chain
// paid it. In each message the reward rows booked to addr are cut to the
// reward payout to addr (less the commission rows and other rows from the
// distribution module, which already name the real receiver), so a delegator
// whose withdraw address is another account gets no coins it never held. A payout with no matching reward row
// (addr is someone else's withdraw address) becomes a reward inflow.
// Rewards without a message are left as booked. A native source correction
// is a reward proved against the chain: it is kept as is and uses its share
// of the payout.
func reconcileRewardPayouts(addr string, events []flowEvent, paid map[uint]flowRewardPayout) []flowEvent {
	room := map[uint]decimal.Decimal{}
	for m, p := range paid {
		room[m] = p.Amount
	}
	dist := moduleAddress("distribution")
	for _, e := range events {
		if e.Key.Message == 0 || e.Dir == apiOut {
			continue
		}
		if e.Category == apiReward && e.Trigger != flowSourceCorrectionTrigger {
			continue
		}
		if e.Category == apiCommission || e.From == dist || e.Trigger == flowSourceCorrectionTrigger {
			if r, ok := room[e.Key.Message]; ok {
				room[e.Key.Message] = decimal.Max(r.Sub(e.Amount), decimal.Zero)
			}
		}
	}
	out := events[:0:0]
	for _, e := range events {
		if e.Category != apiReward || e.Key.Message == 0 || e.Dir == apiOut || e.To != addr || e.Trigger == flowSourceCorrectionTrigger {
			out = append(out, e)
			continue
		}
		r := room[e.Key.Message]
		take := decimal.Min(r, e.Amount)
		room[e.Key.Message] = r.Sub(take)
		if take.IsPositive() {
			e.Amount = take
			out = append(out, e)
		}
	}
	msgs := make([]uint, 0, len(room))
	for m, r := range room {
		if r.IsPositive() {
			msgs = append(msgs, m)
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i] < msgs[j] })
	for _, m := range msgs {
		p := paid[m]
		out = append(out, flowEvent{Key: p.Key, Category: apiReward, Dir: "in", Amount: room[m], From: dist, To: addr, Trigger: flowWithdrawAddressTrigger})
	}
	return out
}

// flowWithdrawAddressTrigger marks a reward the chain paid addr as another
// delegator's withdraw address.
const flowWithdrawAddressTrigger = "withdraw_address"

// flowSourceCorrectionTrigger marks a reward credit a native source
// correction added.
const flowSourceCorrectionTrigger = "native_source_correction"

// flowSingleHopNative reports whether e is an ibc_in row of the native denom
// under a one-hop trace, which the chain releases from escrow as native coin.
func flowSingleHopNative(e tax.TaxableEvent, native string) bool {
	if e.Category != string(tax.CategoryIBCIn) || strings.Count(e.Denom, "/") != 2 {
		return false
	}
	base, ok := ibcBaseDenom(e.Denom)
	return ok && base == native
}

type flowCredit struct {
	message uint
	coin    string
}

// flowReturnCredits finds the native coin the chain credited to addr in the
// messages of its single-hop ibc_in rows. The ledger also records a receive
// whose acknowledgement was an error (a bad receiver, a failed forward), where
// no coin moved, so a returning row counts only when its message credited
// addr exactly that amount.
func (s *Server) flowReturnCredits(addr string, events []tax.TaxableEvent) (map[flowCredit]bool, error) {
	var ids []uint
	for _, e := range events {
		if flowSingleHopNative(e, s.native.Denom) && e.ToAddr == addr {
			ids = append(ids, e.MessageID)
		}
	}
	rows, err := s.flowReceived(addr, ids)
	if err != nil {
		return nil, err
	}
	out := map[flowCredit]bool{}
	for _, r := range rows {
		for _, coin := range strings.Split(r.Value, ",") {
			out[flowCredit{r.MessageID, strings.TrimSpace(coin)}] = true
		}
	}
	return out, nil
}

// flowReceipt is one coin_received amount the chain paid a wallet in a
// message.
type flowReceipt struct {
	MessageID uint
	Value     string
}

// flowReceived reads the coin_received amounts paid to addr in the messages
// ids. Attributes are read per event (LATERAL with OFFSET 0), so a wallet with
// many such messages cannot turn the read into a hash join over the whole
// attribute table.
func (s *Server) flowReceived(addr string, ids []uint) ([]flowReceipt, error) {
	var rows []flowReceipt
	for start := 0; start < len(ids); start += 5000 {
		chunk := ids[start:min(start+5000, len(ids))]
		var part []flowReceipt
		if err := s.db.Raw(`
SELECT me.message_id, a2.value
FROM message_events me
JOIN message_event_types mt ON mt.id = me.message_event_type_id AND mt.type = 'coin_received'
JOIN LATERAL (SELECT message_event_attribute_key_id, value FROM message_event_attributes WHERE message_event_id = me.id OFFSET 0) a1 ON true
JOIN message_event_attribute_keys k1 ON k1.id = a1.message_event_attribute_key_id AND k1.key = 'receiver'
JOIN LATERAL (SELECT message_event_attribute_key_id, value FROM message_event_attributes WHERE message_event_id = me.id OFFSET 0) a2 ON true
JOIN message_event_attribute_keys k2 ON k2.id = a2.message_event_attribute_key_id AND k2.key = 'amount'
WHERE me.message_id IN ? AND a1.value = ?`, chunk, addr).Scan(&part).Error; err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// flowEventFromRow reads one ledger row as the movement of addr's native coin
// it caused, or false when it moved none. Rows are classified for the tax
// report, so a few carry their amount in a different sense:
//   - nft_sale names the seller as From and the buyer as To with the price as
//     Amount: the NFT moves from seller to buyer, the coin the other way.
//   - nft_mint names the owner as To and carries the mint cost the owner paid.
//   - ibc_in of a single-hop trace of the native denom
//     ("transfer/channel-N/uatom") is native coin coming home: the chain
//     releases it from escrow as the native denom. flowEvents keeps such a
//     row only when the chain credited it (flowReturnCredits). Longer traces
//     are vouchers on this chain, not native coin.
func flowEventFromRow(e tax.TaxableEvent, addr, native string) (flowEvent, bool) {
	// The paying leg the report adds to a stream exit is the subscription
	// lot, which the exit itself swaps here; reading it too would spend the
	// coins twice.
	if e.RewardTrigger == flowStreamExit && e.FromAddr == addr && e.ToAddr != addr {
		return flowEvent{}, false
	}
	// A tokenize row names the delegator as From and the share-record owner
	// as To. Only the delegator's native stake leaves its pool. Receiving the
	// record does not spend the recipient's stake or credit liquid ATOM.
	if e.Category == flowCatTokenize && e.FromAddr != addr {
		return flowEvent{}, false
	}
	denom, amount := e.Denom, e.Amount
	if bonded, bondedAmount, isShare := lsmUnderlying(e.Category, e.Denom, e.Detail); isShare {
		denom, amount = bonded, bondedAmount
	}
	if e.Category == string(tax.CategoryIBCIn) && strings.Count(denom, "/") == 2 {
		if base, ok := ibcBaseDenom(denom); ok && base == native {
			denom = native
		}
	}
	if denom != native {
		// A stream exit pays the purchase in the streamed denom; the
		// subscription it spent is native coin waiting in the stream.
		if e.RewardTrigger == flowStreamExit && e.ToAddr == addr && e.FromAddr != addr {
			if lot := flowStreamLot(e); lot != "" {
				return flowEvent{
					Key:      flowKey{Time: e.Timestamp.UTC(), Height: e.BlockHeight, Message: e.MessageID, Sub: e.SubIndex},
					Category: e.Category, Dir: "in", Amount: decimal.Zero,
					From: e.FromAddr, To: e.ToAddr, Trigger: e.RewardTrigger, Detail: e.Detail, Lot: lot,
				}, true
			}
		}
		return flowEvent{}, false
	}
	v, err := decimal.NewFromString(amount)
	if err != nil {
		return flowEvent{}, false
	}
	dir := eventDirection(e, addr)
	switch e.Category {
	case apiNFTSale:
		if e.FromAddr == e.ToAddr {
			return flowEvent{}, false
		}
		if dir == apiOut {
			dir = "in"
		} else {
			dir = apiOut
		}
	case apiNFTMint:
		dir = apiOut
	}
	fe := flowEvent{
		Key:      flowKey{Time: e.Timestamp.UTC(), Height: e.BlockHeight, Message: e.MessageID, Sub: e.SubIndex},
		Category: e.Category, Dir: dir, Amount: v,
		From: e.FromAddr, To: e.ToAddr, Validator: e.ValidatorAddress, Trigger: e.RewardTrigger, Detail: e.Detail,
	}
	if e.RewardTrigger == flowAuctionBid || e.RewardTrigger == flowAuctionRefund || e.RewardTrigger == flowBidFilled || (e.Category == apiNFTSale && flowEscrowedBid(e.Detail)) {
		fe.Lot = flowBidLot(e)
	}
	if e.RewardTrigger == flowStreamSubscribe || e.RewardTrigger == flowStreamWithdraw || e.RewardTrigger == flowStreamRefund {
		fe.Lot = flowStreamLot(e)
	}
	if e.RewardTrigger == tax.TriggerLimitOrder || e.RewardTrigger == tax.TriggerLimitCancel || (dir == apiOut && detailValue(e.Detail, "lot_basis") != "" && (e.RewardTrigger == tax.TriggerLimitFill || e.RewardTrigger == tax.TriggerLimitFee)) {
		fe.Lot = flowLimitLot(e)
	}
	switch e.Category {
	case flowCatRedelegate:
		fe.SrcValidator = detailValue(e.Detail, "source_validator")
	case flowCatTokenize, flowCatRedeem:
		if fe.Validator == "" {
			fe.Validator, _, _ = strings.Cut(e.Denom, "/")
		}
	}
	return fe, true
}

func detailValue(detail, key string) string {
	if detail == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(detail), &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func labelStop(lab AddressLabel) flowStop {
	switch {
	case lab.Kind == labelModule && lab.Name == "distribution":
		return flowStop{Class: flowCommunityPool}
	case lab.Kind == labelModule:
		return flowStop{Class: flowModule, Detail: lab.Name}
	case lab.Kind == labelIBCEscrow:
		return flowStop{Class: flowIBCOut, Detail: lab.Name}
	case lab.Kind == labelExchangeLike:
		return flowStop{Class: flowExchangeLike, Detail: lab.Source}
	case lab.Kind == labelExchange:
		return flowStop{Class: flowExchange, Detail: lab.Name}
	case lab.Kind == labelInterchainAccount:
		return flowStop{Class: flowInterchainAccount, Detail: lab.Name}
	case lab.Kind == labelGrantRecipient:
		return flowStop{Class: flowGrantRecipient, Detail: lab.Name}
	case lab.Kind == labelBridgeEscrow, lab.Kind == labelBridgeContract:
		return flowStop{Class: flowBridge, Detail: lab.Name}
	case lab.Kind == labelDexPool:
		return flowStop{Class: flowDexPool, Detail: lab.Name}
	case lab.Kind == labelContract:
		return flowStop{Class: flowContract, Detail: lab.Name}
	default:
		panic(fmt.Sprintf("label kind %q has no stop class", lab.Kind))
	}
}

// contractAddress is a 32-byte account: a contract or an interchain account.
func contractAddress(addr string) bool { return len(addr) > len("cosmos1")+38+1 }

// known is the stop for addr from its label or address shape alone, with no
// index read. Exchange-like labels are inferred during runs, so they are left
// out and the answer does not depend on which trace inferred one first.
func (l *dbFlowLoader) known(addr string) flowStop {
	l.mu.Lock()
	lab, ok := l.labels[addr]
	l.mu.Unlock()
	if ok && lab.Kind != labelValidatorOperator && lab.Kind != labelExchangeLike {
		return labelStop(lab)
	}
	if contractAddress(addr) {
		return flowStop{Class: flowContract}
	}
	return flowStop{}
}

func (l *dbFlowLoader) stop(addr string, start, end time.Time) (flowStop, error) {
	l.mu.Lock()
	lab, ok := l.labels[addr]
	l.mu.Unlock()
	if ok && lab.Kind != labelValidatorOperator {
		return labelStop(lab), nil
	}
	if contractAddress(addr) {
		return flowStop{Class: flowContract}, nil
	}
	j := l.pre.wait(addr)
	var n int
	var err error
	if j != nil && j.memoOK {
		n = j.memo
	} else if n, err = flowRead(func() (int, error) { return l.s.memoDepositSenders(addr) }); err != nil {
		return flowStop{}, err
	}
	if n >= flowMemoMinSenders {
		lab := AddressLabel{
			Chain: l.chain, Address: addr, Name: "deposit-style address", Kind: labelExchangeLike, Source: flowMemoHeuristic,
			Evidence: fmt.Sprintf("inferred; conditions met: memo-deposit; %d distinct senders with deposit-style memos in a sample of %d receipts", n, flowMemoSample), CreatedAt: nowUTC(),
		}
		if !l.private {
			if err := l.s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&lab).Error; err != nil {
				return flowStop{}, err
			}
		}
		l.mu.Lock()
		l.labels[addr] = lab
		l.mu.Unlock()
		return flowStop{Class: flowExchangeLike, Detail: flowMemoHeuristic}, nil
	}
	var rows int64
	if j != nil && j.rowsOK {
		rows = j.rows
	} else if rows, err = flowRead(func() (int64, error) { return l.s.walletRows(addr, ledgerHeavyRows) }); err != nil {
		return flowStop{}, err
	}
	if rows > ledgerHeavyRows {
		return l.busy(addr, rows, start, end)
	}
	// Last, so labels and the busy cutoff keep their fates; it also applies
	// to validator operator accounts.
	return l.distributor(addr, start, end)
}

// busy is the stop for a wallet too active to trace: exchange-like when its
// senders are deposit addresses, a wide distributor when it pays out to a
// crowd, otherwise busy_wallet with the measurements kept as evidence.
func (l *dbFlowLoader) busy(addr string, rows int64, start, end time.Time) (flowStop, error) {
	read, err := flowRead(func() ([2]int, error) {
		examined, deposits, err := l.s.sweepDepositSenders(addr, start, end)
		return [2]int{examined, deposits}, err
	})
	examined, deposits := read[0], read[1]
	if err != nil {
		return flowStop{}, err
	}
	sweep := fmt.Sprintf("%d of %d sampled senders from %s to %s pass through to it", deposits, examined, start.Format(apiDateLayout), end.Format(apiDateLayout))
	if sweepExchange(examined, deposits) {
		lab := AddressLabel{
			Chain: l.chain, Address: addr, Name: "deposit-sweep wallet", Kind: labelExchangeLike, Source: flowSweepHeuristic,
			Evidence: fmt.Sprintf("inferred; conditions met: deposit-sweep; more than %d ledger rows; %s", ledgerHeavyRows, sweep), CreatedAt: nowUTC(),
		}
		if !l.private {
			if err := l.s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&lab).Error; err != nil {
				return flowStop{}, err
			}
		}
		l.mu.Lock()
		l.labels[addr] = lab
		l.mu.Unlock()
		return flowStop{Class: flowExchangeLike, Detail: flowSweepHeuristic}, nil
	}
	stop, err := l.distributor(addr, start, end)
	if err != nil || stop.Class != "" {
		return stop, err
	}
	lab := AddressLabel{
		Chain: l.chain, Address: addr, Name: "busy wallet", Kind: labelBusyWallet, Source: flowBusyMeasured,
		Evidence: fmt.Sprintf("%d ledger rows or more; %s; sent to at most %d wallets", rows, sweep, flowFanoutMaxReceivers), CreatedAt: nowUTC(),
	}
	l.mu.Lock()
	if l.wide == nil {
		l.wide = map[string]AddressLabel{}
	}
	if _, seen := l.wide[addr]; !seen {
		l.wide[addr] = lab
	}
	l.mu.Unlock()
	return flowStop{Class: flowBusyWallet}, nil
}

func sweepExchange(examined, deposits int) bool {
	return deposits >= flowSweepMinDeposits && 2*deposits >= examined
}

// distributor is the wide distributor stop for addr over [start, end).
func (l *dbFlowLoader) distributor(addr string, start, end time.Time) (flowStop, error) {
	limit := l.fanoutLimit
	if limit <= 0 {
		limit = flowFanoutMaxReceivers
	}
	n, err := flowRead(func() (int, error) { return l.s.fanoutReceivers(addr, start, end, limit) })
	if err != nil {
		return flowStop{}, err
	}
	if n <= limit {
		return flowStop{}, nil
	}
	lab := AddressLabel{
		Chain: l.chain, Address: addr, Name: "wide distributor", Kind: labelWideDistributor, Source: flowFanoutHeuristic,
		Evidence: fmt.Sprintf("more than %d distinct receivers of %s from %s to %s", limit, l.s.native.Denom, start.Format(apiDateLayout), end.Format(apiDateLayout)), CreatedAt: nowUTC(),
	}
	l.mu.Lock()
	if l.wide == nil {
		l.wide = map[string]AddressLabel{}
	}
	if _, seen := l.wide[addr]; !seen {
		l.wide[addr] = lab
	}
	l.mu.Unlock()
	return flowStop{Class: flowWideDistributor, Detail: flowFanoutHeuristic}, nil
}

func (l *dbFlowLoader) ibcDestination(e flowEvent) string {
	dest := l.channelChain(detailValue(e.Detail, "packet_src_port"), detailValue(e.Detail, "packet_src_channel"))
	if dest == "" {
		if c := detailValue(e.Detail, "packet_destination_client"); c != "" {
			dest = "client " + c
		} else {
			dest = "unknown channel"
		}
	}
	if e.Trigger == flowSkipSwap {
		dest += " (swap)"
	}
	return dest
}

// channelChain is the chain id across a channel, from the stored map or the
// node. A channel the node cannot answer for is named by itself.
func (l *dbFlowLoader) channelChain(port, channel string) string {
	if port == "" || channel == "" {
		return ""
	}
	key := port + "/" + channel
	l.mu.Lock()
	if v, ok := l.chans[key]; ok {
		l.mu.Unlock()
		return v
	}
	l.mu.Unlock()
	v := channel
	var row IBCChannel
	if err := l.s.db.Where("chain = ? AND port = ? AND channel = ?", l.chain, port, channel).Limit(1).Find(&row).Error; err == nil && row.ChainID != "" {
		v = row.ChainID
	} else if l.node != nil && l.node.rest != nil {
		var body struct {
			Identified struct {
				ClientID    string `json:"client_id"`
				ClientState struct {
					ChainID string `json:"chain_id"`
				} `json:"client_state"`
			} `json:"identified_client_state"`
		}
		if err := l.node.rest.GetJSON("/ibc/core/channel/v1/channels/"+channel+"/ports/"+port+"/client_state", nil, &body); err == nil && body.Identified.ClientState.ChainID != "" {
			v = body.Identified.ClientState.ChainID
			if !l.private {
				_ = l.s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&IBCChannel{Chain: l.chain, Port: port, Channel: channel, ClientID: body.Identified.ClientID, ChainID: v, FetchedAt: nowUTC()}).Error
			}
		}
	}
	l.mu.Lock()
	l.chans[key] = v
	l.mu.Unlock()
	return v
}

// commissionReceivers lists the wallets validator's commission was paid to.
func (s *Server) commissionReceivers(chain, validator string) ([]CommissionReceiver, error) {
	var out []CommissionReceiver
	err := s.scopedTaxEvents(chain).
		Select("taxable_events.to_addr AS address, MIN(taxable_events.timestamp) AS first_at, MAX(taxable_events.timestamp) AS last_at, COUNT(*) AS withdrawals, SUM(CASE WHEN taxable_events.denom = ? THEN taxable_events.amount::numeric ELSE 0 END)::text AS amount_base", s.native.Denom).
		Where("taxable_events.validator_address = ? AND taxable_events.category = ?", validator, apiCommission).
		Group("taxable_events.to_addr").Order("first_at").Scan(&out).Error
	return out, err
}

// CommissionReceiver is one wallet a validator's commission was paid to.
type CommissionReceiver struct {
	Address     string    `json:"address"`
	FirstAt     time.Time `json:"first_withdrawal"`
	LastAt      time.Time `json:"last_withdrawal"`
	Withdrawals int       `json:"withdrawals"`
	AmountBase  string    `json:"amount_base"`
	Amount      string    `json:"amount" gorm:"-"`
}

// commissionUntracked is the commission paid in other denoms, by month.
func (s *Server) commissionUntracked(chain, validator string, from, to time.Time) (map[string]map[string]string, error) {
	var rows []struct {
		Month  string
		Denom  string
		Amount string
	}
	if err := s.scopedTaxEvents(chain).
		Select("to_char(taxable_events.timestamp AT TIME ZONE 'UTC', 'YYYY-MM') AS month, taxable_events.denom, SUM(taxable_events.amount::numeric)::text AS amount").
		Where("taxable_events.validator_address = ? AND taxable_events.category = ? AND taxable_events.denom <> ?", validator, apiCommission, s.native.Denom).
		Where("taxable_events.timestamp >= ? AND taxable_events.timestamp < ?", from, to).
		Group("month, taxable_events.denom").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, r := range rows {
		if out[r.Month] == nil {
			out[r.Month] = map[string]string{}
		}
		out[r.Month][r.Denom] = r.Amount
	}
	return out, nil
}

// commissionValidators lists every validator that paid commission on chain.
func (s *Server) commissionValidators(chain string) ([]string, error) {
	var out []string
	err := s.scopedTaxEvents(chain).
		Distinct("taxable_events.validator_address").
		Where("taxable_events.category = ? AND taxable_events.validator_address <> ''", apiCommission).
		Order("taxable_events.validator_address").Pluck("taxable_events.validator_address", &out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// commissionFirstWithdrawals maps every validator that ever withdrew
// commission on chain to its first withdrawal.
func (s *Server) commissionFirstWithdrawals(chain string) (map[string]time.Time, error) {
	var rows []struct {
		Validator string
		FirstAt   time.Time
	}
	err := s.scopedTaxEvents(chain).
		Select("taxable_events.validator_address AS validator, MIN(taxable_events.timestamp) AS first_at").
		Where("taxable_events.category = ? AND taxable_events.validator_address <> ''", apiCommission).
		Group("taxable_events.validator_address").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[r.Validator] = r.FirstAt
	}
	return out, nil
}

// BuildCommissionFlows traces every validator's commission (or those named)
// from from through the end of yesterday and stores the result.
func (s *Server) BuildCommissionFlows(chain string, node *ledgerNode, from time.Time, only []string, workers int, logf func(string, ...any), options ...CommissionFlowBuildOptions) error {
	asOf := flowDay(nowUTC())
	var loader *dbFlowLoader
	save := func(run CommissionFlowRun, rows []CommissionFlow, _ CommissionFlowEvidence) error {
		return s.saveCommissionFlows(run, rows)
	}
	if len(options) > 1 {
		return fmt.Errorf("only one commission output version is allowed")
	}
	if len(options) == 1 {
		o := options[0]
		if err := o.validate(); err != nil {
			return err
		}
		if o.PrivateProfile != nil && !from.Equal(o.PrivateProfile.From) {
			return fmt.Errorf("private projection generation start mismatch")
		}
		if o.SourceCorrections != nil && !from.Equal(o.SourceCorrections.From) {
			return fmt.Errorf("native correction generation start mismatch")
		}
		if !from.Before(o.AsOf) || o.AsOf.After(asOf) {
			return fmt.Errorf("invalid commission trace date range")
		}
		if err := CommissionFlowVersionsMigrate(s.db); err != nil {
			return err
		}
		asOf = o.AsOf
		save = func(run CommissionFlowRun, rows []CommissionFlow, evidence CommissionFlowEvidence) error {
			return s.saveCommissionFlowVersion(o, run, rows, evidence)
		}
	} else if err := CommissionFlowsMigrate(s.db); err != nil {
		return err
	}
	validators, err := s.commissionValidators(chain)
	if err != nil {
		return err
	}
	derived := derivedFlowLabels(chain, validators)
	if len(options) == 0 {
		if err := s.seedAddressLabels(chain, validators); err != nil {
			return err
		}
	}
	if len(only) > 0 {
		validators = only
	}
	var labels []AddressLabel
	if err := s.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
		return err
	}
	reader := s
	if len(options) == 1 {
		reader = NewServer(s.db, s.oracle, s.native)
		reader.readOnlyLedger = true
		reader.privateReadSnapshotTimeout = options[0].ReadSnapshotTimeout
		reader.commissionPrivateProfile = options[0].PrivateProfile
		reader.commissionSourceCorrections = options[0].SourceCorrections
		if reader.commissionSourceCorrections != nil {
			reader.sourceCorrectionReceipts = newCorrectionJournal()
		}
		if reader.commissionPrivateProfile != nil {
			reader.privateProjectionReceipts = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
		}
	}
	loader = &dbFlowLoader{s: reader, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: len(options) == 1}
	if len(options) == 1 {
		loader.maxHops = options[0].MaxHops
		loader.prefetchN = options[0].WalletPrefetch
	}
	var progressSink *commissionProgressSink
	if root := os.Getenv("COSMOS_TAX_COMMISSION_CAPTURE_DIR"); loader.private && root != "" {
		progressSink, err = newCommissionProgressSink(root, validators, 5*time.Second)
		if err != nil {
			return fmt.Errorf("initialize private progress snapshots: %w", err)
		}
		defer progressSink.close()
		loader.progress = func(p commissionFlowProgress) {
			p.Version, p.Build = options[0].Version, options[0].Build
			p.JobArchiveCallAttempts, p.JobHeaderCacheReuses = -1, -1
			if loader.archive != nil {
				p.JobArchiveCallAttempts = loader.archive.callAttempts.Load()
				p.JobHeaderCacheReuses = loader.archive.headerReuses.Load()
			}
			progressSink.update(p)
		}
	}
	if len(options) == 1 && options[0].ArchiveRPC != "" {
		var err error
		loader.archive, err = newFlowArchive(options[0].ArchiveRPC)
		if err != nil {
			return err
		}
	}
	for _, l := range labels {
		loader.labels[l.Address] = l
	}
	for _, l := range derived {
		if _, exists := loader.labels[l.Address]; !exists {
			loader.labels[l.Address] = l
		}
	}
	for _, l := range loader.labels {
		if !labelKindsKnown[l.Kind] {
			return fmt.Errorf("label kind %q has no stop class", l.Kind)
		}
	}
	if len(options) == 1 && options[0].Labels != nil {
		if options[0].Labels.Chain != chain {
			return fmt.Errorf("label file is for another chain")
		}
		loader.labelFile = options[0].Labels.digest
		for _, l := range options[0].Labels.Labels {
			if _, exists := loader.labels[l.Address]; !exists {
				loader.labels[l.Address] = l
			}
		}
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var failed int
	var failMu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range jobs {
				if len(options) == 1 {
					exists, err := s.reuseCommissionFlowVersion(options[0], chain, v, from)
					if err != nil {
						failMu.Lock()
						failed++
						failMu.Unlock()
						logf("commission flows: immutable output cannot be reused: %v", err)
						continue
					}
					if exists {
						logf("commission flows: %s immutable output retained", v)
						continue
					}
				}
				started := time.Now()
				run, err := reader.traceCommissionSaved(chain, loader, v, from, asOf, save)
				if err != nil {
					failMu.Lock()
					failed++
					failMu.Unlock()
					logf("commission flows: %s: %v", v, err)
					continue
				}
				logf("commission flows: %s %s: %d wallets, drift %s, %s", v, run.Status, run.Wallets, run.DriftBase, time.Since(started).Round(time.Second))
			}
		}()
	}
	for _, v := range validators {
		jobs <- v
	}
	close(jobs)
	wg.Wait()
	var progressErr error
	if progressSink != nil {
		if err := progressSink.close(); err != nil {
			progressErr = fmt.Errorf("private progress snapshots: %w", err)
		}
	}
	if failed > 0 {
		return errors.Join(fmt.Errorf("%d of %d validators failed", failed, len(validators)), progressErr)
	}
	return progressErr
}

const (
	flowStatusOK         = "ok"
	flowStatusReconciled = "reconciled"
	flowStatusDrift      = "drift"
	flowStatusError      = "error"
)

// flowDriftTolerance is how far, in base units, a traced wallet's simulated
// holdings may be from the chain's and still count as reconciled. Unclaimed
// rewards are not in a rebuilt balance, and a slash's fraction is derived
// from the validator's power at the infraction, not its exact tokens.
var flowDriftTolerance = decimal.NewFromInt(1000000)

func (s *Server) traceCommissionSaved(chain string, loader *dbFlowLoader, validator string, from, asOf time.Time, save func(CommissionFlowRun, []CommissionFlow, CommissionFlowEvidence) error) (CommissionFlowRun, error) {
	if s.commissionPrivateProfile != nil || s.commissionSourceCorrections != nil {
		s, loader = s.privateProjectionTrace(loader)
	}
	if loader.prefetchN > 0 {
		own := *loader
		own.pre = newFlowPrefetch(&own, own.prefetchN)
		loader = &own
		defer loader.closePrefetch()
	}
	run := CommissionFlowRun{Chain: chain, Validator: validator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: nowUTC(), Overdraw: "{}", Commission: "{}", Untracked: "{}", DriftBase: "0"}
	tr := newFlowTrace(validator, from, asOf, loader)
	tr.maxHops = loader.maxHops
	tr.strict = loader.private
	if loader.private {
		tr.progress = loader.progress
	}
	tr.reportProgress("root_started", "", -1)
	var walletEvidence []CommissionFlowWalletEvidence
	fail := func(err error) (CommissionFlowRun, error) {
		run.Status, run.Reason = flowStatusError, err.Error()
		if errors.Is(err, errAwaitingCorrection) {
			run.Reason = "a receiving wallet has rows awaiting a methodology correction"
		}
		evidence := loader.evidence()
		evidence.Wallets = walletEvidence
		if loader.private {
			evidence.retainReadFailure(err, tr.order)
		}
		if loader.archive != nil {
			evidence.Archive = loader.archive.evidence(tr.attempted)
		}
		tr.reportProgress("saving_error_started", "", -1)
		if saveErr := save(run, nil, evidence); saveErr != nil {
			tr.reportProgress("save_failed", "", -1)
			return run, errors.Join(err, saveErr)
		}
		tr.reportProgress("error_saved", "", -1)
		return run, err
	}
	receivers, err := s.commissionReceivers(chain, validator)
	if err != nil {
		return fail(err)
	}
	group := []string{}
	seen := map[string]bool{}
	for _, r := range receivers {
		if r.Address != "" && !seen[r.Address] {
			seen[r.Address] = true
			group = append(group, r.Address)
		}
	}
	if op := operatorAccount(validator); op != "" && !seen[op] {
		group = append(group, op)
	}
	run.Receivers = len(receivers)
	if err := tr.run(group); err != nil {
		return fail(err)
	}
	if loader.private && len(tr.Unresolved) > 0 {
		return fail(fmt.Errorf("commission destinations remain unresolved"))
	}
	overdraw := map[string]string{}
	var total [flowRules]decimal.Decimal
	drift := decimal.Zero
	asOfDay := asOf.AddDate(0, 0, -1)
	closing := loader.closingReads(tr.order, asOfDay)
	defer closing.close()
	for i, w := range tr.order {
		run.Wallets++
		if !w.Known {
			run.OpeningUnknown++
			if loader.private {
				return fail(fmt.Errorf("commission opening requires exact archive state"))
			}
		}
		for r := 0; r < flowRules; r++ {
			total[r] = total[r].Add(w.Overdraw[r])
		}
		tr.reportProgress("closing_started", w.Addr, len(tr.order)-i)
		actual, known, err := closing.holdingsAt(loader, i, w.Addr, asOfDay)
		if err != nil || !known {
			if loader.private {
				return fail(fmt.Errorf("commission closing requires exact archive state"))
			}
			continue
		}
		tr.closingReadCompletedWallets++
		tr.reportProgress("closing_complete", w.Addr, len(tr.order)-i-1)
		drift = drift.Add(w.simulated().Sub(actual.total()).Abs())
		if loader.private {
			e := commissionFlowWalletEvidence(w, actual, asOfDay)
			e.OpeningSource = w.OpeningSource
			e.ClosingSource = actual.Source
			walletEvidence = append(walletEvidence, e)
		}
	}
	for r := 0; r < flowRules; r++ {
		overdraw[flowRuleNames[r]] = total[r].String()
	}
	run.Unresolved = len(tr.Unresolved)
	run.DriftBase = drift.String()
	run.Status = flowStatusReconciled
	if drift.GreaterThan(flowDriftTolerance.Mul(decimal.NewFromInt(int64(run.Wallets)))) {
		run.Status = flowStatusDrift
	}
	if run.Wallets == 0 {
		run.Status = flowStatusOK
	}
	b, _ := json.Marshal(overdraw)
	run.Overdraw = string(b)
	commission := map[string]string{}
	for m, v := range tr.Commission {
		commission[m] = v.String()
	}
	b, _ = json.Marshal(commission)
	run.Commission = string(b)
	untracked, err := s.commissionUntracked(chain, validator, from, asOf)
	if err != nil {
		return fail(err)
	}
	b, _ = json.Marshal(untracked)
	run.Untracked = string(b)

	rows := make([]CommissionFlow, 0, len(tr.Fates))
	for k, v := range tr.Fates {
		if !v.IsPositive() {
			continue
		}
		rows = append(rows, CommissionFlow{Chain: chain, Validator: validator, Rule: flowRuleNames[k.Rule], Month: k.Month, Class: k.Class, Detail: k.Detail, ViaUnbonding: k.ViaUnbonding, AmountBase: v.String()})
	}
	evidence := loader.evidence()
	evidence.Wallets = walletEvidence
	if loader.private {
		evidence.MaxHops = loader.maxHops
		evidence.LabelFile = loader.labelFile
		evidence.Stops = commissionFlowStopEvidence(tr.Stops)
	}
	if loader.archive != nil {
		evidence.Archive = loader.archive.evidence(tr.attempted)
	}
	tr.reportProgress("saving_started", "", 0)
	if err := save(run, rows, evidence); err != nil {
		tr.reportProgress("save_failed", "", 0)
		return run, err
	}
	tr.reportProgress("saved", "", 0)
	return run, nil
}

// saveCommissionFlows replaces a validator's stored result. A failed run
// keeps the last good flows and records only its own status.
func (s *Server) saveCommissionFlows(run CommissionFlowRun, rows []CommissionFlow) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if run.Status != flowStatusError {
			if err := tx.Where("chain = ? AND validator = ?", run.Chain, run.Validator).Delete(&CommissionFlow{}).Error; err != nil {
				return err
			}
			if len(rows) > 0 {
				if err := tx.CreateInBatches(rows, 500).Error; err != nil {
					return err
				}
			}
			return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&run).Error
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: apiChain}, {Name: apiValidator}},
			DoUpdates: clause.AssignmentColumns([]string{apiStatus, "reason", "computed_at"}),
		}).Create(&run).Error
	})
}
