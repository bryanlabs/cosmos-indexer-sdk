package taxapi

import (
	"encoding/csv"
	"io"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Form8949Row is one disposal line for IRS Form 8949 (Sales and Other
// Dispositions of Capital Assets).
type Form8949Row struct {
	Description  string // e.g. "1.5 ATOM"
	DateAcquired string // MM/DD/YYYY or "Various"
	DateSold     string // MM/DD/YYYY
	Proceeds     decimal.Decimal
	CostBasis    decimal.Decimal
	GainLoss     decimal.Decimal
	LongTerm     bool
}

type lot struct {
	qty  decimal.Decimal // display units
	cost decimal.Decimal // USD per unit
	date time.Time
}

// Build8949 runs a per-asset FIFO over the priced rows and emits one 8949 line
// per disposal lot consumed. NOTE: this is a basic engine — cost basis for
// units acquired before the queried window is unknown (treated as 0 / "Various");
// accurate basis needs full acquisition history (archive-node indexing).
func Build8949(rows []Row) []Form8949Row {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	lots := map[string][]lot{} // asset -> FIFO lots
	var out []Form8949Row

	for _, r := range sorted {
		asset := r.Symbol
		if asset == "" {
			asset = r.Denom
		}
		switch {
		case r.Direction == "in" && r.Category != "fee":
			// acquisition
			lots[asset] = append(lots[asset], lot{qty: r.Amount, cost: r.PriceUSD, date: r.Time})

		case r.Direction == "out":
			// disposal (incl. fee spends): consume FIFO
			remaining := r.Amount
			disposalPrice := r.PriceUSD
			for remaining.IsPositive() {
				q := lots[asset]
				if len(q) == 0 {
					// no known lot: basis unknown
					proceeds := remaining.Mul(disposalPrice)
					out = append(out, Form8949Row{
						Description:  remaining.String() + " " + asset,
						DateAcquired: "Various",
						DateSold:     r.Time.UTC().Format("01/02/2006"),
						Proceeds:     proceeds,
						CostBasis:    decimal.Zero,
						GainLoss:     proceeds,
						LongTerm:     false,
					})
					break
				}
				l := q[0]
				take := decimal.Min(remaining, l.qty)
				proceeds := take.Mul(disposalPrice)
				cost := take.Mul(l.cost)
				out = append(out, Form8949Row{
					Description:  take.String() + " " + asset,
					DateAcquired: l.date.UTC().Format("01/02/2006"),
					DateSold:     r.Time.UTC().Format("01/02/2006"),
					Proceeds:     proceeds,
					CostBasis:    cost,
					GainLoss:     proceeds.Sub(cost),
					LongTerm:     r.Time.Sub(l.date) > 365*24*time.Hour,
				})
				remaining = remaining.Sub(take)
				if l.qty.Equal(take) {
					lots[asset] = q[1:]
				} else {
					q[0].qty = l.qty.Sub(take)
				}
			}
		}
	}
	return out
}

// ScheduleD is the capital-gains summary that the 8949 totals flow into on IRS
// Schedule D (Form 1040). Short-term and long-term are taxed differently, so they
// stay separate; Net is line 16 (overall capital gain or loss).
type ScheduleD struct {
	ShortTermProceeds  decimal.Decimal `json:"short_term_proceeds"`
	ShortTermCostBasis decimal.Decimal `json:"short_term_cost_basis"`
	ShortTermGainLoss  decimal.Decimal `json:"short_term_gain_loss"` // Schedule D line 7
	LongTermProceeds   decimal.Decimal `json:"long_term_proceeds"`
	LongTermCostBasis  decimal.Decimal `json:"long_term_cost_basis"`
	LongTermGainLoss   decimal.Decimal `json:"long_term_gain_loss"` // Schedule D line 15
	NetGainLoss        decimal.Decimal `json:"net_gain_loss"`        // Schedule D line 16
}

// BuildScheduleD rolls up 8949 lines into the Schedule D short/long-term totals.
func BuildScheduleD(rows []Form8949Row) ScheduleD {
	var d ScheduleD
	for _, r := range rows {
		if r.LongTerm {
			d.LongTermProceeds = d.LongTermProceeds.Add(r.Proceeds)
			d.LongTermCostBasis = d.LongTermCostBasis.Add(r.CostBasis)
			d.LongTermGainLoss = d.LongTermGainLoss.Add(r.GainLoss)
		} else {
			d.ShortTermProceeds = d.ShortTermProceeds.Add(r.Proceeds)
			d.ShortTermCostBasis = d.ShortTermCostBasis.Add(r.CostBasis)
			d.ShortTermGainLoss = d.ShortTermGainLoss.Add(r.GainLoss)
		}
	}
	d.NetGainLoss = d.ShortTermGainLoss.Add(d.LongTermGainLoss)
	return d
}

// Write8949CSV writes the 8949 rows, short-term first then long-term, matching
// the Part I / Part II split.
func Write8949CSV(out io.Writer, rows []Form8949Row) error {
	w := csv.NewWriter(out)
	defer w.Flush()
	_ = w.Write([]string{"Part", "Description of property", "Date acquired", "Date sold", "Proceeds (USD)", "Cost basis (USD)", "Gain or loss (USD)"})
	write := func(part string, longTerm bool) {
		for _, r := range rows {
			if r.LongTerm != longTerm {
				continue
			}
			_ = w.Write([]string{part, r.Description, r.DateAcquired, r.DateSold, r.Proceeds.StringFixed(2), r.CostBasis.StringFixed(2), r.GainLoss.StringFixed(2)})
		}
	}
	write("I (short-term)", false)
	write("II (long-term)", true)
	return w.Error()
}
