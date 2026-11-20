package taxapi

import (
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

// flowSaleProceeds is the native coin the chain credited addr in each message
// where addr sold an NFT. An nft_sale row carries the sale price, but the
// marketplace pays the seller the price less its fee and the royalty, so the
// coin the seller holds afterwards is what the chain credited.
func (s *Server) flowSaleProceeds(addr string, events []tax.TaxableEvent) (map[uint]decimal.Decimal, error) {
	var ids []uint
	seen := map[uint]bool{}
	for _, e := range events {
		if e.Category == apiNFTSale && e.FromAddr == addr && e.ToAddr != addr && e.Denom == s.native.Denom && !seen[e.MessageID] {
			seen[e.MessageID] = true
			ids = append(ids, e.MessageID)
		}
	}
	rows, err := s.flowReceived(addr, ids)
	if err != nil {
		return nil, err
	}
	return saleCredits(rows, s.native.Denom), nil
}

// saleCredits sums the native coin in each message's receipts.
func saleCredits(rows []flowReceipt, native string) map[uint]decimal.Decimal {
	out := map[uint]decimal.Decimal{}
	for _, r := range rows {
		for _, coin := range strings.Split(r.Value, ",") {
			amount, ok := strings.CutSuffix(strings.TrimSpace(coin), native)
			if !ok {
				continue
			}
			v, err := decimal.NewFromString(amount)
			if err != nil {
				continue
			}
			out[r.MessageID] = out[r.MessageID].Add(v)
		}
	}
	return out
}

// netSaleProceeds sets the amount of each NFT sale addr made to the coin the
// chain credited it in that message, less what other rows of the message
// already credit. Several sales in one message share the credit in
// proportion to their prices, the last taking the rounding. A message with
// no recorded credit keeps the price.
func netSaleProceeds(events []flowEvent, addr string, credited map[uint]decimal.Decimal) []flowEvent {
	sales := map[uint][]int{}
	other := map[uint]decimal.Decimal{}
	for i, e := range events {
		if e.Dir != "in" {
			continue
		}
		if e.Category == apiNFTSale && e.From == addr && e.To != addr {
			sales[e.Key.Message] = append(sales[e.Key.Message], i)
		} else {
			other[e.Key.Message] = other[e.Key.Message].Add(e.Amount)
		}
	}
	for msg, idx := range sales {
		paid, ok := credited[msg]
		if !ok {
			continue
		}
		net := decimal.Max(paid.Sub(other[msg]), decimal.Zero)
		price := decimal.Zero
		for _, i := range idx {
			price = price.Add(events[i].Amount)
		}
		if !price.IsPositive() {
			continue
		}
		left := net
		for n, i := range idx {
			share := left
			if n < len(idx)-1 {
				share = net.Mul(events[i].Amount).Div(price).Floor()
			}
			events[i].Amount = share
			left = left.Sub(share)
		}
	}
	return events
}
