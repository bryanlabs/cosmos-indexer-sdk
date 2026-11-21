package taxapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GET /staking/inflation: how much new ATOM the Hub minted, where it went, and
// what staking paid against it, by day, month or year. Built from the mint
// event of every cosmoshub-4 block (INF-748). The daily file is produced by
// the extraction under cluster/apps/cosmos-tax-sdk-backfill and embedded here
// until the daily job writes it to Postgres.

//go:embed stakingdata/inflation_daily.json
var inflationDailyJSON []byte

type inflationDay struct {
	Date             string
	FirstHeight      int64
	LastHeight       int64
	Blocks           int64
	Minted           *big.Int
	CommunityPool    *big.Int
	Stakers          *big.Int
	Commission       *big.Int
	RewardsEvents    *big.Int
	CommissionEvents *big.Int
	SupplyStart      *big.Int
	InflationAvg     float64
	BondedAvg        float64
	CommunityTax     string
	Seconds          float64
}

type inflationData struct {
	Chain           string
	AsOf            string
	FirstBlockTime  string
	FirstHeight     int64
	LastHeight      int64
	BlocksCounted   int64
	BlocksExpected  int64
	ProposalHeights map[string]int64
	Days            []inflationDay
}

type inflationFile struct {
	Chain           string              `json:"chain"`
	AsOf            string              `json:"as_of"`
	FirstBlockTime  string              `json:"first_block_time"`
	FirstHeight     int64               `json:"first_height"`
	LastHeight      int64               `json:"last_height"`
	BlocksCounted   int64               `json:"blocks_counted"`
	BlocksExpected  int64               `json:"blocks_expected"`
	ProposalHeights map[string]int64    `json:"proposal_heights"`
	Columns         []string            `json:"columns"`
	Days            [][]json.RawMessage `json:"days"`
}

var (
	inflationOnce sync.Once
	inflationSet  *inflationData
	inflationErr  error
)

func loadInflation() (*inflationData, error) {
	inflationOnce.Do(func() { inflationSet, inflationErr = parseInflation(inflationDailyJSON) })
	return inflationSet, inflationErr
}

func parseInflation(raw []byte) (*inflationData, error) {
	var f inflationFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, c := range f.Columns {
		col[c] = i
	}
	need := []string{"date", "first_height", "last_height", "blocks", "minted_uatom", "community_pool_uatom",
		"stakers_uatom", "commission_uatom", "rewards_events_uatom", "commission_events_uatom",
		"supply_start_uatom", "inflation_param_avg", "bonded_ratio_avg", "community_tax_start"}
	for _, c := range need {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("inflation file lacks column %s", c)
		}
	}
	first, err := time.Parse(time.RFC3339, f.FirstBlockTime)
	if err != nil {
		return nil, err
	}
	d := &inflationData{Chain: f.Chain, AsOf: f.AsOf, FirstBlockTime: f.FirstBlockTime, FirstHeight: f.FirstHeight,
		LastHeight: f.LastHeight, BlocksCounted: f.BlocksCounted, BlocksExpected: f.BlocksExpected,
		ProposalHeights: f.ProposalHeights}
	str := func(r []json.RawMessage, name string) (string, error) {
		var s string
		err := json.Unmarshal(r[col[name]], &s)
		return s, err
	}
	num := func(r []json.RawMessage, name string) (*big.Int, error) {
		v, ok := new(big.Int).SetString(string(r[col[name]]), 10)
		if !ok {
			return nil, fmt.Errorf("bad integer %s in %s", r[col[name]], name)
		}
		return v, nil
	}
	flt := func(r []json.RawMessage, name string) (float64, error) {
		var v float64
		err := json.Unmarshal(r[col[name]], &v)
		return v, err
	}
	for _, r := range f.Days {
		if len(r) != len(f.Columns) {
			return nil, fmt.Errorf("inflation row has %d values for %d columns", len(r), len(f.Columns))
		}
		var day inflationDay
		if day.Date, err = str(r, "date"); err != nil {
			return nil, err
		}
		if day.CommunityTax, err = str(r, "community_tax_start"); err != nil {
			return nil, err
		}
		ints := []struct {
			name string
			dst  **big.Int
		}{{"minted_uatom", &day.Minted}, {"community_pool_uatom", &day.CommunityPool}, {"stakers_uatom", &day.Stakers},
			{"commission_uatom", &day.Commission}, {"rewards_events_uatom", &day.RewardsEvents},
			{"commission_events_uatom", &day.CommissionEvents}, {"supply_start_uatom", &day.SupplyStart}}
		for _, it := range ints {
			if *it.dst, err = num(r, it.name); err != nil {
				return nil, err
			}
		}
		for _, it := range []struct {
			name string
			dst  *int64
		}{{"first_height", &day.FirstHeight}, {"last_height", &day.LastHeight}, {"blocks", &day.Blocks}} {
			v, err := num(r, it.name)
			if err != nil {
				return nil, err
			}
			*it.dst = v.Int64()
		}
		if day.InflationAvg, err = flt(r, "inflation_param_avg"); err != nil {
			return nil, err
		}
		if day.BondedAvg, err = flt(r, "bonded_ratio_avg"); err != nil {
			return nil, err
		}
		dayStart, err := time.Parse(time.DateOnly, day.Date)
		if err != nil {
			return nil, err
		}
		start := dayStart
		if first.After(start) {
			start = first
		}
		day.Seconds = dayStart.Add(24 * time.Hour).Sub(start).Seconds()
		d.Days = append(d.Days, day)
	}
	if len(d.Days) == 0 {
		return nil, fmt.Errorf("inflation file has no days")
	}
	return d, nil
}

// inflationChange is one governance decision that moved issuance or its split.
type inflationChange struct {
	Proposal int    `json:"proposal"`
	Date     string `json:"date"`
	Height   int64  `json:"height,omitempty"`
	Setting  string `json:"setting"`
	From     string `json:"from"`
	To       string `json:"to"`
	Effect   string `json:"effect"`
}

func inflationChanges(heights map[string]int64) []inflationChange {
	out := []inflationChange{
		{Proposal: 30, Date: "2020-11-18", Setting: "blocks_per_year", From: "4855015", To: "4360000",
			Effect: "Passed on the chain before cosmoshub-4 and carried into it. Each block mints a year's issuance divided by this number, so when the chain makes more blocks than this in a year it mints more than the stated rate."},
		{Proposal: 48, Date: "2021-06-16", Setting: "inflation_rate_change", From: "0.13", To: "1.0",
			Effect: "Lets the inflation rate move toward its limit within one year instead of drifting slowly, so it reacts faster when the staked share is below the 67% goal."},
		{Proposal: 88, Date: "2022-12-19", Setting: "community_tax", From: "0.02", To: "0.10",
			Effect: "Sends 10% of new ATOM to the community pool instead of 2%, which lowers what stakers receive."},
		{Proposal: 848, Date: "2023-11-25", Setting: "inflation_max", From: "0.20", To: "0.10",
			Effect: "Caps the stated inflation rate at 10% instead of 20%. The stated rate had been near 14%."},
		{Proposal: 996, Date: "2025-05-15", Setting: "community_tax", From: "0.10", To: "0.02",
			Effect: "Returns the community pool share to 2%."},
	}
	for i := range out {
		out[i].Height = heights[fmt.Sprint(out[i].Proposal)]
	}
	return out
}

type inflationRow struct {
	Period                 string  `json:"period"`
	StartDate              string  `json:"start_date"`
	EndDate                string  `json:"end_date"`
	Days                   float64 `json:"days"`
	FirstHeight            int64   `json:"first_height"`
	LastHeight             int64   `json:"last_height"`
	Blocks                 int64   `json:"blocks"`
	BlocksPerYear          float64 `json:"blocks_per_year"`
	Minted                 string  `json:"minted_atom"`
	ToCommunityPool        string  `json:"to_community_pool_atom"`
	ToStakers              string  `json:"to_stakers_atom"`
	ToValidatorsCommission string  `json:"to_validator_commission_atom"`
	ToDelegators           string  `json:"to_delegators_atom"`
	SupplyStart            string  `json:"supply_start_atom"`
	SupplyEnd              string  `json:"supply_end_atom,omitempty"`
	OtherSupplyChange      string  `json:"other_supply_change_atom,omitempty"`
	CommunityTax           float64 `json:"community_tax"`
	CommissionShare        float64 `json:"commission_share"`
	InflationStated        float64 `json:"inflation_stated"`
	InflationActual        float64 `json:"inflation_actual"`
	BondedRatio            float64 `json:"bonded_ratio"`
	AprStated              float64 `json:"apr_stated"`
	AprPaid                float64 `json:"apr_paid"`
	AprAfterCommission     float64 `json:"apr_after_commission"`
	AprReal                float64 `json:"apr_real"`
	FeeShare               string  `json:"fees_to_stakers_atom"`
}

func atomString(u *big.Int) string {
	neg := u.Sign() < 0
	a := new(big.Int).Abs(u)
	q, r := new(big.Int).QuoRem(a, big.NewInt(1_000_000), new(big.Int))
	s := fmt.Sprintf("%s.%06d", q.String(), r.Int64())
	if neg {
		return "-" + s
	}
	return s
}

func ratio(a, b *big.Int) float64 {
	if b.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	return f
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

func daysInYear(year int) float64 {
	return time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)).Hours() / 24
}

// summarize turns a run of consecutive days into one row. next is the day after
// the run, when there is one, for the supply at the end of the period.
func summarize(period string, days []inflationDay, next *inflationDay) inflationRow {
	sum := func(get func(inflationDay) *big.Int) *big.Int {
		t := new(big.Int)
		for _, d := range days {
			t.Add(t, get(d))
		}
		return t
	}
	minted := sum(func(d inflationDay) *big.Int { return d.Minted })
	pool := sum(func(d inflationDay) *big.Int { return d.CommunityPool })
	stakers := sum(func(d inflationDay) *big.Int { return d.Stakers })
	commission := sum(func(d inflationDay) *big.Int { return d.Commission })
	rewards := sum(func(d inflationDay) *big.Int { return d.RewardsEvents })
	commissionEvents := sum(func(d inflationDay) *big.Int { return d.CommissionEvents })
	var blocks int64
	var secs, inflW, bondW, stakedSecs float64
	for _, d := range days {
		blocks += d.Blocks
		secs += d.Seconds
		inflW += d.InflationAvg * float64(d.Blocks)
		bondW += d.BondedAvg * float64(d.Blocks)
		supplyF, _ := new(big.Float).SetInt(d.SupplyStart).Float64()
		stakedSecs += d.BondedAvg * supplyF * d.Seconds
	}
	first, last := days[0], days[len(days)-1]
	year, _ := time.Parse(time.DateOnly, first.Date)
	yearSecs := daysInYear(year.Year()) * 86400
	scale := yearSecs / secs
	supply := first.SupplyStart
	row := inflationRow{
		Period: period, StartDate: first.Date, EndDate: last.Date, Days: math.Round(secs/864) / 100,
		FirstHeight: first.FirstHeight, LastHeight: last.LastHeight, Blocks: blocks,
		BlocksPerYear: math.Round(float64(blocks) * scale),
		Minted:        atomString(minted), ToCommunityPool: atomString(pool), ToStakers: atomString(stakers),
		ToValidatorsCommission: atomString(commission),
		ToDelegators:           atomString(new(big.Int).Sub(stakers, commission)),
		SupplyStart:            atomString(supply),
		FeeShare:               atomString(new(big.Int).Sub(rewards, stakers)),
	}
	if next != nil {
		row.SupplyEnd = atomString(next.SupplyStart)
		other := new(big.Int).Sub(next.SupplyStart, supply)
		row.OtherSupplyChange = atomString(other.Sub(other, minted))
	}
	bonded := bondW / float64(blocks)
	tax := ratio(pool, minted)
	actual := ratio(minted, supply) * scale
	commShare := ratio(commissionEvents, rewards)
	row.CommunityTax = round6(tax)
	row.CommissionShare = round6(commShare)
	row.InflationStated = round6(inflW / float64(blocks))
	row.InflationActual = round6(actual)
	row.BondedRatio = round6(bonded)
	row.AprStated = round6(row.InflationStated * (1 - tax) / bonded)
	// Rewards over the ATOM staked on average across the period (each day's
	// staked share of that day's supply, weighted by time), scaled to a year.
	rewardsF, _ := new(big.Float).SetInt(rewards).Float64()
	paid := rewardsF * yearSecs / stakedSecs
	row.AprPaid = round6(paid)
	row.AprAfterCommission = round6(paid * (1 - commShare))
	row.AprReal = round6((1+paid)/(1+actual) - 1)
	return row
}

func groupInflation(days []inflationDay, view string) []inflationRow {
	key := func(d inflationDay) string {
		switch view {
		case "yearly":
			return d.Date[:4]
		case "daily":
			return d.Date
		}
		return d.Date[:7]
	}
	var out []inflationRow
	for i := 0; i < len(days); {
		j := i
		for j < len(days) && key(days[j]) == key(days[i]) {
			j++
		}
		var next *inflationDay
		if j < len(days) {
			next = &days[j]
		}
		out = append(out, summarize(key(days[i]), days[i:j], next))
		i = j
	}
	return out
}

var inflationMethod = map[string]string{
	"source":               "Every cosmoshub-4 block's mint event, read from our own copy of the chain's block results, summed per UTC day. Nothing is sampled or estimated.",
	"minted":               "New ATOM created by the chain. Each block mints one block's share of the year's issuance: the stated inflation rate times the supply, divided by blocks_per_year.",
	"inflation_stated":     "The inflation rate the chain reports in its mint parameters, averaged over the period's blocks.",
	"inflation_actual":     "ATOM minted in the period divided by the supply at its start, scaled to that calendar year's length. It is higher than the stated rate when the chain makes more blocks in a year than blocks_per_year assumes.",
	"supply":               "Total ATOM supply before the period's first block, read from the mint event (annual provisions divided by the inflation rate). Other supply change is any change not explained by minting, such as burned tokens.",
	"community_pool":       "The community tax share of new ATOM, at the rate in force at each block. A change passed by governance applies from the block after the vote closed.",
	"stakers":              "New ATOM left after the community tax, paid to bonded validators by voting power. Validators keep their commission rate from it and the rest accrues to their delegators.",
	"commission":           "The commission part of what stakers received, in the same proportion the chain's commission events show for the period. Delegators' part is the rest.",
	"fees_to_stakers":      "Transaction fees paid to stakers on top of new ATOM, net of the fractions of a uatom the chain sends to the community pool when it divides each block's rewards. It can be a tiny negative amount when fees are close to zero.",
	"bonded_ratio":         "Share of all ATOM that is staked, as reported in each block's mint event, averaged over the period's blocks.",
	"apr_stated":           "What staking pays if the stated inflation rate were the real one: stated inflation times (1 minus community tax), divided by the staked share.",
	"apr_paid":             "What staking actually paid before validator commission: rewards the chain credited to validators in the period (new ATOM plus fees), divided by the ATOM staked on average over the period, scaled to a year.",
	"apr_after_commission": "Paid APR after the average validator commission in the period. A delegator's own rate depends on their validator's commission.",
	"apr_real":             "Paid APR adjusted for the new ATOM everyone else received: (1 + paid APR) / (1 + actual inflation) minus 1. Holding ATOM without staking loses about the actual inflation rate in share of supply.",
	"periods":              "Days are UTC. The first day starts at the chain's first block (2021-02-18 12:23:53 UTC). Rates for a partial period are scaled to its calendar year.",
	"scope":                "cosmoshub-4 only, from its first block. Earlier Hub chains are not included because their mint events are not part of our block copy yet.",
}

func (s *Server) handleStakingInflation(w http.ResponseWriter, r *http.Request) {
	data, err := loadInflation()
	if err != nil {
		http.Error(w, "inflation data unavailable", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	if view == "" {
		view = "monthly"
	}
	if view != "monthly" && view != "yearly" && view != "daily" {
		http.Error(w, "view must be daily, monthly or yearly", http.StatusBadRequest)
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
	days := data.Days
	lo, hi := 0, len(days)
	if from != "" {
		f := padDate(from, "01")
		for lo < hi && days[lo].Date < f {
			lo++
		}
	}
	if to != "" {
		t := padDate(to, "31")
		for hi > lo && days[hi-1].Date > t {
			hi--
		}
	}
	rows := []inflationRow{}
	if lo < hi {
		// Group over the full series so a period's end supply can use the next day,
		// then keep the requested periods.
		all := groupInflation(days, view)
		for _, row := range all {
			if row.EndDate >= days[lo].Date && row.StartDate <= days[hi-1].Date {
				rows = append(rows, row)
			}
		}
	}
	last := data.Days[len(data.Days)-1]
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{
		"chain": data.Chain,
		"as_of": data.AsOf,
		"view":  view,
		"unit":  "ATOM",
		"coverage": map[string]any{
			"first_height":    data.FirstHeight,
			"last_height":     data.LastHeight,
			"first_day":       data.Days[0].Date,
			"last_day":        last.Date,
			"blocks_counted":  data.BlocksCounted,
			"blocks_expected": data.BlocksExpected,
			"complete":        data.BlocksCounted == data.BlocksExpected,
		},
		"method":  inflationMethod,
		"changes": inflationChanges(data.ProposalHeights),
		"rows":    rows,
	})
}

// padDate widens YYYY or YYYY-MM to a full date with the given day (or month and day).
func padDate(v, day string) string {
	switch len(v) {
	case 4:
		if day == "01" {
			return v + "-01-01"
		}
		return v + "-12-31"
	case 7:
		return v + "-" + day
	}
	return strings.TrimSpace(v)
}
