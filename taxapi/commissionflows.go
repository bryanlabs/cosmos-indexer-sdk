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
func (s *Server) seedAddressLabels(chain string, validators []string) error {
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
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error
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
	s      *Server
	chain  string
	node   *ledgerNode
	labels map[string]AddressLabel
	mu     *sync.Mutex
	chans  map[string]string
}

func (l *dbFlowLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	return l.s.flowHoldingsAt(l.chain, addr, day)
}

func (s *Server) flowHoldingsAt(chain, addr string, day time.Time) (flowHoldings, bool, error) {
	h := flowHoldings{Liquid: decimal.Zero, Staked: map[string]decimal.Decimal{}, Unbonding: map[string]decimal.Decimal{}}
	bals, err := s.HistoricalBalances(chain, addr, []time.Time{day})
	if err != nil {
		return h, false, err
	}
	b := bals[0]
	if b.Status != balanceExact && b.Status != balanceApproximate {
		return h, false, nil
	}
	for _, line := range b.Lines {
		if line.Denom != s.native.Denom {
			continue
		}
		v, err := decimal.NewFromString(line.AmountBase)
		if err != nil {
			return h, false, err
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
	return h, true, nil
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
	out := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees))
	for _, e := range rows.Events {
		denom, amount := e.Denom, e.Amount
		if bonded, bondedAmount, isShare := lsmUnderlying(e.Category, e.Denom, e.Detail); isShare {
			denom, amount = bonded, bondedAmount
		}
		if denom != s.native.Denom {
			continue
		}
		v, err := decimal.NewFromString(amount)
		if err != nil {
			continue
		}
		fe := flowEvent{
			Key:      flowKey{Time: e.Timestamp.UTC(), Height: e.BlockHeight, Message: e.MessageID, Sub: e.SubIndex},
			Category: e.Category, Dir: eventDirection(e, addr), Amount: v,
			From: e.FromAddr, To: e.ToAddr, Validator: e.ValidatorAddress, Trigger: e.RewardTrigger, Detail: e.Detail,
		}
		switch e.Category {
		case flowCatRedelegate:
			fe.SrcValidator = detailValue(e.Detail, "source_validator")
		case flowCatTokenize, flowCatRedeem:
			if fe.Validator == "" {
				fe.Validator, _, _ = strings.Cut(e.Denom, "/")
			}
		}
		out = append(out, fe)
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
	if lab, ok := l.labels[addr]; ok && lab.Kind != labelValidatorOperator {
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
		if err := l.s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&lab).Error; err != nil {
			return flowStop{}, err
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
			_ = l.s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&IBCChannel{Chain: l.chain, Port: port, Channel: channel, ClientID: body.Identified.ClientID, ChainID: v, FetchedAt: nowUTC()}).Error
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
	err := s.db.Raw(`SELECT DISTINCT validator_address FROM taxable_events WHERE category = ? AND validator_address <> '' ORDER BY 1`, apiCommission).Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BuildCommissionFlows traces every validator's commission (or those named)
// from from through the end of yesterday and stores the result.
func (s *Server) BuildCommissionFlows(chain string, node *ledgerNode, from time.Time, only []string, workers int, logf func(string, ...any)) error {
	if err := CommissionFlowsMigrate(s.db); err != nil {
		return err
	}
	validators, err := s.commissionValidators(chain)
	if err != nil {
		return err
	}
	if err := s.seedAddressLabels(chain, validators); err != nil {
		return err
	}
	if len(only) > 0 {
		validators = only
	}
	var labels []AddressLabel
	if err := s.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
		return err
	}
	loader := &dbFlowLoader{s: s, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
	for _, l := range labels {
		loader.labels[l.Address] = l
	}
	asOf := flowDay(nowUTC())
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
				started := time.Now()
				run, err := s.traceCommission(chain, loader, v, from, asOf)
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

func (s *Server) traceCommission(chain string, loader *dbFlowLoader, validator string, from, asOf time.Time) (CommissionFlowRun, error) {
	run := CommissionFlowRun{Chain: chain, Validator: validator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: nowUTC(), Overdraw: "{}", Commission: "{}", Untracked: "{}", DriftBase: "0"}
	fail := func(err error) (CommissionFlowRun, error) {
		run.Status, run.Reason = flowStatusError, err.Error()
		if errors.Is(err, errAwaitingCorrection) {
			run.Reason = "a receiving wallet has rows awaiting a methodology correction"
		}
		_ = s.saveCommissionFlows(run, nil)
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
	tr := newFlowTrace(validator, from, asOf, loader)
	if err := tr.run(group); err != nil {
		return fail(err)
	}
	overdraw := map[string]string{}
	var total [flowRules]decimal.Decimal
	drift := decimal.Zero
	asOfDay := asOf.AddDate(0, 0, -1)
	for _, w := range tr.order {
		run.Wallets++
		if !w.Known {
			run.OpeningUnknown++
		}
		for r := 0; r < flowRules; r++ {
			total[r] = total[r].Add(w.Overdraw[r])
		}
		actual, known, err := s.flowHoldingsAt(chain, w.Addr, asOfDay)
		if err != nil || !known {
			continue
		}
		drift = drift.Add(w.simulated().Sub(actual.total()).Abs())
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
	return run, s.saveCommissionFlows(run, rows)
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
