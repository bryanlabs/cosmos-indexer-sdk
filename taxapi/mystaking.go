package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// My staking (INF-749): one holder's delegations seen per validator for a
// period. Rewards come from the same rows as /income, so per-validator
// rewards plus the other income buckets add up to the income report's total
// for the same wallets and period. Commission paid, yield, votes and
// commission changes are read from the staking tables and the index; nothing
// here writes.

const (
	myStakingReasonKey    = "reason"
	myStakingVoteAbstain  = "abstain"
	myStakingVoteVeto     = "no_with_veto"
	myStakingVoteYes      = "yes"
	myStakingMaxAddresses = 20
	// myStakingNoticeDays is how far back a commission change still raises
	// the dashboard notice.
	myStakingNoticeDays = 30
)

type myStakingAmount struct {
	Symbol string `json:"symbol"`
	Amount string `json:"amount"`
}

type myStakingTx struct {
	Date     string `json:"date"`
	Category string `json:"category"`
	TxHash   string `json:"tx_hash"`
	Symbol   string `json:"symbol"`
	Amount   string `json:"amount"`
	ValueUSD string `json:"value_usd"`
	// PriceMissing marks a row whose USD value is unknown (0 is not a value).
	PriceMissing bool `json:"price_missing,omitempty"`
	// CommissionPaid is this row's estimated commission, in the row's symbol.
	CommissionPaid string `json:"commission_paid,omitempty"`
	addr           int
}

type myStakingVote struct {
	ProposalID string `json:"proposal_id"`
	Option     string `json:"option"`
	Height     int64  `json:"height,omitempty"`
	TxHash     string `json:"tx_hash,omitempty"`
}

type myStakingCommissionChange struct {
	Date   string `json:"date"`
	Height int64  `json:"height"`
	TxHash string `json:"tx_hash"`
	// Kind is "create" or "edit". A genesis validator has no create
	// message, so its first edit has no From.
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

type myStakingYield struct {
	// APR is the validator's reward per staked ATOM after commission,
	// annualized over the days with data.
	APR       float64 `json:"apr"`
	Days      int     `json:"days"`
	FirstDay  string  `json:"first_day"`
	LastDay   string  `json:"last_day"`
	GrossAPR  float64 `json:"gross_apr"`
	RateAvg   float64 `json:"commission_rate_avg"`
	RateBasis string  `json:"rate_basis"`
}

type myStakingValidator struct {
	Operator          string                      `json:"operator_address"`
	Moniker           string                      `json:"moniker"`
	Rewards           []myStakingAmount           `json:"rewards"`
	RewardsUSD        string                      `json:"rewards_usd"`
	RewardRows        int                         `json:"reward_rows"`
	OwnCommission     []myStakingAmount           `json:"own_commission,omitempty"`
	OwnCommissionUSD  string                      `json:"own_commission_usd,omitempty"`
	IncomeUSD         string                      `json:"income_usd"`
	PriceMissingRows  int                         `json:"price_missing_rows"`
	CommissionPaid    []myStakingAmount           `json:"commission_paid"`
	CommissionPaidUSD string                      `json:"commission_paid_usd"`
	CommissionDayRate int                         `json:"commission_rows_day_rate"`
	CommissionNoData  int                         `json:"commission_rows_no_data"`
	Yield             *myStakingYield             `json:"yield"`
	Slashed           *string                     `json:"slashed"`
	Votes             []myStakingVote             `json:"votes"`
	VotesSummary      map[string]int              `json:"votes_summary"`
	CommissionChanges []myStakingCommissionChange `json:"commission_changes"`
	CommissionNotice  *myStakingCommissionChange  `json:"commission_notice"`
	CommissionRateNow string                      `json:"commission_rate_now,omitempty"`
	Transactions      []myStakingTx               `json:"transactions"`
}

type myStakingBucket struct {
	Kind         string            `json:"kind"`
	Amounts      []myStakingAmount `json:"amounts"`
	ValueUSD     string            `json:"value_usd"`
	Rows         int               `json:"rows"`
	PriceMissing int               `json:"price_missing_rows"`
	Transactions []myStakingTx     `json:"transactions"`
}

type myStakingProposal struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	VotingEnd string `json:"voting_end"`
	// YourVotes is each of the holder's addresses that voted, by position in
	// the request, with its last vote before voting ended.
	YourVotes map[string]string `json:"your_votes,omitempty"`
}

type myStakingResponse struct {
	Chain          string               `json:"chain"`
	Start          string               `json:"start"`
	End            string               `json:"end"`
	Addresses      int                  `json:"addresses"`
	Validators     []myStakingValidator `json:"validators"`
	Other          []myStakingBucket    `json:"other_income"`
	Reconciliation map[string]any       `json:"reconciliation"`
	Proposals      []myStakingProposal  `json:"proposals"`
	Slashing       map[string]string    `json:"slashing"`
	Method         map[string]string    `json:"method"`
	Unavailable    map[string]string    `json:"unavailable,omitempty"`
}

func (s *Server) handleMyStaking(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var addrs []string
	seen := map[string]bool{}
	for _, v := range q[apiAddress] {
		for _, a := range splitNonEmpty(v, ",") {
			a = strings.TrimSpace(a)
			if a != "" && !seen[a] {
				seen[a] = true
				addrs = append(addrs, a)
			}
		}
	}
	if len(addrs) == 0 {
		http.Error(w, apiAddressRequired, http.StatusBadRequest)
		return
	}
	if len(addrs) > myStakingMaxAddresses {
		http.Error(w, fmt.Sprintf("at most %d addresses", myStakingMaxAddresses), http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get(apiChain))
	start := dateParam(q.Get(apiStart), time.Time{})
	end := dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1))
	// The wallets' rows are all held at once, so they share one row cap.
	if err := s.checkReportRowCap(addrs, start, end); err != nil {
		writeTaxError(w, err)
		return
	}
	rowsByAddr := make([][]Row, len(addrs))
	for i, a := range addrs {
		rows, err := s.rowsForRequest(r, chain, []string{a}, start, end)
		if err != nil {
			writeTaxError(w, err)
			return
		}
		rowsByAddr[i] = rows
	}
	out := buildMyStaking(chain, start, end, rowsByAddr)
	out.Unavailable = map[string]string{}
	ops := make([]string, 0, len(out.Validators))
	for _, v := range out.Validators {
		ops = append(ops, v.Operator)
	}
	names := s.oracle.ValidatorMonikers()
	if indexed, err := s.indexedMonikers(chain); err == nil {
		for op, n := range indexed {
			if names[op] == "" {
				if names == nil {
					names = map[string]string{}
				}
				names[op] = n
			}
		}
	}
	for i := range out.Validators {
		if out.Validators[i].Moniker == "" {
			out.Validators[i].Moniker = names[out.Validators[i].Operator]
		}
	}
	if chain == apiMainnet && len(ops) > 0 {
		if days, err := readValidatorDaysFor(s.db, StakingChain, ops, out.firstDay(), out.lastDay()); err != nil {
			log.Printf("my-staking: validator days: %v", err)
			out.Unavailable["commission_paid"] = "staking tables unavailable"
		} else {
			applyCommissionEstimate(&out, days)
		}
		if ys, err := readValidatorYield(s.db, chain, ops, start, end); err != nil {
			log.Printf("my-staking: yield: %v", err)
			out.Unavailable["yield"] = "staking tables unavailable"
		} else {
			for i := range out.Validators {
				out.Validators[i].Yield = ys[out.Validators[i].Operator]
			}
		}
	} else if len(ops) > 0 {
		out.Unavailable["commission_paid"] = "staking tables cover the Cosmos Hub mainnet only"
		out.Unavailable["yield"] = "staking tables cover the Cosmos Hub mainnet only"
	}
	if changes, err := s.commissionChanges(chain); err != nil {
		log.Printf("my-staking: commission changes: %v", err)
		out.Unavailable["commission_changes"] = "commission history unavailable"
	} else {
		applyCommissionChanges(&out, changes, end)
	}
	if props, err := gaiaProposals(); err != nil {
		log.Printf("my-staking: proposals: %v", err)
		out.Unavailable["votes"] = "proposal list unavailable from the node"
	} else if err := s.applyVotes(&out, chain, addrs, props, start, end); err != nil {
		log.Printf("my-staking: votes: %v", err)
		out.Unavailable["votes"] = "vote history unavailable"
	}
	if len(out.Unavailable) == 0 {
		out.Unavailable = nil
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, out)
}

// buildMyStaking groups the income rows the way the income report counts
// them: every row isIncomeRow keeps, and nothing else.
func buildMyStaking(chain string, start, end time.Time, rowsByAddr [][]Row) myStakingResponse {
	out := myStakingResponse{
		Chain: chain, Addresses: len(rowsByAddr),
		Slashing: map[string]string{
			apiStatus:          "hidden",
			myStakingReasonKey: "slashing is not yet recorded in the staking ledger, so losses are not shown rather than shown as zero",
		},
		Method: map[string]string{
			"rewards":            "the income report's own reward rows (same wallets, period, tags and asset decisions), grouped by the validator each row names",
			"commission_paid":    "estimate: each reward row times c/(1-c), where c is the validator's commission over its total rewards on chain from the day after the previous reward row from that validator to the row's day; the first row in the period uses that day's rate",
			"yield":              "the validator's rewards after commission divided by its bonded tokens at the end of each day, summed over the days in the period and annualized; your own stake does not change it",
			"votes":              "each proposal whose voting ended in the period; the vote is the last one the validator's account cast before voting ended. Your own vote replaces your validator's for your stake",
			"commission_changes": "successful create and edit validator messages that set a commission rate",
		},
	}
	if !start.IsZero() {
		out.Start = start.Format(apiDateLayout)
	}
	out.End = end.Format(apiDateLayout)
	byOp := map[string]*myStakingValidator{}
	other := map[string]*myStakingBucket{}
	rewardsBySym := map[string]map[string]decimal.Decimal{}
	ownBySym := map[string]map[string]decimal.Decimal{}
	otherBySym := map[string]map[string]decimal.Decimal{}
	rewardsUSD, ownUSD, otherUSD := map[string]decimal.Decimal{}, map[string]decimal.Decimal{}, map[string]decimal.Decimal{}
	total := decimal.Zero
	totalRows, missing := 0, 0
	add := func(m map[string]map[string]decimal.Decimal, k, sym string, v decimal.Decimal) {
		if m[k] == nil {
			m[k] = map[string]decimal.Decimal{}
		}
		m[k][sym] = m[k][sym].Add(v)
	}
	for ai, rows := range rowsByAddr {
		for _, row := range rows {
			if row.Excluded || !isIncomeRow(row) {
				continue
			}
			total = total.Add(row.ValueUSD)
			totalRows++
			if row.PriceMissing {
				missing++
			}
			tx := myStakingTx{Date: row.Time.UTC().Format(apiDateLayout), Category: row.Category, TxHash: row.TxHash, Symbol: row.exportSymbol(), Amount: row.Amount.String(), ValueUSD: row.ValueUSD.String(), PriceMissing: row.PriceMissing, addr: ai}
			op := row.ValidatorAddress
			if op != "" && (row.Category == apiReward || row.Category == apiCommission) && row.UserTag == "" {
				v := byOp[op]
				if v == nil {
					v = &myStakingValidator{Operator: op, Moniker: row.ValidatorMoniker, VotesSummary: map[string]int{}}
					byOp[op] = v
				}
				if v.Moniker == "" {
					v.Moniker = row.ValidatorMoniker
				}
				if row.Category == apiCommission {
					add(ownBySym, op, row.exportSymbol(), row.Amount)
					ownUSD[op] = ownUSD[op].Add(row.ValueUSD)
				} else {
					add(rewardsBySym, op, row.exportSymbol(), row.Amount)
					rewardsUSD[op] = rewardsUSD[op].Add(row.ValueUSD)
					v.RewardRows++
				}
				if row.PriceMissing {
					v.PriceMissingRows++
				}
				v.Transactions = append(v.Transactions, tx)
				continue
			}
			kind := incomeType(row)
			if row.Category == apiReward || row.Category == apiCommission {
				kind = row.Category + "_unattributed"
			}
			b := other[kind]
			if b == nil {
				b = &myStakingBucket{Kind: kind}
				other[kind] = b
			}
			add(otherBySym, kind, row.exportSymbol(), row.Amount)
			otherUSD[kind] = otherUSD[kind].Add(row.ValueUSD)
			b.Rows++
			if row.PriceMissing {
				b.PriceMissing++
			}
			b.Transactions = append(b.Transactions, tx)
		}
	}
	valTotal, othTotal := decimal.Zero, decimal.Zero
	for op, v := range byOp {
		v.Rewards = amountsOf(rewardsBySym[op])
		v.RewardsUSD = rewardsUSD[op].String()
		if len(ownBySym[op]) > 0 {
			v.OwnCommission = amountsOf(ownBySym[op])
			v.OwnCommissionUSD = ownUSD[op].String()
		}
		inc := rewardsUSD[op].Add(ownUSD[op])
		v.IncomeUSD = inc.String()
		valTotal = valTotal.Add(inc)
		sortTxs(v.Transactions)
		v.CommissionPaid = []myStakingAmount{}
		v.CommissionPaidUSD = "0"
		v.Votes = []myStakingVote{}
		v.CommissionChanges = []myStakingCommissionChange{}
		out.Validators = append(out.Validators, *v)
	}
	sort.Slice(out.Validators, func(i, j int) bool {
		a, _ := decimal.NewFromString(out.Validators[i].IncomeUSD)
		b, _ := decimal.NewFromString(out.Validators[j].IncomeUSD)
		if !a.Equal(b) {
			return a.GreaterThan(b)
		}
		return out.Validators[i].Operator < out.Validators[j].Operator
	})
	for kind, b := range other {
		b.Amounts = amountsOf(otherBySym[kind])
		b.ValueUSD = otherUSD[kind].String()
		othTotal = othTotal.Add(otherUSD[kind])
		sortTxs(b.Transactions)
		out.Other = append(out.Other, *b)
	}
	sort.Slice(out.Other, func(i, j int) bool { return out.Other[i].Kind < out.Other[j].Kind })
	if out.Validators == nil {
		out.Validators = []myStakingValidator{}
	}
	if out.Other == nil {
		out.Other = []myStakingBucket{}
	}
	out.Reconciliation = map[string]any{
		"income_report_total_usd": total.String(),
		"validators_usd":          valTotal.String(),
		"other_income_usd":        othTotal.String(),
		"difference_usd":          total.Sub(valTotal).Sub(othTotal).String(),
		"income_rows":             totalRows,
		"price_missing_rows":      missing,
	}
	return out
}

func amountsOf(m map[string]decimal.Decimal) []myStakingAmount {
	out := make([]myStakingAmount, 0, len(m))
	for sym, v := range m {
		out = append(out, myStakingAmount{Symbol: sym, Amount: v.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func sortTxs(txs []myStakingTx) {
	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Date < txs[j].Date })
}

func (o *myStakingResponse) firstDay() string {
	first := ""
	for _, v := range o.Validators {
		for _, t := range v.Transactions {
			if first == "" || t.Date < first {
				first = t.Date
			}
		}
	}
	if first == "" {
		return o.End
	}
	return first
}

func (o *myStakingResponse) lastDay() string {
	last := ""
	for _, v := range o.Validators {
		for _, t := range v.Transactions {
			if t.Date > last {
				last = t.Date
			}
		}
	}
	if last == "" {
		return o.End
	}
	return last
}

// readValidatorDaysFor reads the per-validator day totals for the named
// validators only.
func readValidatorDaysFor(db *gorm.DB, chain string, ops []string, from, to string) (map[string]map[string][2]decimal.Decimal, error) {
	var rows []struct {
		Day        string
		Operator   string
		Rewards    string
		Commission string
	}
	if err := db.Raw(`SELECT day::text AS day, operator, rewards_uatom::text AS rewards, commission_uatom::text AS commission
FROM tax_staking_validator_days WHERE chain = ? AND operator IN ? AND day BETWEEN ?::date AND ?::date`, chain, ops, from, to).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string][2]decimal.Decimal{}
	for _, r := range rows {
		rw, err1 := decimal.NewFromString(r.Rewards)
		cm, err2 := decimal.NewFromString(r.Commission)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("validator day %s: bad amount", r.Day)
		}
		if out[r.Operator] == nil {
			out[r.Operator] = map[string][2]decimal.Decimal{}
		}
		out[r.Operator][r.Day] = [2]decimal.Decimal{rw, cm}
	}
	return out, nil
}

// applyCommissionEstimate prices the commission each reward row paid. The
// rate is the validator's commission over its total rewards for the days the
// claimed rewards built up in: from the day after the previous claim from
// the same validator to this claim's day.
func applyCommissionEstimate(out *myStakingResponse, days map[string]map[string][2]decimal.Decimal) {
	for i := range out.Validators {
		v := &out.Validators[i]
		vd := days[v.Operator]
		sums := map[string]decimal.Decimal{}
		usd := decimal.Zero
		prevByAddr := map[int]string{}
		for j := range v.Transactions {
			t := &v.Transactions[j]
			if t.Category != apiReward {
				continue
			}
			prev := prevByAddr[t.addr]
			rw, cm := decimal.Zero, decimal.Zero
			if prev != "" && prev < t.Date {
				for d, x := range vd {
					if d > prev && d <= t.Date {
						rw, cm = rw.Add(x[0]), cm.Add(x[1])
					}
				}
			} else {
				v.CommissionDayRate++
				x := vd[t.Date]
				rw, cm = x[0], x[1]
			}
			prevByAddr[t.addr] = t.Date
			if !rw.IsPositive() || !cm.LessThan(rw) {
				v.CommissionNoData++
				continue
			}
			factor := cm.Div(rw.Sub(cm))
			amt, _ := decimal.NewFromString(t.Amount)
			val, _ := decimal.NewFromString(t.ValueUSD)
			paid := amt.Mul(factor).Round(6)
			t.CommissionPaid = paid.String()
			sums[t.Symbol] = sums[t.Symbol].Add(paid)
			usd = usd.Add(val.Mul(factor))
		}
		v.CommissionPaid = amountsOf(sums)
		v.CommissionPaidUSD = usd.Round(2).String()
	}
}

// readValidatorYield sums each day's rewards after commission over the
// validator's bonded tokens at the end of that day.
func readValidatorYield(db *gorm.DB, chain string, ops []string, start, end time.Time) (map[string]*myStakingYield, error) {
	from := start
	if from.IsZero() {
		from = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	var rows []struct {
		Operator string
		Net      float64
		Gross    float64
		Rewards  float64
		Comm     float64
		Days     int
		FirstDay string
		LastDay  string
	}
	if err := db.Raw(`SELECT v.operator,
  sum((v.rewards_uatom - v.commission_uatom) / r.tokens)::float8 AS net,
  sum(v.rewards_uatom / r.tokens)::float8 AS gross,
  sum(v.rewards_uatom)::float8 AS rewards, sum(v.commission_uatom)::float8 AS comm,
  count(*) AS days, min(v.day)::text AS first_day, max(v.day)::text AS last_day
FROM tax_staking_validator_days v
JOIN tax_staking_days d ON d.chain = v.chain AND d.day = v.day
JOIN tax_validator_rates r ON r.chain = ? AND r.operator = v.operator AND r.height = d.last_height AND r.tokens > 0
WHERE v.chain = ? AND v.operator IN ? AND v.day >= ?::date AND v.day < ?::date
GROUP BY v.operator`, chain, StakingChain, ops, from.Format(apiDateLayout), end.Format(apiDateLayout)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]*myStakingYield{}
	for _, r := range rows {
		if r.Days == 0 {
			continue
		}
		y := &myStakingYield{APR: r.Net * 365 / float64(r.Days), GrossAPR: r.Gross * 365 / float64(r.Days), Days: r.Days, FirstDay: r.FirstDay, LastDay: r.LastDay, RateBasis: "commission over total rewards on chain in the period"}
		if r.Rewards > 0 {
			y.RateAvg = r.Comm / r.Rewards
		}
		out[r.Operator] = y
	}
	return out, nil
}

var commissionChangeCache = struct {
	sync.Mutex
	byChain map[string]commissionChangeEntry
}{byChain: map[string]commissionChangeEntry{}}

type commissionChangeEntry struct {
	at   time.Time
	rows map[string][]myStakingCommissionChange
}

// commissionChanges replays every successful create and edit validator
// message in block order and keeps the ones that set a rate.
func (s *Server) commissionChanges(chain string) (map[string][]myStakingCommissionChange, error) {
	commissionChangeCache.Lock()
	defer commissionChangeCache.Unlock()
	if e, ok := commissionChangeCache.byChain[chain]; ok && time.Since(e.at) < time.Hour {
		return e.rows, nil
	}
	var types []struct {
		ID          int64
		MessageType string
	}
	if err := s.db.Table("message_types").Select("id, message_type").Where("message_type IN ?", validatorDescriptionTypes).Scan(&types).Error; err != nil {
		return nil, err
	}
	out := map[string][]myStakingCommissionChange{}
	if len(types) == 0 {
		return out, nil
	}
	ids := make([]int64, len(types))
	urls := map[int64]string{}
	for i, t := range types {
		ids[i], urls[t.ID] = t.ID, t.MessageType
	}
	var msgs []struct {
		TypeID int64
		Raw    []byte
		Height int64
		Time   time.Time
		Hash   string
	}
	if err := s.db.Raw(`SELECT m.message_type_id AS type_id, m.message_bytes AS raw, b.height, b.time_stamp AS time, t.hash
FROM messages m
JOIN txes t ON t.id = m.tx_id
JOIN blocks b ON b.id = t.block_id
JOIN chains c ON c.id = b.chain_id
WHERE m.message_type_id IN ? AND c.chain_id IN ? AND t.code = 0 AND m.message_bytes IS NOT NULL
ORDER BY b.height, m.tx_id, m.message_index`, ids, chainIDs(chain)).Scan(&msgs).Error; err != nil {
		return nil, err
	}
	for _, m := range msgs {
		var op, rate, kind string
		switch urls[m.TypeID] {
		case validatorDescriptionTypes[0]:
			var msg stakingtypes.MsgCreateValidator
			if msg.Unmarshal(m.Raw) != nil {
				continue
			}
			op, rate, kind = msg.ValidatorAddress, msg.Commission.Rate.String(), "create"
		case validatorDescriptionTypes[1]:
			var msg stakingtypes.MsgEditValidator
			if msg.Unmarshal(m.Raw) != nil || msg.CommissionRate == nil || msg.CommissionRate.IsNil() {
				continue
			}
			op, rate, kind = msg.ValidatorAddress, msg.CommissionRate.String(), "edit"
		}
		if op == "" {
			continue
		}
		c := myStakingCommissionChange{Date: m.Time.UTC().Format(apiDateLayout), Height: m.Height, TxHash: m.Hash, Kind: kind, To: trimRate(rate)}
		if prev := out[op]; len(prev) > 0 {
			c.From = prev[len(prev)-1].To
			if c.From == c.To {
				continue
			}
		}
		out[op] = append(out[op], c)
	}
	commissionChangeCache.byChain[chain] = commissionChangeEntry{at: time.Now(), rows: out}
	return out, nil
}

func trimRate(s string) string {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return s
	}
	return d.String()
}

// applyCommissionChanges lists each validator's rate history up to the end
// of the period and raises the notice for a change in the last
// myStakingNoticeDays before the period ends.
func applyCommissionChanges(out *myStakingResponse, changes map[string][]myStakingCommissionChange, end time.Time) {
	endDay := end.Format(apiDateLayout)
	noticeFrom := end.AddDate(0, 0, -myStakingNoticeDays).Format(apiDateLayout)
	for i := range out.Validators {
		v := &out.Validators[i]
		for _, c := range changes[v.Operator] {
			if c.Date >= endDay {
				break
			}
			v.CommissionChanges = append(v.CommissionChanges, c)
		}
		if n := len(v.CommissionChanges); n > 0 {
			last := v.CommissionChanges[n-1]
			v.CommissionRateNow = last.To
			if last.Date >= noticeFrom && last.Kind == "edit" {
				c := last
				v.CommissionNotice = &c
			}
		}
	}
}

type gaiaProposal struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	VotingEnd string `json:"voting_end_time"`
}

var gaiaProposalCache = struct {
	sync.Mutex
	at   time.Time
	rows []gaiaProposal
}{}

// gaiaProposals reads the proposal list from the node, cached for an hour.
func gaiaProposals() ([]gaiaProposal, error) {
	gaiaProposalCache.Lock()
	defer gaiaProposalCache.Unlock()
	if gaiaProposalCache.rows != nil && time.Since(gaiaProposalCache.at) < time.Hour {
		return gaiaProposalCache.rows, nil
	}
	node, err := newLedgerNode()
	if err != nil {
		return nil, err
	}
	var all []gaiaProposal
	key := ""
	for i := 0; i < 20; i++ {
		q := map[string][]string{apiPaginationLimit: {"500"}}
		if key != "" {
			q["pagination.key"] = []string{key}
		}
		var body struct {
			Proposals  []gaiaProposal `json:"proposals"`
			Pagination struct {
				NextKey string `json:"next_key"`
			} `json:"pagination"`
		}
		if err := node.rest.GetJSON("/cosmos/gov/v1/proposals", q, &body); err != nil {
			return nil, err
		}
		all = append(all, body.Proposals...)
		if body.Pagination.NextKey == "" {
			break
		}
		key = body.Pagination.NextKey
	}
	if len(all) == 0 {
		return nil, errors.New("node returned no proposals")
	}
	gaiaProposalCache.rows, gaiaProposalCache.at = all, time.Now()
	return all, nil
}

type castVote struct {
	Account    string
	ProposalID string
	Option     string
	Height     int64
	Time       time.Time
	Hash       string
}

// applyVotes lists, for each proposal whose voting ended in the period, how
// each validator voted and how the holder's own addresses voted.
func (s *Server) applyVotes(out *myStakingResponse, chain string, addrs []string, props []gaiaProposal, start, end time.Time) error {
	var inPeriod []gaiaProposal
	ends := map[string]time.Time{}
	for _, p := range props {
		t, err := time.Parse(time.RFC3339Nano, p.VotingEnd)
		if err != nil || t.Year() < 2000 || p.Status == "PROPOSAL_STATUS_DEPOSIT_PERIOD" {
			continue
		}
		if (!start.IsZero() && t.Before(start)) || !t.Before(end) {
			continue
		}
		inPeriod = append(inPeriod, p)
		ends[p.ID] = t
	}
	sort.Slice(inPeriod, func(i, j int) bool { return ends[inPeriod[i].ID].Before(ends[inPeriod[j].ID]) })
	out.Proposals = []myStakingProposal{}
	if len(inPeriod) == 0 {
		return nil
	}
	accounts := map[string]string{}
	var lookup []string
	for _, v := range out.Validators {
		if a := operatorAccount(v.Operator); a != "" {
			accounts[v.Operator] = a
			lookup = append(lookup, a)
		}
	}
	lookup = append(lookup, addrs...)
	votes, err := s.votesBy(lookup)
	if err != nil {
		return err
	}
	last := lastVotes(votes, ends)
	for _, p := range inPeriod {
		mp := myStakingProposal{ID: p.ID, Title: p.Title, Status: strings.TrimPrefix(p.Status, "PROPOSAL_STATUS_"), VotingEnd: ends[p.ID].Format(time.RFC3339)}
		for i, a := range addrs {
			if v, ok := last[a][p.ID]; ok {
				if mp.YourVotes == nil {
					mp.YourVotes = map[string]string{}
				}
				mp.YourVotes[strconv.Itoa(i)] = v.Option
			}
		}
		out.Proposals = append(out.Proposals, mp)
	}
	for i := range out.Validators {
		v := &out.Validators[i]
		acct := accounts[v.Operator]
		for _, p := range inPeriod {
			cv, ok := last[acct][p.ID]
			if !ok || acct == "" {
				v.Votes = append(v.Votes, myStakingVote{ProposalID: p.ID, Option: "did_not_vote"})
				v.VotesSummary["did_not_vote"]++
				continue
			}
			v.Votes = append(v.Votes, myStakingVote{ProposalID: p.ID, Option: cv.Option, Height: cv.Height, TxHash: cv.Hash})
			v.VotesSummary[cv.Option]++
		}
	}
	return nil
}

// lastVotes keeps each account's last vote cast before the proposal's
// voting ended.
func lastVotes(votes []castVote, ends map[string]time.Time) map[string]map[string]castVote {
	out := map[string]map[string]castVote{}
	for _, v := range votes {
		e, ok := ends[v.ProposalID]
		if !ok || v.Time.After(e) {
			continue
		}
		if out[v.Account] == nil {
			out[v.Account] = map[string]castVote{}
		}
		if prev, ok := out[v.Account][v.ProposalID]; ok && prev.Height > v.Height {
			continue
		}
		out[v.Account][v.ProposalID] = v
	}
	return out
}

var voteKeyCache = struct {
	sync.Mutex
	ids map[string]int64
}{}

func (s *Server) voteKeyIDs() (map[string]int64, error) {
	voteKeyCache.Lock()
	defer voteKeyCache.Unlock()
	if voteKeyCache.ids != nil {
		return voteKeyCache.ids, nil
	}
	ids := map[string]int64{}
	var types []struct {
		ID   int64
		Type string
	}
	if err := s.db.Raw(`SELECT id, type FROM message_event_types WHERE type IN ('proposal_vote', 'message')`).Scan(&types).Error; err != nil {
		return nil, err
	}
	for _, t := range types {
		ids["type:"+t.Type] = t.ID
	}
	var keys []struct {
		ID  int64
		Key string
	}
	if err := s.db.Raw(`SELECT id, key FROM message_event_attribute_keys WHERE key IN ('option', 'proposal_id', 'voter', 'sender')`).Scan(&keys).Error; err != nil {
		return nil, err
	}
	for _, k := range keys {
		ids["key:"+k.Key] = k.ID
	}
	for _, need := range []string{"type:proposal_vote", "type:message", "key:option", "key:proposal_id", "key:voter", "key:sender"} {
		if ids[need] == 0 {
			return nil, fmt.Errorf("index has no %s", need)
		}
	}
	voteKeyCache.ids = ids
	return ids, nil
}

// votesBy reads every successful vote the accounts cast: a message with a
// proposal_vote event that either names the account as voter or was sent by
// it. When the event names a voter, it must be the account, so a vote cast
// for someone else through authz is not counted as the sender's.
//
// It runs as two reads. The planner estimates the event join far too high
// and scans the attribute table, so the message ids are read first and the
// second read is told to use the indexes.
func (s *Server) votesBy(accounts []string) ([]castVote, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	ids, err := s.voteKeyIDs()
	if err != nil {
		return nil, err
	}
	var hits []struct {
		MessageID int64
		Account   string
	}
	if err := s.db.Raw(`SELECT DISTINCT e.message_id, left(a.value, 100) AS account
FROM message_event_attributes a
JOIN message_events e ON e.id = a.message_event_id AND e.message_event_type_id IN ?
WHERE left(a.value, 100) IN ? AND a.message_event_attribute_key_id IN ?`,
		[]int64{ids["type:proposal_vote"], ids["type:message"]}, accounts, []int64{ids["key:voter"], ids["key:sender"]}).Scan(&hits).Error; err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	byMsg := map[int64][]string{}
	msgIDs := make([]int64, 0, len(hits))
	for _, h := range hits {
		if _, ok := byMsg[h.MessageID]; !ok {
			msgIDs = append(msgIDs, h.MessageID)
		}
		byMsg[h.MessageID] = append(byMsg[h.MessageID], h.Account)
	}
	var attrs []struct {
		MessageID int64
		EventID   int64
		KeyID     int64
		Value     string
		Height    int64
		Time      time.Time
		Hash      string
	}
	const chunk = 5000
	for i := 0; i < len(msgIDs); i += chunk {
		part := msgIDs[i:min(i+chunk, len(msgIDs))]
		var got []struct {
			MessageID int64
			EventID   int64
			KeyID     int64
			Value     string
			Height    int64
			Time      time.Time
			Hash      string
		}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL enable_seqscan = off").Error; err != nil {
				return err
			}
			if err := tx.Exec("SET LOCAL enable_hashjoin = off").Error; err != nil {
				return err
			}
			return tx.Raw(`SELECT pv.message_id, pv.id AS event_id, va.message_event_attribute_key_id AS key_id, left(va.value, 4000) AS value, b.height, b.time_stamp AS time, t.hash
FROM message_events pv
JOIN message_event_attributes va ON va.message_event_id = pv.id AND va.message_event_attribute_key_id IN ?
JOIN messages m ON m.id = pv.message_id
JOIN txes t ON t.id = m.tx_id AND t.code = 0
JOIN blocks b ON b.id = t.block_id
WHERE pv.message_id IN ? AND pv.message_event_type_id = ?`,
				[]int64{ids["key:option"], ids["key:proposal_id"], ids["key:voter"]}, part, ids["type:proposal_vote"]).Scan(&got).Error
		})
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, got...)
	}
	type ev struct {
		msg         int64
		option, pid string
		voter       string
		height      int64
		at          time.Time
		hash        string
	}
	events := map[int64]*ev{}
	var order []int64
	for _, a := range attrs {
		e := events[a.EventID]
		if e == nil {
			e = &ev{msg: a.MessageID, height: a.Height, at: a.Time, hash: a.Hash}
			events[a.EventID] = e
			order = append(order, a.EventID)
		}
		switch a.KeyID {
		case ids["key:option"]:
			e.option = a.Value
		case ids["key:proposal_id"]:
			e.pid = a.Value
		case ids["key:voter"]:
			e.voter = a.Value
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var out []castVote
	for _, id := range order {
		e := events[id]
		if e.pid == "" {
			continue
		}
		for _, acct := range byMsg[e.msg] {
			if e.voter != "" && e.voter != acct {
				continue
			}
			out = append(out, castVote{Account: acct, ProposalID: e.pid, Option: voteOptionLabel(e.option), Height: e.height, Time: e.at.UTC(), Hash: e.hash})
		}
	}
	return out, nil
}

var voteOptionNames = map[string]string{
	"1": myStakingVoteYes, "2": myStakingVoteAbstain, "3": "no", "4": myStakingVoteVeto,
	"VOTE_OPTION_YES": myStakingVoteYes, "VOTE_OPTION_ABSTAIN": myStakingVoteAbstain, "VOTE_OPTION_NO": "no", "VOTE_OPTION_NO_WITH_VETO": myStakingVoteVeto,
	"Yes": myStakingVoteYes, "Abstain": myStakingVoteAbstain, "No": "no", "NoWithVeto": myStakingVoteVeto,
}

// voteOptionLabel reads the proposal_vote option attribute in each format
// the Hub has written: a plain option name, "option:X weight:..." lines, a
// JSON object, or a JSON array of weighted options. A split vote is
// "weighted".
func voteOptionLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	var opts []string
	switch {
	case strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{"):
		var list []struct {
			Option json.RawMessage `json:"option"`
		}
		if strings.HasPrefix(raw, "{") {
			raw = "[" + raw + "]"
		}
		if json.Unmarshal([]byte(raw), &list) != nil {
			return apiUnknown
		}
		for _, o := range list {
			opts = append(opts, strings.Trim(string(o.Option), `"`))
		}
	case strings.Contains(raw, "option:"):
		for _, line := range strings.Split(raw, "\n") {
			f := strings.Fields(line)
			if len(f) > 0 {
				opts = append(opts, strings.TrimPrefix(f[0], "option:"))
			}
		}
	case raw != "":
		opts = []string{raw}
	}
	switch len(opts) {
	case 0:
		return apiUnknown
	case 1:
		if n, ok := voteOptionNames[opts[0]]; ok {
			return n
		}
		return apiUnknown
	}
	return "weighted"
}
