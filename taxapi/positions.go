package taxapi

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Position is the open FIFO lot inventory of one asset as of a date: what the
// wallet still holds from acquisitions we saw, its cost basis, and the
// unrealized gain or loss against that day's reference price.
type Position struct {
	Asset             string          `json:"asset"`
	Denom             string          `json:"denom"`
	Quantity          decimal.Decimal `json:"quantity"`
	CostBasisUSD      decimal.Decimal `json:"cost_basis_usd"`
	BasisUnknownQty   decimal.Decimal `json:"basis_unknown_quantity"`
	MarketPriceUSD    decimal.Decimal `json:"market_price_usd"`
	MarketValueUSD    decimal.Decimal `json:"market_value_usd"`
	UnrealizedGainUSD decimal.Decimal `json:"unrealized_gain_usd"`
	PriceMissing      bool            `json:"price_missing"`
	Lots              int             `json:"lots"`
	OldestLot         string          `json:"oldest_lot,omitempty"`
}

// BuildPositions replays the same FIFO Form 8949 uses and reports what is
// left. priceAt returns the reference price for a denom on the as-of date.
func BuildPositions(rows []Row, asOf time.Time, priceAt func(denom string) (decimal.Decimal, bool)) []Position {
	sorted := make([]Row, 0, len(rows))
	for _, r := range rows {
		if !r.Time.After(asOf) {
			sorted = append(sorted, r)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	lots := map[string][]lot{}
	denoms := map[string]string{}
	for _, r := range sorted {
		if r.Excluded || (r.AssetIdentity != nil && r.AssetDecision == nil) {
			continue
		}
		if !r.isTaxable() || r.UserTag == TagInternalTransfer || r.UserTag == TagProtocolDeposit || r.UserTag == TagSpam {
			continue
		}
		if r.Category == "nft_sale" || r.Category == "nft_mint" {
			continue
		}
		asset := r.Symbol
		if asset == "" {
			asset = r.Denom
		}
		denoms[asset] = r.Denom
		switch {
		case r.Direction == "in" && r.Category != "fee":
			cost, acquired, unknown := r.PriceUSD, r.Time, basisUnknownForRow(r)
			if r.ManualBasisUSD != nil && r.ManualAcquiredDate != nil && r.Amount.IsPositive() {
				cost, acquired, unknown = r.ManualBasisUSD.DivRound(r.Amount, 64), *r.ManualAcquiredDate, false
			}
			lots[asset] = append(lots[asset], lot{qty: r.Amount, cost: cost, date: acquired, basisUnknown: unknown})
		case r.Direction == "out":
			remaining := r.Amount
			for remaining.IsPositive() && len(lots[asset]) > 0 {
				q := lots[asset]
				take := decimal.Min(remaining, q[0].qty)
				remaining = remaining.Sub(take)
				if take.Equal(q[0].qty) {
					lots[asset] = q[1:]
				} else {
					q[0].qty = q[0].qty.Sub(take)
				}
			}
		}
	}
	assets := make([]string, 0, len(lots))
	for a := range lots {
		if len(lots[a]) > 0 {
			assets = append(assets, a)
		}
	}
	sort.Strings(assets)
	out := make([]Position, 0, len(assets))
	for _, a := range assets {
		p := Position{Asset: a, Denom: denoms[a], Lots: len(lots[a])}
		for i, l := range lots[a] {
			p.Quantity = p.Quantity.Add(l.qty)
			if l.basisUnknown {
				p.BasisUnknownQty = p.BasisUnknownQty.Add(l.qty)
			} else {
				p.CostBasisUSD = p.CostBasisUSD.Add(l.qty.Mul(l.cost))
			}
			if i == 0 {
				p.OldestLot = l.date.UTC().Format("2006-01-02")
			}
		}
		if price, ok := priceAt(denoms[a]); ok {
			p.MarketPriceUSD = price.Round(8)
			p.MarketValueUSD = p.Quantity.Mul(price).Round(2)
			p.UnrealizedGainUSD = p.Quantity.Sub(p.BasisUnknownQty).Mul(price).Sub(p.CostBasisUSD).Round(2)
		} else {
			p.PriceMissing = true
		}
		p.CostBasisUSD = p.CostBasisUSD.Round(2)
		out = append(out, p)
	}
	return out
}

// GET /positions?address=&chain=&date=YYYY-MM-DD&format=csv|json
func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := strings.TrimSpace(q.Get("address"))
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := chainParam(q.Get("chain"))
	asOf := dateParam(q.Get("date"), nowUTC())
	// Load the wallet's whole history, not just up to the as-of date, so the
	// caller can pass the same asset_decisions a full report carries even when
	// some flagged receipts fall after the as-of date. BuildPositions applies
	// the date cut itself.
	rows, err := s.rowsForRequest(r, chain, []string{addr}, time.Time{}, nowUTC().AddDate(0, 0, 1))
	if err != nil {
		writeTaxError(w, err)
		return
	}
	day := asOf.UTC().Format("2006-01-02")
	positions := BuildPositions(rows, asOf.AddDate(0, 0, 1), func(denom string) (decimal.Decimal, bool) {
		base, _ := s.resolveBaseDenom(denom)
		if p, ok := s.oracle.PriceAt(chain, base, day); ok {
			return decimal.NewFromFloat(p), true
		}
		if p, ok := s.oracle.PriceAt(chain, denom, day); ok {
			return decimal.NewFromFloat(p), true
		}
		return decimal.Zero, false
	})
	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=positions.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"address", "as_of", "asset", "denom", "quantity", "cost_basis_usd", "basis_unknown_quantity", "market_price_usd", "market_value_usd", "unrealized_gain_usd", "price_missing", "lots", "oldest_lot"})
		for _, p := range positions {
			_ = cw.Write([]string{addr, day, spreadsheetLabel(p.Asset), p.Denom, p.Quantity.String(), p.CostBasisUSD.StringFixed(2), p.BasisUnknownQty.String(), p.MarketPriceUSD.String(), p.MarketValueUSD.StringFixed(2), p.UnrealizedGainUSD.StringFixed(2), boolStr(p.PriceMissing), itoa(p.Lots), p.OldestLot})
		}
		cw.Flush()
		return
	}
	writeJSON(w, map[string]any{
		"address": addr, "chain": chain, "as_of": day, "methodology_version": MethodologyVersion,
		"note":      "Open lots from acquisitions seen in the indexed history (Cosmos Hub from 2025-12-31 onward), FIFO, per wallet. Coins acquired before the index starts are not represented. Quantity with basis_unknown arrived from outside our view (a plain transfer or IBC receipt) and is valued but has no cost basis here. Staked, unbonding and protocol-held coins stay in quantity because delegating is not a disposal; the liquid, staked and unbonding split is on /balances.",
		"positions": positions,
	})
}

func itoa(n int) string { return decimal.NewFromInt(int64(n)).String() }
