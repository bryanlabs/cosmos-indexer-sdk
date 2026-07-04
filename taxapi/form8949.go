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
	Address      string // the wallet this lot was computed for (see INF-204: never pooled across addresses)
	Description  string // e.g. "1.5 ATOM"
	DateAcquired string // MM/DD/YYYY or "Various"
	DateSold     string // MM/DD/YYYY
	Proceeds     decimal.Decimal
	CostBasis    decimal.Decimal
	GainLoss     decimal.Decimal
	LongTerm     bool
	// BasisUnknown is true when the asset disposed of arrived from outside our
	// indexed view (a plain transfer-in or IBC-in, or a disposal with no
	// matching acquisition at all) rather than a known on-chain acquisition
	// (reward, commission, swap, NFT mint/buy). CostBasis is then 0, not a
	// fabricated basis — the true cost is whatever the user actually paid
	// wherever they acquired it, which we cannot see (see INF-205).
	BasisUnknown bool
}

const basisUnknownNote = " (basis unknown, complete in your aggregator)"

type lot struct {
	qty          decimal.Decimal // display units
	cost         decimal.Decimal // USD per unit
	date         time.Time
	basisUnknown bool // true when this lot's cost is a receipt-time price guess, not a known acquisition cost (see INF-205)
}

// basisUnknownFor reports whether an "in" event establishes a lot whose cost
// basis we can actually stand behind. Rewards/commission are dominion-and-
// control income at receipt (the FMV then IS the basis); swaps and NFT mints/
// buys are on-chain trades we priced ourselves. A plain transfer-in or IBC-in
// could be an exchange withdrawal or a wallet we don't track — the FMV at
// receipt is not necessarily what the user actually paid for it.
func basisUnknownFor(category string) bool {
	return category == "transfer" || category == "ibc_in"
}

// Build8949 runs a per-asset FIFO over the priced rows and emits one 8949 line
// per disposal lot consumed. NOTE: this is a basic engine — cost basis for
// units acquired before the queried window is unknown (treated as 0 / "Various");
// accurate basis needs full acquisition history (archive-node indexing).
//
// NFT marketplace sales (Category "nft_sale") are treated as dispositions of the
// non-fungible asset itself, NOT of the ATOM that changed hands: a buy (in)
// establishes the NFT's USD cost basis, a sell (out) is a capital disposal with
// proceeds = the sale's USD value. (The ATOM leg of an NFT trade is a separate,
// second-order disposal not yet modeled; the headline NFT gain/loss is.)
func Build8949(rows []Row) []Form8949Row {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	lots := map[string][]lot{}    // asset -> FIFO lots (fungible)
	nftLots := map[string][]lot{} // "<collection>/<token>" -> FIFO lots (qty always 1, cost in total USD)
	var out []Form8949Row

	for _, r := range sorted {
		// NFT acquisition via mint: establishes the NFT's cost basis.
		if r.Category == "nft_mint" {
			nftLots[r.Asset] = append(nftLots[r.Asset], lot{qty: decimal.NewFromInt(1), cost: r.ValueUSD, date: r.Time})
			continue
		}

		// NFT marketplace sale: dispose/acquire the NFT, valued in USD.
		if r.Category == "nft_sale" {
			key := r.Asset
			desc := "NFT " + r.Asset
			switch r.Direction {
			case "in": // buyer acquires the NFT; basis = USD paid
				nftLots[key] = append(nftLots[key], lot{qty: decimal.NewFromInt(1), cost: r.ValueUSD, date: r.Time})
			case "out": // seller disposes the NFT; proceeds = USD received
				proceeds := r.ValueUSD
				q := nftLots[key]
				if len(q) == 0 {
					// No recorded mint/buy: this NFT's acquisition is outside our
					// view (bought elsewhere, or minted before the indexed window).
					out = append(out, Form8949Row{
						Description:  desc + basisUnknownNote,
						DateAcquired: "Various",
						DateSold:     r.Time.UTC().Format("01/02/2006"),
						Proceeds:     proceeds,
						CostBasis:    decimal.Zero,
						GainLoss:     proceeds,
						LongTerm:     false,
						BasisUnknown: true,
					})
				} else {
					l := q[0]
					out = append(out, Form8949Row{
						Description:  desc,
						DateAcquired: l.date.UTC().Format("01/02/2006"),
						DateSold:     r.Time.UTC().Format("01/02/2006"),
						Proceeds:     proceeds,
						CostBasis:    l.cost,
						GainLoss:     proceeds.Sub(l.cost),
						LongTerm:     r.Time.Sub(l.date) > 365*24*time.Hour,
					})
					nftLots[key] = q[1:]
				}
			}
			continue
		}

		asset := r.Symbol
		if asset == "" {
			asset = r.Denom
		}
		switch {
		case r.Direction == "in" && r.Category != "fee":
			// acquisition
			lots[asset] = append(lots[asset], lot{
				qty: r.Amount, cost: r.PriceUSD, date: r.Time,
				basisUnknown: basisUnknownFor(r.Category),
			})

		case r.Direction == "out":
			// disposal (incl. fee spends): consume FIFO
			remaining := r.Amount
			disposalPrice := r.PriceUSD
			for remaining.IsPositive() {
				q := lots[asset]
				if len(q) == 0 {
					// no known lot at all: basis unknown
					proceeds := remaining.Mul(disposalPrice)
					out = append(out, Form8949Row{
						Description:  remaining.String() + " " + asset + basisUnknownNote,
						DateAcquired: "Various",
						DateSold:     r.Time.UTC().Format("01/02/2006"),
						Proceeds:     proceeds,
						CostBasis:    decimal.Zero,
						GainLoss:     proceeds,
						LongTerm:     false,
						BasisUnknown: true,
					})
					break
				}
				l := q[0]
				take := decimal.Min(remaining, l.qty)
				proceeds := take.Mul(disposalPrice)
				desc := take.String() + " " + asset
				var cost, gain decimal.Decimal
				if l.basisUnknown {
					// We saw this lot arrive (transfer/IBC-in) but not its true
					// origin, so its receipt-time FMV is not a defensible basis —
					// mark it rather than silently treat it as known (INF-205).
					cost = decimal.Zero
					gain = proceeds
					desc += basisUnknownNote
				} else {
					cost = take.Mul(l.cost)
					gain = proceeds.Sub(cost)
				}
				out = append(out, Form8949Row{
					Description:  desc,
					DateAcquired: l.date.UTC().Format("01/02/2006"),
					DateSold:     r.Time.UTC().Format("01/02/2006"),
					Proceeds:     proceeds,
					CostBasis:    cost,
					GainLoss:     gain,
					LongTerm:     r.Time.Sub(l.date) > 365*24*time.Hour,
					BasisUnknown: l.basisUnknown,
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

// CountUnknownBasis returns how many 8949 lines have an unresolved cost basis.
// Zero means this is a "pure on-chain wallet" (INF-205): every disposal traces
// to a known acquisition (reward, commission, swap, NFT mint/buy), so the 8949
// is fully correct on its own. Nonzero means the wallet received assets from
// outside our view (an exchange withdrawal, IBC-in, or an untracked sender) —
// steer those users to a full-history aggregator instead.
func CountUnknownBasis(rows []Form8949Row) int {
	n := 0
	for _, r := range rows {
		if r.BasisUnknown {
			n++
		}
	}
	return n
}

// ScheduleD is the capital-gains summary that the 8949 totals flow into on IRS
// Schedule D (Form 1040). Short-term and long-term are taxed differently, so they
// stay separate; Net is line 16 (overall capital gain or loss).
type ScheduleD struct {
	// Address and WalletByWallet are set by the handler, not BuildScheduleD:
	// every call is scoped to one wallet, lots are never pooled across
	// addresses (Rev. Proc. 2024-28, see INF-204). Callers combining several
	// wallets' responses should keep them as per-wallet subtotals, not sum them
	// into one number, to stay labeled wallet-by-wallet.
	Address        string `json:"address,omitempty"`
	WalletByWallet bool   `json:"wallet_by_wallet"`
	// UnknownBasisLines > 0 means this wallet is not "pure on-chain" (INF-205):
	// some disposed lots arrived from outside our view, so these totals are a
	// floor, not the full picture — steer to a full-history aggregator.
	UnknownBasisLines  int             `json:"unknown_basis_lines"`
	ShortTermProceeds  decimal.Decimal `json:"short_term_proceeds"`
	ShortTermCostBasis decimal.Decimal `json:"short_term_cost_basis"`
	ShortTermGainLoss  decimal.Decimal `json:"short_term_gain_loss"` // Schedule D line 7
	LongTermProceeds   decimal.Decimal `json:"long_term_proceeds"`
	LongTermCostBasis  decimal.Decimal `json:"long_term_cost_basis"`
	LongTermGainLoss   decimal.Decimal `json:"long_term_gain_loss"` // Schedule D line 15
	NetGainLoss        decimal.Decimal `json:"net_gain_loss"`       // Schedule D line 16
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
// the Part I / Part II split. The Address column identifies which wallet each
// lot belongs to — lots are computed per-address and never pooled across
// wallets (Rev. Proc. 2024-28, see INF-204), so a multi-address report is
// several single-wallet responses concatenated, not a combined basis pool.
func Write8949CSV(out io.Writer, rows []Form8949Row) error {
	w := csv.NewWriter(out)
	defer w.Flush()
	_ = w.Write([]string{"Address", "Part", "Description of property", "Date acquired", "Date sold", "Proceeds (USD)", "Cost basis (USD)", "Gain or loss (USD)", "Basis unknown"})
	write := func(part string, longTerm bool) {
		for _, r := range rows {
			if r.LongTerm != longTerm {
				continue
			}
			_ = w.Write([]string{r.Address, part, r.Description, r.DateAcquired, r.DateSold, r.Proceeds.StringFixed(2), r.CostBasis.StringFixed(2), r.GainLoss.StringFixed(2), boolStr(r.BasisUnknown)})
		}
	}
	write("I (short-term)", false)
	write("II (long-term)", true)
	return w.Error()
}
