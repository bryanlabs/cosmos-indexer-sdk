package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
)

// FlowClassNotes explains each fate class on the page and in the API.
var FlowClassNotes = map[string]string{ //nolint:gosec // class descriptions, not credentials
	flowHeld:             "Still in a wallet the commission was paid to, unstaked.",
	flowRestakedOwn:      "Delegated back to this validator by a receiving wallet and still bonded.",
	flowRestakedOther:    "Delegated to another validator by a receiving wallet and still bonded.",
	flowUnbonding:        "In the unbonding queue of a receiving wallet at the as-of date.",
	flowLocked:           "Locked in a protocol or a governance deposit by a receiving wallet.",
	flowHeldDownstream:   "Sent on to another wallet (up to three hops) and still there, unstaked.",
	flowStakedDownstream: "Sent on to another wallet (up to three hops) that delegated it.",
	flowUnbondDownstream: "Sent on to another wallet (up to three hops) and unbonding there.",
	flowLockedDownstream: "Sent on to another wallet (up to three hops) and locked in a protocol there.",
	flowExchange:         "Sent to an address a named source labels as an exchange.",
	flowExchangeLike:     "Sent to an address that receives deposit-style memos from many different senders, which is how exchange deposit addresses look. Inferred, not confirmed.",
	flowIBCOut:           "Sent over IBC; the detail names the chain on the other end. Marked (swap) when the memo asked Skip Go to swap there.",
	flowFees:             "Paid as transaction fees.",
	flowSwapped:          "Swapped for another asset on the Hub.",
	flowSpent:            "Paid to a contract or for an NFT on the Hub.",
	flowTokenized:        "Turned into liquid staking share tokens, which the trace does not follow.",
	flowBurned:           "Burned with a governance deposit.",
	flowCommunityPool:    "Given to the community pool.",
	flowModule:           "Sent to another module account.",
	flowContract:         "Sent to a contract or an interchain account, which the trace does not follow.",
	flowBeyondHops:       "Moved more than three wallets away from the receiving wallets.",
	flowBusyWallet:       "Sent to a wallet with too much activity to trace (an exchange, a bridge or a service, most likely).",
	flowUnresolved:       "The trace could not read the receiving wallet's history.",
}

// FlowMethod is the method note served with every result.
const FlowMethod = "Commission is followed from each withdrawal to the wallet that received it, then through every transfer, delegation, unbonding and IBC send of that wallet, and up to three wallets further. " +
	"Each receiving wallet starts from its rebuilt holdings on the day before tracing starts, so coins it already held count as other coins. " +
	"Mixed coins need a rule: first in first out spends a wallet's oldest coins first, pro-rata spends every origin in proportion. Both are stored; neither is the truth, and the gap between them is the uncertainty. " +
	"Selling commission is normal: it is taxable income when withdrawn and pays for servers, staff and that tax. The figures describe on-chain movements, not intent, and are never ranked. " +
	"Exchange labels come only from the label table, each with its source; addresses inferred from deposit-style memos are reported separately as exchange_like. " +
	"Amounts are in ATOM. USD is the amount times that month's average ATOM price on the days commission was withdrawn, so it is the value when earned, not the sale price. " +
	"Commission paid in other denoms is listed as untracked."

type flowMonthPrice struct {
	at     time.Time
	prices map[string]decimal.Decimal
}

var (
	flowPriceMu    sync.Mutex
	flowPriceCache = map[string]flowMonthPrice{}
)

const flowPriceTTL = time.Hour

// commissionMonthPrices is each month's commission-weighted ATOM price, for
// one validator or (empty) every validator.
func (s *Server) commissionMonthPrices(chain, validator string) (map[string]decimal.Decimal, error) {
	key := chain + "|" + validator
	flowPriceMu.Lock()
	if c, ok := flowPriceCache[key]; ok && time.Since(c.at) < flowPriceTTL {
		flowPriceMu.Unlock()
		return c.prices, nil
	}
	flowPriceMu.Unlock()
	var days []struct {
		Day    time.Time
		Amount string
	}
	q := s.db.Table(apiTaxableEvents).
		Select("date_trunc('day', timestamp AT TIME ZONE 'UTC') AS day, SUM(amount::numeric)::text AS amount").
		Where("category = ? AND denom = ?", apiCommission, s.native.Denom)
	if validator != "" {
		q = q.Where("validator_address = ?", validator)
	}
	if err := q.Group("day").Order("day").Scan(&days).Error; err != nil {
		return nil, err
	}
	weighted, weights := map[string]decimal.Decimal{}, map[string]decimal.Decimal{}
	for _, d := range days {
		a, err := decimal.NewFromString(d.Amount)
		if err != nil {
			continue
		}
		p, ok := s.oracle.PriceAt(chain, s.native.Denom, d.Day.Format(apiDateLayout))
		if !ok {
			continue
		}
		m := d.Day.Format(apiMonthLayout)
		weighted[m] = weighted[m].Add(a.Mul(decimal.NewFromFloat(p)))
		weights[m] = weights[m].Add(a)
	}
	out := map[string]decimal.Decimal{}
	for m, w := range weights {
		if w.IsPositive() {
			out[m] = weighted[m].Div(w)
		}
	}
	flowPriceMu.Lock()
	flowPriceCache[key] = flowMonthPrice{at: time.Now(), prices: out}
	flowPriceMu.Unlock()
	return out, nil
}

// FlowLine is one class of a month's commission.
type FlowLine struct {
	Class        string           `json:"class"`
	Detail       string           `json:"detail,omitempty"`
	ViaUnbonding bool             `json:"via_unbonding,omitempty"`
	Amount       decimal.Decimal  `json:"amount"`
	USD          *decimal.Decimal `json:"usd_at_withdrawal"`
	Share        decimal.Decimal  `json:"share"`
}

// FlowMonth is one month of withdrawn commission and where it went.
type FlowMonth struct {
	Month     string            `json:"month"`
	Withdrawn decimal.Decimal   `json:"withdrawn"`
	PriceUSD  *decimal.Decimal  `json:"price_usd"`
	USD       *decimal.Decimal  `json:"withdrawn_usd"`
	Untracked map[string]string `json:"untracked,omitempty"`
	Flows     []FlowLine        `json:"flows"`
}

func (s *Server) displayAmount(base decimal.Decimal) decimal.Decimal {
	return base.Shift(-int32(s.native.Decimals)) //nolint:gosec // asset decimals are single digits
}

func flowRuleParam(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), flowRuleNames[flowRuleProRata]) || strings.EqualFold(strings.TrimSpace(v), "prorata") {
		return flowRuleNames[flowRuleProRata]
	}
	return flowRuleNames[flowRuleFIFO]
}

func flowMonthParam(v string) string {
	v = strings.TrimSpace(v)
	if _, err := time.Parse(apiMonthLayout, v); err == nil {
		return v
	}
	if t, err := time.Parse(apiDateLayout, v); err == nil {
		return t.Format(apiMonthLayout)
	}
	return ""
}

func inFlowMonths(m, start, end string) bool {
	return (start == "" || m >= start) && (end == "" || m <= end)
}

// buildFlowMonths turns stored lines into months with shares of withdrawn.
// collapse drops the detail of every class but IBC and named exchanges.
func (s *Server) buildFlowMonths(flows []CommissionFlow, withdrawn map[string]decimal.Decimal, untracked map[string]map[string]string, prices map[string]decimal.Decimal, collapse bool) []FlowMonth {
	type lineKey struct {
		month, class, detail string
		via                  bool
	}
	sums := map[lineKey]decimal.Decimal{}
	for _, f := range flows {
		v, err := decimal.NewFromString(f.AmountBase)
		if err != nil {
			continue
		}
		k := lineKey{f.Month, f.Class, f.Detail, f.ViaUnbonding}
		if collapse {
			k.via = false
			if f.Class != flowIBCOut && f.Class != flowExchange {
				k.detail = ""
			}
		}
		sums[k] = sums[k].Add(v)
	}
	byMonth := map[string]*FlowMonth{}
	month := func(m string) *FlowMonth {
		if fm, ok := byMonth[m]; ok {
			return fm
		}
		fm := &FlowMonth{Month: m, Withdrawn: s.displayAmount(withdrawn[m]), Untracked: untracked[m], Flows: []FlowLine{}}
		if p, ok := prices[m]; ok {
			price := p.Round(6)
			usd := fm.Withdrawn.Mul(p).Round(2)
			fm.PriceUSD, fm.USD = &price, &usd
		}
		byMonth[m] = fm
		return fm
	}
	for m := range withdrawn {
		month(m)
	}
	for k, v := range sums {
		fm := month(k.month)
		line := FlowLine{Class: k.class, Detail: k.detail, ViaUnbonding: k.via, Amount: s.displayAmount(v), Share: decimal.Zero}
		if fm.PriceUSD != nil {
			usd := line.Amount.Mul(prices[k.month]).Round(2)
			line.USD = &usd
		}
		if w := withdrawn[k.month]; w.IsPositive() {
			line.Share = v.Div(w).Round(4)
		}
		fm.Flows = append(fm.Flows, line)
	}
	out := make([]FlowMonth, 0, len(byMonth))
	for _, fm := range byMonth {
		sort.Slice(fm.Flows, func(i, j int) bool {
			if !fm.Flows[i].Amount.Equal(fm.Flows[j].Amount) {
				return fm.Flows[i].Amount.GreaterThan(fm.Flows[j].Amount)
			}
			return fm.Flows[i].Class+fm.Flows[i].Detail < fm.Flows[j].Class+fm.Flows[j].Detail
		})
		out = append(out, *fm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month < out[j].Month })
	return out
}

// flowTotals sums months into one period line set.
func flowTotals(months []FlowMonth) FlowMonth {
	total := FlowMonth{Month: "total", Withdrawn: decimal.Zero, Flows: []FlowLine{}}
	usdKnown := true
	withdrawnUSD := decimal.Zero
	type lineKey struct {
		class, detail string
		via           bool
	}
	sums := map[lineKey]*FlowLine{}
	var order []lineKey
	for _, m := range months {
		total.Withdrawn = total.Withdrawn.Add(m.Withdrawn)
		if m.USD == nil {
			usdKnown = false
		} else {
			withdrawnUSD = withdrawnUSD.Add(*m.USD)
		}
		for _, l := range m.Flows {
			k := lineKey{l.Class, l.Detail, l.ViaUnbonding}
			cur, ok := sums[k]
			if !ok {
				zero := decimal.Zero
				cur = &FlowLine{Class: l.Class, Detail: l.Detail, ViaUnbonding: l.ViaUnbonding, Amount: decimal.Zero, USD: &zero}
				sums[k] = cur
				order = append(order, k)
			}
			cur.Amount = cur.Amount.Add(l.Amount)
			if l.USD != nil && cur.USD != nil {
				v := cur.USD.Add(*l.USD)
				cur.USD = &v
			} else {
				cur.USD = nil
			}
		}
	}
	if usdKnown && len(months) > 0 {
		total.USD = &withdrawnUSD
	}
	for _, k := range order {
		l := *sums[k]
		if total.Withdrawn.IsPositive() {
			l.Share = l.Amount.Div(total.Withdrawn).Round(4)
		}
		total.Flows = append(total.Flows, l)
	}
	sort.Slice(total.Flows, func(i, j int) bool { return total.Flows[i].Amount.GreaterThan(total.Flows[j].Amount) })
	return total
}

func decodeFlowAmounts(raw string) map[string]decimal.Decimal {
	var m map[string]string
	out := map[string]decimal.Decimal{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return out
	}
	for k, v := range m {
		if d, err := decimal.NewFromString(v); err == nil {
			out[k] = d
		}
	}
	return out
}

// ValidatorCommissionFlows is the per-validator response.
type ValidatorCommissionFlows struct {
	Chain          string               `json:"chain"`
	Validator      string               `json:"validator"`
	Moniker        string               `json:"moniker,omitempty"`
	Rule           string               `json:"rule"`
	Rules          []string             `json:"rules"`
	Status         string               `json:"status"`
	Reason         string               `json:"reason,omitempty"`
	From           string               `json:"from,omitempty"`
	AsOf           string               `json:"as_of,omitempty"`
	ComputedAt     *time.Time           `json:"computed_at,omitempty"`
	Reconciliation map[string]any       `json:"reconciliation"`
	Receivers      []CommissionReceiver `json:"receivers"`
	Months         []FlowMonth          `json:"months"`
	Total          FlowMonth            `json:"total"`
	Classes        map[string]string    `json:"classes"`
	Method         string               `json:"method"`
	Symbol         string               `json:"symbol"`
}

func (s *Server) handleValidatorCommissionFlows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	val := strings.TrimSpace(q.Get(apiValidator))
	if hrp, _, err := bech32.DecodeAndConvert(val); err != nil || hrp != flowValoperHRP {
		http.Error(w, "validator (cosmosvaloper address) required", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get(apiChain))
	rule := flowRuleParam(q.Get("rule"))
	start, end := flowMonthParam(q.Get(apiStart)), flowMonthParam(q.Get(apiEnd))
	var run CommissionFlowRun
	res := s.db.Where("chain = ? AND validator = ?", chain, val).Limit(1).Find(&run)
	if res.Error != nil {
		http.Error(w, res.Error.Error(), http.StatusInternalServerError)
		return
	}
	if res.RowsAffected == 0 {
		http.Error(w, "no commission trace for this validator yet; traces are rebuilt daily for every validator that has withdrawn commission", http.StatusNotFound)
		return
	}
	var flows []CommissionFlow
	if err := s.db.Where("chain = ? AND validator = ? AND rule = ?", chain, val, rule).Find(&flows).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	receivers, err := s.commissionReceivers(chain, val)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prices, err := s.commissionMonthPrices(chain, val)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	withdrawn := map[string]decimal.Decimal{}
	for m, v := range decodeFlowAmounts(run.Commission) {
		if inFlowMonths(m, start, end) {
			withdrawn[m] = v
		}
	}
	kept := flows[:0]
	for _, f := range flows {
		if inFlowMonths(f.Month, start, end) {
			kept = append(kept, f)
		}
	}
	var untracked map[string]map[string]string
	_ = json.Unmarshal([]byte(run.Untracked), &untracked)
	months := s.buildFlowMonths(kept, withdrawn, untracked, prices, false)
	overdraw := map[string]decimal.Decimal{}
	for k, v := range decodeFlowAmounts(run.Overdraw) {
		overdraw[k] = s.displayAmount(v)
	}
	drift, _ := decimal.NewFromString(run.DriftBase)
	computed := run.ComputedAt
	out := ValidatorCommissionFlows{
		Chain: chain, Validator: val, Moniker: s.oracle.ValidatorMonikers()[val], Rule: rule, Rules: flowRuleNames[:],
		Status: run.Status, Reason: run.Reason, From: run.FromDay.Format(apiDateLayout), AsOf: run.AsOf.Format(apiDateLayout), ComputedAt: &computed,
		Reconciliation: map[string]any{
			"receivers": run.Receivers, "wallets": run.Wallets, "opening_unknown": run.OpeningUnknown, "unresolved": run.Unresolved,
			"overdraw": overdraw, "drift": s.displayAmount(drift),
		},
		Receivers: receivers, Months: months, Total: flowTotals(months), Classes: FlowClassNotes, Method: FlowMethod, Symbol: s.native.Symbol,
	}
	if out.Receivers == nil {
		out.Receivers = []CommissionReceiver{}
	}
	for i := range out.Receivers {
		if b, err := decimal.NewFromString(out.Receivers[i].AmountBase); err == nil {
			out.Receivers[i].Amount = s.displayAmount(b).String()
		}
	}
	if strings.EqualFold(q.Get(apiFormat), "csv") {
		writeFlowCSV(w, "commission-flows-"+val+"-"+rule+".csv", months)
		return
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, out)
}

// FlowLeftOut is what the Hub totals leave out because a validator's rebuilt
// balances did not match the chain: how many validators and how much
// commission they withdrew in the period.
type FlowLeftOut struct {
	Validators int             `json:"validators"`
	Withdrawn  decimal.Decimal `json:"withdrawn"`
}

// flowHubLeftOut are the run statuses the Hub view leaves out of both its
// withdrawn and its flow totals.
var flowHubLeftOut = []string{flowStatusError, flowStatusDrift}

// flowExpectedTTL is how long the list of validators that ever withdrew
// commission is reused before it is read again in the background.
const flowExpectedTTL = 6 * time.Hour

type flowExpected struct {
	set     map[string]bool
	at      time.Time
	loading bool
}

// flowExpectedValidators returns the validators the daily trace covers on
// chain, and false until that list has been read once. Reading it takes tens
// of seconds, so it never runs on the request path: a missing or stale list
// is refreshed in the background and the last one is served meanwhile.
func (s *Server) flowExpectedValidators(chain string) (map[string]bool, bool) {
	s.flowExpMu.Lock()
	defer s.flowExpMu.Unlock()
	if s.flowExp == nil {
		s.flowExp = map[string]*flowExpected{}
	}
	e := s.flowExp[chain]
	if e == nil {
		e = &flowExpected{}
		s.flowExp[chain] = e
	}
	if !e.loading && (e.set == nil || time.Since(e.at) > flowExpectedTTL) {
		e.loading = true
		// A failed read leaves the list unknown and the next request retries.
		go func() { _ = s.refreshFlowExpected(chain) }()
	}
	return e.set, e.set != nil
}

func (s *Server) refreshFlowExpected(chain string) error {
	validators, err := s.commissionValidators(chain)
	s.flowExpMu.Lock()
	defer s.flowExpMu.Unlock()
	if s.flowExp == nil {
		s.flowExp = map[string]*flowExpected{}
	}
	e := s.flowExp[chain]
	if e == nil {
		e = &flowExpected{}
		s.flowExp[chain] = e
	}
	e.loading = false
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(validators))
	for _, v := range validators {
		set[v] = true
	}
	e.set, e.at = set, time.Now()
	return nil
}

// CommissionFlowsHub is the Hub-wide response: every traced validator's
// commission summed by month, with no per-validator breakdown.
type CommissionFlowsHub struct {
	Chain      string         `json:"chain"`
	Rule       string         `json:"rule"`
	Rules      []string       `json:"rules"`
	Validators int            `json:"validators"`
	Statuses   map[string]int `json:"statuses"`
	// Traced of Expected validators have a stored run. Expected is every
	// validator that ever withdrew commission, the set the daily trace
	// covers; 0 while that list is still being read. Months and Total stay
	// empty until Complete: a partial set is not a Hub figure.
	Traced       int               `json:"traced"`
	Expected     int               `json:"expected"`
	Complete     bool              `json:"complete"`
	Unreconciled FlowLeftOut       `json:"unreconciled"`
	AsOf         string            `json:"as_of,omitempty"`
	Months       []FlowMonth       `json:"months"`
	Total        FlowMonth         `json:"total"`
	Classes      map[string]string `json:"classes"`
	Method       string            `json:"method"`
	Symbol       string            `json:"symbol"`
}

func (s *Server) handleCommissionFlowsHub(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	chain := chainParam(q.Get(apiChain))
	rule := flowRuleParam(q.Get("rule"))
	start, end := flowMonthParam(q.Get(apiStart)), flowMonthParam(q.Get(apiEnd))
	var runs []CommissionFlowRun
	if err := s.db.Where("chain = ?", chain).Find(&runs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A failed run keeps its last good flows for its own page, and a run whose
	// rebuilt balances do not match the chain keeps its flows there too, but
	// the Hub leaves both out of both sides so shares stay whole.
	var flows []CommissionFlow
	if err := s.db.Table(CommissionFlow{}.TableName()).
		Select("month, class, detail, via_unbonding, SUM(amount_base)::text AS amount_base").
		Where("chain = ? AND rule = ?", chain, rule).
		Where("validator NOT IN (?)", s.db.Table(CommissionFlowRun{}.TableName()).Select(apiValidator).Where("chain = ? AND status IN ?", chain, flowHubLeftOut)).
		Group("month, class, detail, via_unbonding").Scan(&flows).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prices, err := s.commissionMonthPrices(chain, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := CommissionFlowsHub{Chain: chain, Rule: rule, Rules: flowRuleNames[:], Statuses: map[string]int{}, Classes: FlowClassNotes, Method: FlowMethod, Symbol: s.native.Symbol}
	expected, known := s.flowExpectedValidators(chain)
	withdrawn := map[string]decimal.Decimal{}
	unreconciled := decimal.Zero
	var asOf time.Time
	for _, run := range runs {
		out.Validators++
		out.Statuses[run.Status]++
		if expected[run.Validator] {
			out.Traced++
		}
		if run.AsOf.After(asOf) {
			asOf = run.AsOf
		}
		if run.Status == flowStatusError {
			continue
		}
		for m, v := range decodeFlowAmounts(run.Commission) {
			if !inFlowMonths(m, start, end) {
				continue
			}
			if run.Status == flowStatusDrift {
				unreconciled = unreconciled.Add(v)
				continue
			}
			withdrawn[m] = withdrawn[m].Add(v)
		}
		if run.Status == flowStatusDrift {
			out.Unreconciled.Validators++
		}
	}
	out.Unreconciled.Withdrawn = s.displayAmount(unreconciled)
	if !asOf.IsZero() {
		out.AsOf = asOf.Format(apiDateLayout)
	}
	out.Expected = len(expected)
	out.Complete = known && len(expected) > 0 && out.Traced == len(expected)
	if !out.Complete {
		flows, withdrawn = nil, map[string]decimal.Decimal{}
	}
	kept := flows[:0]
	for _, f := range flows {
		if inFlowMonths(f.Month, start, end) {
			kept = append(kept, f)
		}
	}
	out.Months = s.buildFlowMonths(kept, withdrawn, nil, prices, true)
	out.Total = flowTotals(out.Months)
	if strings.EqualFold(q.Get(apiFormat), "csv") {
		writeFlowCSV(w, "commission-flows-hub-"+rule+".csv", out.Months)
		return
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, out)
}

func writeFlowCSV(w http.ResponseWriter, name string, months []FlowMonth) {
	w.Header().Set(apiContentType, "text/csv")
	w.Header().Set(apiContentDisposition, `attachment; filename="`+name+`"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"month", "withdrawn_atom", "class", "detail", "via_unbonding", "amount_atom", "share", "usd_at_withdrawal"})
	for _, m := range months {
		for _, l := range m.Flows {
			usd := ""
			if l.USD != nil {
				usd = l.USD.StringFixed(2)
			}
			_ = cw.Write([]string{m.Month, m.Withdrawn.String(), l.Class, l.Detail, strconv.FormatBool(l.ViaUnbonding), l.Amount.String(), l.Share.String(), usd})
		}
	}
	cw.Flush()
}

// handleAddressLabels lists the labels the tracer stops at, with sources.
func (s *Server) handleAddressLabels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	chain := chainParam(q.Get(apiChain))
	var labels []AddressLabel
	db := s.db.Where("chain = ? AND kind IN ?", chain, []string{labelExchange, labelExchangeLike, labelModule})
	if err := db.Order("kind, name").Find(&labels).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type label struct {
		Address  string `json:"address"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Source   string `json:"source"`
		Evidence string `json:"evidence"`
	}
	out := make([]label, 0, len(labels))
	for _, l := range labels {
		out = append(out, label{l.Address, l.Name, l.Kind, l.Source, l.Evidence})
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, map[string]any{apiChain: chain, "labels": out, "note": "IBC escrow and validator operator accounts are also labelled and derived the same way; they are left out of this list."})
}
