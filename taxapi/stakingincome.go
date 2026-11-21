package taxapi

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// GET /staking/validator-income and GET /staking/validator-income/break-even:
// what the validator set earns in commission, month by month, and how many
// validators a given running cost and inflation rate would leave below
// break-even (INF-748 option 2). Built from the commission and rewards events
// every cosmoshub-4 block emits for every bonded validator, so validators that
// never withdrew are counted. The file is produced by the same extraction as
// the inflation view and embedded until the daily job writes it to Postgres.

//go:embed stakingdata/validator_income.json.gz
var validatorIncomeGz []byte

type incomeFile struct {
	Chain           string `json:"chain"`
	AsOf            string `json:"as_of"`
	FirstDay        string `json:"first_day"`
	LastDay         string `json:"last_day"`
	FirstHeight     int64  `json:"first_height"`
	LastHeight      int64  `json:"last_height"`
	BlocksCounted   int64  `json:"blocks_counted"`
	BlocksExpected  int64  `json:"blocks_expected"`
	ExchangeRunRule string `json:"exchange_run_rule"`
	ExchangeRun     []int  `json:"exchange_run"`
	// Withdrawn holds, per day from first_day, the commission and rewards
	// withdrawn by successful transactions, in uatom.
	Withdrawn  [][2]int64 `json:"withdrawn"`
	Validators []struct {
		Operator string `json:"operator"`
		// Commission is the validator's commission credited per day from
		// FirstDay, in whole uatom. Each day is the change in the floor of the
		// running total, so any run of days sums to within 1 uatom of the exact
		// figure.
		FirstDay   string           `json:"first_day"`
		Commission []int64          `json:"commission"`
		Rewards    map[string]int64 `json:"rewards_monthly"`
	} `json:"validators"`
}

type incomeValidator struct {
	Operator     string
	First        int
	Commission   []int64
	Rewards      map[string]int64
	ExchangeRun  bool
	commissionCS []int64
}

type incomeData struct {
	File       incomeFile
	Days       []string
	DayIndex   map[string]int
	Validators []incomeValidator
}

var (
	incomeOnce sync.Once
	incomeSet  *incomeData
	incomeErr  error
)

func loadIncome() (*incomeData, error) {
	incomeOnce.Do(func() { incomeSet, incomeErr = parseIncome(validatorIncomeGz) })
	return incomeSet, incomeErr
}

func parseIncome(gz []byte) (*incomeData, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var f incomeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	first, err := time.Parse(time.DateOnly, f.FirstDay)
	if err != nil {
		return nil, err
	}
	last, err := time.Parse(time.DateOnly, f.LastDay)
	if err != nil {
		return nil, err
	}
	d := &incomeData{File: f, DayIndex: map[string]int{}}
	for t := first; !t.After(last); t = t.AddDate(0, 0, 1) {
		d.DayIndex[t.Format(time.DateOnly)] = len(d.Days)
		d.Days = append(d.Days, t.Format(time.DateOnly))
	}
	if len(f.Withdrawn) != len(d.Days) {
		return nil, fmt.Errorf("income file has %d withdrawal days for %d days", len(f.Withdrawn), len(d.Days))
	}
	ex := map[int]bool{}
	for _, i := range f.ExchangeRun {
		if i < 0 || i >= len(f.Validators) {
			return nil, fmt.Errorf("exchange-run index %d out of range", i)
		}
		ex[i] = true
	}
	for i, v := range f.Validators {
		at, ok := d.DayIndex[v.FirstDay]
		if !ok {
			return nil, fmt.Errorf("validator %d starts on %s, outside the file", i, v.FirstDay)
		}
		if at+len(v.Commission) > len(d.Days) {
			return nil, fmt.Errorf("validator %d runs past the last day", i)
		}
		cs := make([]int64, len(v.Commission)+1)
		for j, c := range v.Commission {
			if c < 0 {
				return nil, fmt.Errorf("validator %d has negative commission on day %d", i, j)
			}
			cs[j+1] = cs[j] + c
		}
		d.Validators = append(d.Validators, incomeValidator{Operator: v.Operator, First: at, Commission: v.Commission,
			Rewards: v.Rewards, ExchangeRun: ex[i], commissionCS: cs})
	}
	return d, nil
}

// commissionIn is the validator's commission over days [lo, hi) in uatom.
func (v incomeValidator) commissionIn(lo, hi int) int64 {
	lo, hi = lo-v.First, hi-v.First
	if lo < 0 {
		lo = 0
	}
	if hi > len(v.Commission) {
		hi = len(v.Commission)
	}
	if hi <= lo {
		return 0
	}
	return v.commissionCS[hi] - v.commissionCS[lo]
}

// dayPrices returns the ATOM price for each day the oracle knows, fetched
// concurrently so a cold cache costs one round of parallel lookups.
func (s *Server) dayPrices(chain string, days []string) map[string]float64 {
	out := make(map[string]float64, len(days))
	if s.oracle == nil {
		return out
	}
	var mu sync.Mutex
	work := make(chan string, len(days))
	for _, d := range days {
		work <- d
	}
	close(work)
	var wg sync.WaitGroup
	for i := 0; i < priceWarmConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				if p, ok := s.oracle.PriceAt(chain, s.native.Denom, d); ok && p > 0 {
					mu.Lock()
					out[d] = p
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return out
}

type incomeSpread struct {
	P10    float64 `json:"p10"`
	P25    float64 `json:"p25"`
	Median float64 `json:"median"`
	P75    float64 `json:"p75"`
	P90    float64 `json:"p90"`
}

type incomeGroup struct {
	Group           string   `json:"group"`
	Validators      int      `json:"validators"`
	StakeShare      float64  `json:"stake_share"`
	CommissionAtom  string   `json:"commission_atom"`
	CommissionUSD   *float64 `json:"commission_usd"`
	CommissionShare float64  `json:"commission_share"`
	MedianAtom      string   `json:"median_atom"`
	MedianUSD       *float64 `json:"median_usd"`
}

type incomeRow struct {
	Period                  string        `json:"period"`
	StartDate               string        `json:"start_date"`
	EndDate                 string        `json:"end_date"`
	Days                    int           `json:"days"`
	EarnedCommissionAtom    string        `json:"earned_commission_atom"`
	EarnedCommissionUSD     *float64      `json:"earned_commission_usd"`
	WithdrawnCommissionAtom string        `json:"withdrawn_commission_atom"`
	WithdrawnCommissionUSD  *float64      `json:"withdrawn_commission_usd"`
	ToDelegatorsAtom        string        `json:"to_delegators_atom"`
	WithdrawnRewardsAtom    string        `json:"withdrawn_delegator_rewards_atom"`
	Validators              int           `json:"validators"`
	ZeroCommission          int           `json:"validators_at_zero_commission"`
	PerValidatorAtom        incomeSpread  `json:"per_validator_atom"`
	PerValidatorUSD         *incomeSpread `json:"per_validator_usd"`
	ByStake                 []incomeGroup `json:"by_stake"`
	ExchangeRun             *incomeGroup  `json:"exchange_run"`
	PriceDaysMissing        int           `json:"price_days_missing"`
}

// stakeBands groups validators by their rank in the period's rewards, which
// the chain pays in proportion to voting power.
var stakeBands = []struct {
	name   string
	lo, hi int
}{{"ranks 1 to 10 by stake", 1, 10}, {"ranks 11 to 50", 11, 50}, {"ranks 51 to 100", 51, 100}, {"ranks 101 and below", 101, math.MaxInt}}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

func spreadOf(vals []float64, scale float64) incomeSpread {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	r := func(q float64) float64 { return math.Round(quantile(s, q)*scale) / scale }
	return incomeSpread{P10: r(0.10), P25: r(0.25), Median: r(0.5), P75: r(0.75), P90: r(0.90)}
}

func usdPtr(v float64) *float64 {
	v = math.Round(v*100) / 100
	return &v
}

func uatomString(v int64) string { return atomString(big.NewInt(v)) }

func median(vals []float64) float64 {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	return quantile(s, 0.5)
}

type incomePeriod struct {
	key    string
	lo, hi int
}

func incomePeriods(days []string, view string) []incomePeriod {
	var out []incomePeriod
	n := 7
	if view == "yearly" {
		n = 4
	}
	for i := 0; i < len(days); {
		j := i
		for j < len(days) && days[j][:n] == days[i][:n] {
			j++
		}
		out = append(out, incomePeriod{key: days[i][:n], lo: i, hi: j})
		i = j
	}
	return out
}

// incomeRowFor builds one period. infl holds the inflation file's days by date
// for the Hub-wide totals, which are exact to the uatom.
func incomeRowFor(d *incomeData, p incomePeriod, infl map[string]inflationDay, prices map[string]float64, havePrices bool) incomeRow {
	row := incomeRow{Period: p.key, StartDate: d.Days[p.lo], EndDate: d.Days[p.hi-1], Days: p.hi - p.lo}
	hubC, hubR := new(big.Int), new(big.Int)
	var earnedUSD, wcUSD float64
	var wc, wr int64
	for i := p.lo; i < p.hi; i++ {
		day := d.Days[i]
		w := d.File.Withdrawn[i]
		wc += w[0]
		wr += w[1]
		pr, ok := prices[day]
		id, inInfl := infl[day]
		if inInfl {
			hubC.Add(hubC, id.CommissionEvents)
			hubR.Add(hubR, id.RewardsEvents)
		}
		if !ok {
			row.PriceDaysMissing++
			continue
		}
		if inInfl {
			c, _ := new(big.Float).SetInt(id.CommissionEvents).Float64()
			earnedUSD += c / 1e6 * pr
		}
		wcUSD += float64(w[0]) / 1e6 * pr
	}
	row.EarnedCommissionAtom = atomString(hubC)
	row.ToDelegatorsAtom = atomString(new(big.Int).Sub(hubR, hubC))
	row.WithdrawnCommissionAtom = uatomString(wc)
	row.WithdrawnRewardsAtom = uatomString(wr)
	usdOK := havePrices && row.PriceDaysMissing == 0
	if usdOK {
		row.EarnedCommissionUSD = usdPtr(earnedUSD)
		row.WithdrawnCommissionUSD = usdPtr(wcUSD)
	}

	months := map[string]bool{}
	for i := p.lo; i < p.hi; i++ {
		months[d.Days[i][:7]] = true
	}
	type vRow struct {
		rewards    int64
		commission int64
		usd        float64
		exchange   bool
	}
	var set []vRow
	var totalRewards, totalCommission int64
	for _, v := range d.Validators {
		var r int64
		for m := range months {
			r += v.Rewards[m]
		}
		if r <= 0 {
			continue
		}
		vr := vRow{rewards: r, commission: v.commissionIn(p.lo, p.hi), exchange: v.ExchangeRun}
		if usdOK {
			for i := max(p.lo, v.First); i < min(p.hi, v.First+len(v.Commission)); i++ {
				vr.usd += float64(v.Commission[i-v.First]) / 1e6 * prices[d.Days[i]]
			}
		}
		totalRewards += r
		totalCommission += vr.commission
		set = append(set, vr)
	}
	row.Validators = len(set)
	sort.Slice(set, func(i, j int) bool { return set[i].rewards > set[j].rewards })
	atoms := make([]float64, len(set))
	usds := make([]float64, len(set))
	for i, v := range set {
		atoms[i] = float64(v.commission) / 1e6
		usds[i] = v.usd
		if v.commission == 0 {
			row.ZeroCommission++
		}
	}
	row.PerValidatorAtom = spreadOf(atoms, 1e6)
	if usdOK {
		sp := spreadOf(usds, 100)
		row.PerValidatorUSD = &sp
	}
	group := func(name string, members []int) incomeGroup {
		g := incomeGroup{Group: name, Validators: len(members)}
		var r, c int64
		var usd float64
		var a, u []float64
		for _, i := range members {
			r += set[i].rewards
			c += set[i].commission
			usd += set[i].usd
			a = append(a, atoms[i])
			u = append(u, usds[i])
		}
		g.StakeShare = round6(float64(r) / float64(max(totalRewards, 1)))
		g.CommissionAtom = uatomString(c)
		g.CommissionShare = round6(float64(c) / float64(max(totalCommission, 1)))
		g.MedianAtom = strconv.FormatFloat(math.Round(median(a)*1e6)/1e6, 'f', 6, 64)
		if usdOK {
			g.CommissionUSD = usdPtr(usd)
			g.MedianUSD = usdPtr(median(u))
		}
		return g
	}
	for _, b := range stakeBands {
		var members []int
		for i := range set {
			if rank := i + 1; rank >= b.lo && rank <= b.hi {
				members = append(members, i)
			}
		}
		if len(members) > 0 {
			row.ByStake = append(row.ByStake, group(b.name, members))
		}
	}
	if d.File.ExchangeRunRule != "" {
		var members []int
		for i := range set {
			if set[i].exchange {
				members = append(members, i)
			}
		}
		g := group("exchange-run validators", members)
		row.ExchangeRun = &g
	}
	return row
}

var incomeMethod = map[string]string{
	"source":                      "Every cosmoshub-4 block's commission and rewards events for every bonded validator, read from our own copy of the chain's block results and summed per UTC day. Validators that never withdrew are counted.",
	"earned_commission":           "Commission the chain credited to validators in the period. It is income whether or not the validator withdrew it.",
	"withdrawn_commission":        "Commission validators withdrew in the period (withdraw_commission in successful transactions). Earned minus withdrawn is what stayed unclaimed.",
	"to_delegators":               "Rewards credited to delegators in the period: every validator's rewards minus its commission.",
	"withdrawn_delegator_rewards": "Rewards delegators withdrew in the period, including the automatic withdrawals the chain makes when a delegator delegates more, undelegates or redelegates, and withdrawals by interchain accounts (such as liquid staking hosts) that arrive in relayed packets.",
	"usd":                         "Each day's ATOM at that day's price from the tax price series, never today's price. Dollar figures are left out for a period with any day the price series lacks.",
	"validators":                  "Validators that were bonded and paid rewards at any point in the period. A validator that joined or left during the period is counted with what it earned while bonded.",
	"per_validator":               "The spread of commission earned per validator in the period: 10th, 25th, 50th (median), 75th and 90th percentile. Validators at zero commission are included at zero.",
	"by_stake":                    "Validators grouped by rank in the period's rewards, which the chain pays in proportion to voting power, so the rank is the validator's average stake rank over the period. No validator is named.",
	"exchange_run":                "Validators operated by an exchange, as their own group, so moving their commission into their own exchange wallets is not read as selling. The rule is given in exchange_run_rule. When it is empty the set has not been classified yet and no group is shown.",
	"accuracy":                    "Hub-wide totals are exact to the uatom. Per-validator figures are kept in whole uatom per day and are within 1 uatom of the exact amount for any period.",
	"scope":                       "cosmoshub-4 only, from its first block (2021-02-18).",
}

var breakEvenMethod = map[string]string{
	"question":   "How many of the month's validators would earn less commission than a given monthly running cost if inflation were set to a given rate, with everything else as it was that month.",
	"scenario":   "Rewards at the new rate = rate times the supply at the start of the month times (1 minus the community tax) times the month's share of the year, plus the month's fees to stakers. Each validator keeps its stake share and its effective commission rate, so its commission scales by the new rewards divided by the month's actual rewards.",
	"price":      "The month's average daily ATOM price from the tax price series, or atom_usd when given.",
	"break_even": "A validator is below break-even when its scenario commission in dollars is less than cost_usd. Self-delegation rewards, other income and other costs are not counted.",
	"curve":      "The count below break-even at cost_usd for every inflation rate from 0% to 20% in 0.25 point steps.",
	"validators": "Validators bonded and paid rewards in the month, as in /staking/validator-income.",
}

func (s *Server) handleValidatorIncome(w http.ResponseWriter, r *http.Request) {
	d, err := loadIncome()
	if err != nil {
		http.Error(w, "validator income data unavailable", http.StatusInternalServerError)
		return
	}
	infl, err := loadInflation()
	if err != nil {
		http.Error(w, "inflation data unavailable", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	if view == "" {
		view = "monthly"
	}
	if view != "monthly" && view != "yearly" {
		http.Error(w, "view must be monthly or yearly", http.StatusBadRequest)
		return
	}
	from, to := q.Get("from"), q.Get("to")
	for _, v := range []string{from, to} {
		if v == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, padDate(v, "01")); err != nil {
			http.Error(w, "from and to are YYYY, YYYY-MM or YYYY-MM-DD", http.StatusBadRequest)
			return
		}
	}
	lo, hi := "", "9999-12-31"
	if from != "" {
		lo = padDate(from, "01")
	}
	if to != "" {
		hi = padDate(to, "31")
	}
	var periods []incomePeriod
	need := []string{}
	for _, p := range incomePeriods(d.Days, view) {
		if d.Days[p.hi-1] < lo || d.Days[p.lo] > hi {
			continue
		}
		periods = append(periods, p)
		need = append(need, d.Days[p.lo:p.hi]...)
	}
	chain := chainParam(q.Get(apiChain))
	prices := s.dayPrices(chain, need)
	byDate := make(map[string]inflationDay, len(infl.Days))
	for _, day := range infl.Days {
		byDate[day.Date] = day
	}
	rows := make([]incomeRow, 0, len(periods))
	for _, p := range periods {
		rows = append(rows, incomeRowFor(d, p, byDate, prices, s.oracle != nil))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{
		"chain":             d.File.Chain,
		"as_of":             d.File.AsOf,
		"view":              view,
		"unit":              "ATOM",
		"exchange_run_rule": d.File.ExchangeRunRule,
		"coverage": map[string]any{
			"first_height":    d.File.FirstHeight,
			"last_height":     d.File.LastHeight,
			"first_day":       d.File.FirstDay,
			"last_day":        d.File.LastDay,
			"blocks_counted":  d.File.BlocksCounted,
			"blocks_expected": d.File.BlocksExpected,
			"complete":        d.File.BlocksCounted == d.File.BlocksExpected,
			"validators":      len(d.Validators),
		},
		"method": incomeMethod,
		"rows":   rows,
	})
}

type breakEvenPoint struct {
	Inflation float64 `json:"inflation"`
	Below     int     `json:"below"`
}

func (s *Server) handleValidatorBreakEven(w http.ResponseWriter, r *http.Request) {
	d, err := loadIncome()
	if err != nil {
		http.Error(w, "validator income data unavailable", http.StatusInternalServerError)
		return
	}
	infl, err := loadInflation()
	if err != nil {
		http.Error(w, "inflation data unavailable", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	// The latest month with every day in the file.
	month := q.Get("month")
	months := incomePeriods(d.Days, "monthly")
	var p *incomePeriod
	if month == "" {
		for i := len(months) - 1; i >= 0; i-- {
			start, _ := time.Parse(time.DateOnly, d.Days[months[i].lo])
			if months[i].hi-months[i].lo == start.AddDate(0, 1, -start.Day()).Day() {
				p = &months[i]
				break
			}
		}
	} else {
		for i := range months {
			if months[i].key == month {
				p = &months[i]
			}
		}
	}
	if p == nil {
		http.Error(w, "month must be YYYY-MM within the data", http.StatusBadRequest)
		return
	}
	num := func(name string, def, lo, hi float64) (float64, bool) {
		v := q.Get(name)
		if v == "" {
			return def, true
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < lo || f > hi {
			http.Error(w, fmt.Sprintf("%s must be a number from %g to %g", name, lo, hi), http.StatusBadRequest)
			return 0, false
		}
		return f, true
	}
	var mdays []inflationDay
	for _, day := range infl.Days {
		if day.Date[:7] == p.key {
			mdays = append(mdays, day)
		}
	}
	if len(mdays) == 0 {
		http.Error(w, "no inflation data for that month", http.StatusBadRequest)
		return
	}
	actual := summarize(p.key, mdays, nil)
	cost, ok := num("cost_usd", 1000, 0, 10_000_000)
	if !ok {
		return
	}
	rate, ok := num("inflation", actual.InflationStated, 0, 1)
	if !ok {
		return
	}
	chain := chainParam(q.Get(apiChain))
	price, ok := num("atom_usd", 0, 0, 1_000_000)
	if !ok {
		return
	}
	priceSource := "atom_usd"
	if q.Get("atom_usd") == "" {
		prices := s.dayPrices(chain, d.Days[p.lo:p.hi])
		if len(prices) != p.hi-p.lo {
			http.Error(w, "the price series lacks a day in that month; pass atom_usd", http.StatusConflict)
			return
		}
		var sum float64
		for _, v := range prices {
			sum += v
		}
		price = sum / float64(len(prices))
		priceSource = "month average"
	}

	var rewards, stakers big.Int
	for _, day := range mdays {
		rewards.Add(&rewards, day.RewardsEvents)
		stakers.Add(&stakers, day.Stakers)
	}
	fees := feesToStakers(&rewards, &stakers)
	supply, _ := new(big.Float).SetInt(mdays[0].SupplyStart).Float64()
	feesF, _ := new(big.Float).SetInt(fees).Float64()
	rewardsF, _ := new(big.Float).SetInt(&rewards).Float64()
	start, _ := time.Parse(time.DateOnly, mdays[0].Date)
	share := float64(p.hi-p.lo) / daysInYear(start.Year())
	tax := actual.CommunityTax
	scenarioRewards := func(i float64) float64 { return i*supply*(1-tax)*share + feesF }

	type vc struct{ commissionUSD, rewards float64 }
	var set []vc
	for _, v := range d.Validators {
		var r int64
		r = v.Rewards[p.key]
		if r <= 0 {
			continue
		}
		set = append(set, vc{commissionUSD: float64(v.commissionIn(p.lo, p.hi)) / 1e6 * price, rewards: float64(r)})
	}
	below := func(i float64) (int, float64) {
		scale := scenarioRewards(i) / rewardsF
		n, stake := 0, 0.0
		for _, v := range set {
			if v.commissionUSD*scale < cost {
				n++
				stake += v.rewards
			}
		}
		return n, stake / rewardsF
	}
	n, stake := below(rate)
	nNow, stakeNow := 0, 0.0
	for _, v := range set {
		if v.commissionUSD < cost {
			nNow++
			stakeNow += v.rewards
		}
	}
	curve := []breakEvenPoint{}
	for bp := 0; bp <= 2000; bp += 25 {
		c, _ := below(float64(bp) / 10000)
		curve = append(curve, breakEvenPoint{Inflation: float64(bp) / 10000, Below: c})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{
		"chain":      d.File.Chain,
		"as_of":      d.File.AsOf,
		"month":      p.key,
		"cost_usd":   cost,
		"inflation":  rate,
		"atom_usd":   math.Round(price*10000) / 10000,
		"price_from": priceSource,
		"month_actual": map[string]any{
			"inflation_stated":     actual.InflationStated,
			"inflation_actual":     actual.InflationActual,
			"community_tax":        tax,
			"supply_start_atom":    actual.SupplyStart,
			"rewards_atom":         atomString(&rewards),
			"fees_to_stakers_atom": atomString(fees),
			"below_break_even":     nNow,
			"below_stake_share":    round6(stakeNow / rewardsF),
		},
		"validators":            len(set),
		"below_break_even":      n,
		"below_stake_share":     round6(stake),
		"scenario_rewards_atom": strconv.FormatFloat(math.Round(scenarioRewards(rate))/1e6, 'f', 6, 64),
		"curve":                 curve,
		"method":                breakEvenMethod,
	})
}
