package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
)

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

var flowMemoShape = regexp.MustCompile(`^[0-9A-Za-z_-]+$`)

func depositMemo(memo string) bool {
	memo = strings.TrimSpace(memo)
	return len(memo) >= flowMemoMinLen && len(memo) <= flowMemoMaxLen && flowMemoShape.MatchString(memo) && strings.ContainsAny(memo, flowMemoDigitsCheck)
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
WHERE te.to_addr = ? AND te.category IN ('transfer', 'ibc_in') AND t.memo <> ''
LIMIT ?`, addr, flowMemoSample).Scan(&rows).Error; err != nil {
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
	s       *Server
	chain   string
	node    *ledgerNode
	labels  map[string]AddressLabel
	mu      *sync.Mutex
	chans   map[string]string
	private bool
	archive *flowArchive
}

func (l *dbFlowLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	return l.holdingsAt(addr, day)
}

func (l *dbFlowLoader) holdingsAt(addr string, day time.Time) (flowHoldings, bool, error) {
	if l.archive != nil && l.chain == apiMainnet {
		if h, ok := l.archive.cachedHoldings(addr, day); ok {
			if l.private && h.Source.Kind != "archive_exact" {
				return h, false, fmt.Errorf("commission archive holding provenance missing")
			}
			return h, true, nil
		}
	}
	h, source, err := l.s.flowHoldingsAt(l.chain, addr, day, l.private)
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
	return l.s.flowEvents(l.chain, addr, start, end)
}

// flowEvents is addr's native coin movements in [start,end), as the report
// sees them: classified events, block events and fees.
func (s *Server) flowEvents(chain, addr string, start, end time.Time) ([]flowEvent, error) {
	input, err := s.readReportInput(chain, []string{addr}, start, end, false)
	if err != nil {
		return nil, err
	}
	rows := input.Wallets[0]
	credited, err := s.flowReturnCredits(addr, rows.Events)
	if err != nil {
		return nil, err
	}
	out := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees))
	for _, e := range rows.Events {
		if flowSingleHopNative(e, s.native.Denom) && !credited[flowCredit{e.MessageID, e.Amount + s.native.Denom}] {
			continue
		}
		if fe, ok := flowEventFromRow(e, addr, s.native.Denom); ok {
			out = append(out, fe)
		}
	}
	for _, f := range rows.Fees {
		if f.Denom != s.native.Denom {
			continue
		}
		v, err := decimal.NewFromString(f.Amount)
		if err != nil {
			continue
		}
		out = append(out, flowEvent{Key: flowKey{Time: f.Ts.UTC(), Height: 0}, Category: apiFee, Dir: apiOut, Amount: v, From: addr})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.before(out[j].Key) })
	return out, nil
}

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
	out := map[flowCredit]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		MessageID uint
		Value     string
	}
	for start := 0; start < len(ids); start += 5000 {
		chunk := ids[start:min(start+5000, len(ids))]
		var part []struct {
			MessageID uint
			Value     string
		}
		if err := s.db.Raw(`
SELECT me.message_id, a2.value
FROM message_events me
JOIN message_event_types mt ON mt.id = me.message_event_type_id AND mt.type = 'coin_received'
JOIN message_event_attributes a1 ON a1.message_event_id = me.id
JOIN message_event_attribute_keys k1 ON k1.id = a1.message_event_attribute_key_id AND k1.key = 'receiver'
JOIN message_event_attributes a2 ON a2.message_event_id = me.id
JOIN message_event_attribute_keys k2 ON k2.id = a2.message_event_attribute_key_id AND k2.key = 'amount'
WHERE me.message_id IN ? AND a1.value = ?`, chunk, addr).Scan(&part).Error; err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	for _, r := range rows {
		for _, coin := range strings.Split(r.Value, ",") {
			out[flowCredit{r.MessageID, strings.TrimSpace(coin)}] = true
		}
	}
	return out, nil
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
	if e.RewardTrigger == flowAuctionBid || e.RewardTrigger == flowAuctionRefund || (e.Category == apiNFTSale && flowEscrowedBid(e.Detail)) {
		fe.Lot = flowBidLot(e)
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

func (l *dbFlowLoader) stop(addr string) (flowStop, error) {
	l.mu.Lock()
	lab, ok := l.labels[addr]
	l.mu.Unlock()
	if ok && lab.Kind != labelValidatorOperator {
		switch {
		case lab.Kind == labelModule && lab.Name == "distribution":
			return flowStop{Class: flowCommunityPool}, nil
		case lab.Kind == labelModule:
			return flowStop{Class: flowModule, Detail: lab.Name}, nil
		case lab.Kind == labelIBCEscrow:
			return flowStop{Class: flowIBCOut, Detail: lab.Name}, nil
		case lab.Kind == labelExchangeLike:
			return flowStop{Class: flowExchangeLike, Detail: lab.Source}, nil
		default:
			return flowStop{Class: flowExchange, Detail: lab.Name}, nil
		}
	}
	// A 32-byte account is a contract or an interchain account.
	if len(addr) > len("cosmos1")+38+1 {
		return flowStop{Class: flowContract}, nil
	}
	n, err := l.s.memoDepositSenders(addr)
	if err != nil {
		return flowStop{}, err
	}
	if n >= flowMemoMinSenders {
		lab := AddressLabel{
			Chain: l.chain, Address: addr, Name: "deposit-style address", Kind: labelExchangeLike, Source: flowMemoHeuristic,
			Evidence: fmt.Sprintf("%d distinct senders with deposit-style memos in a sample of %d receipts", n, flowMemoSample), CreatedAt: nowUTC(),
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
	rows, err := l.s.walletRows(addr, ledgerHeavyRows)
	if err != nil {
		return flowStop{}, err
	}
	if rows > ledgerHeavyRows {
		return flowStop{Class: flowBusyWallet}, nil
	}
	return flowStop{}, nil
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
	}
	loader = &dbFlowLoader{s: reader, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: len(options) == 1}
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
	if failed > 0 {
		return fmt.Errorf("%d of %d validators failed", failed, len(validators))
	}
	return nil
}

const (
	flowStatusOK         = "ok"
	flowStatusReconciled = "reconciled"
	flowStatusDrift      = "drift"
	flowStatusError      = "error"
)

// flowDriftTolerance is how far, in base units, a traced wallet's simulated
// holdings may be from the chain's and still count as reconciled. Unclaimed
// rewards are not in a rebuilt balance, and slashing is not yet an event.
var flowDriftTolerance = decimal.NewFromInt(1000000)

func (s *Server) traceCommissionSaved(chain string, loader *dbFlowLoader, validator string, from, asOf time.Time, save func(CommissionFlowRun, []CommissionFlow, CommissionFlowEvidence) error) (CommissionFlowRun, error) {
	run := CommissionFlowRun{Chain: chain, Validator: validator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: nowUTC(), Overdraw: "{}", Commission: "{}", Untracked: "{}", DriftBase: "0"}
	tr := newFlowTrace(validator, from, asOf, loader)
	tr.strict = loader.private
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
		if saveErr := save(run, nil, evidence); saveErr != nil {
			return run, errors.Join(err, saveErr)
		}
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
	for _, w := range tr.order {
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
		actual, known, err := loader.holdingsAt(w.Addr, asOfDay)
		if err != nil || !known {
			if loader.private {
				return fail(fmt.Errorf("commission closing requires exact archive state"))
			}
			continue
		}
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
	if loader.archive != nil {
		evidence.Archive = loader.archive.evidence(tr.attempted)
	}
	return run, save(run, rows, evidence)
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
