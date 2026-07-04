package taxapi

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(i int64) decimal.Decimal { return decimal.NewFromInt(i) }

func TestBuild8949FIFOGain(t *testing.T) {
	day1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	day10 := day1.AddDate(0, 0, 10)
	rows := []Row{
		// Acquired via reward: a known on-chain acquisition, so basis is trusted
		// (see TestBuild8949TransferInHasUnknownBasis for the opposite case).
		{Time: day1, Direction: "in", Category: "reward", Symbol: "ATOM", Amount: d(10), PriceUSD: d(2)},
		{Time: day10, Direction: "out", Category: "transfer", Symbol: "ATOM", Amount: d(4), PriceUSD: d(3)},
	}
	got := Build8949(rows)
	if len(got) != 1 {
		t.Fatalf("want 1 disposal, got %d: %+v", len(got), got)
	}
	r := got[0]
	if !r.Proceeds.Equal(d(12)) || !r.CostBasis.Equal(d(8)) || !r.GainLoss.Equal(d(4)) || r.LongTerm || r.BasisUnknown {
		t.Fatalf("wrong 8949 calc: %+v", r)
	}
}

func TestBuild8949NFTSale(t *testing.T) {
	buy := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sell := buy.AddDate(0, 0, 10)
	rows := []Row{
		// Bought NFT for ~$2, sold for ~$3 → short-term gain $1 on the NFT itself.
		{Time: buy, Direction: "in", Category: "nft_sale", Asset: "coll/42", ValueUSD: d(2)},
		{Time: sell, Direction: "out", Category: "nft_sale", Asset: "coll/42", ValueUSD: d(3)},
	}
	got := Build8949(rows)
	if len(got) != 1 {
		t.Fatalf("want 1 disposal, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Description != "NFT coll/42" || !r.Proceeds.Equal(d(3)) || !r.CostBasis.Equal(d(2)) ||
		!r.GainLoss.Equal(d(1)) || r.LongTerm {
		t.Fatalf("nft 8949 calc wrong: %+v", r)
	}
}

// An NFT sold without a recorded prior buy has unknown ("Various") basis.
func TestBuild8949NFTUnknownBasis(t *testing.T) {
	sell := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	rows := []Row{{Time: sell, Direction: "out", Category: "nft_sale", Asset: "coll/7", ValueUSD: d(5)}}
	got := Build8949(rows)
	if len(got) != 1 || got[0].DateAcquired != "Various" || !got[0].CostBasis.IsZero() || !got[0].Proceeds.Equal(d(5)) {
		t.Fatalf("nft unknown-basis wrong: %+v", got)
	}
}

func TestBuild8949LongTermAndUnknownBasis(t *testing.T) {
	buy := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sell := buy.AddDate(1, 0, 1) // >365d → long term
	rows := []Row{
		{Time: buy, Direction: "in", Category: "reward", Symbol: "ATOM", Amount: d(5), PriceUSD: d(2)},
		{Time: sell, Direction: "out", Category: "transfer", Symbol: "ATOM", Amount: d(8), PriceUSD: d(4)},
	}
	got := Build8949(rows)
	// 5 from the lot (long-term) + 3 with unknown basis
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d: %+v", len(got), got)
	}
	if !got[0].LongTerm || !got[0].CostBasis.Equal(d(10)) || !got[0].Proceeds.Equal(d(20)) {
		t.Fatalf("lot line wrong: %+v", got[0])
	}
	if got[1].DateAcquired != "Various" || !got[1].CostBasis.IsZero() || !got[1].Proceeds.Equal(d(12)) {
		t.Fatalf("unknown-basis line wrong: %+v", got[1])
	}
	if got[0].BasisUnknown {
		t.Fatalf("the reward-sourced lot IS a known acquisition, should not be flagged: %+v", got[0])
	}
	if !got[1].BasisUnknown {
		t.Fatalf("a disposal with no matching lot at all must be flagged BasisUnknown: %+v", got[1])
	}
	if CountUnknownBasis(got) != 1 {
		t.Fatalf("want 1 unknown-basis line, got %d", CountUnknownBasis(got))
	}
}

// A plain transfer-in (could be an exchange withdrawal, or IBC-in from an
// untracked wallet) establishes a lot we can't stand behind — its receipt-time
// price is not necessarily what the user actually paid (INF-205).
func TestBuild8949TransferInHasUnknownBasis(t *testing.T) {
	in := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := in.AddDate(0, 0, 5)
	rows := []Row{
		{Time: in, Direction: "in", Category: "transfer", Symbol: "ATOM", Amount: d(10), PriceUSD: d(2)},
		{Time: out, Direction: "out", Category: "transfer", Symbol: "ATOM", Amount: d(10), PriceUSD: d(3)},
	}
	got := Build8949(rows)
	if len(got) != 1 || !got[0].BasisUnknown {
		t.Fatalf("transfer-in lot should be flagged BasisUnknown: %+v", got)
	}
	if !got[0].CostBasis.IsZero() || !got[0].GainLoss.Equal(got[0].Proceeds) {
		t.Fatalf("unknown-basis disposal should not fabricate a cost basis: %+v", got[0])
	}
	if got[0].DateAcquired != "01/01/2026" {
		t.Fatalf("the acquisition date IS known (we saw the transfer), should not be blanked to Various: %+v", got[0])
	}
	if !strings.Contains(got[0].Description, "basis unknown") {
		t.Fatalf("description should carry the warning: %q", got[0].Description)
	}
}

// IBC-in is the other explicit "arrived from elsewhere" case named in INF-205.
// Swaps and NFT mints/buys are on-chain trades we priced ourselves, so those
// stay known-basis.
func TestBuild8949KnownVsUnknownBasisCategories(t *testing.T) {
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		category string
		unknown  bool
	}{
		{"reward", false},
		{"commission", false},
		{"swap", false},
		{"transfer", true},
		{"ibc_in", true},
	}
	for _, c := range cases {
		rows := []Row{
			{Time: base, Direction: "in", Category: c.category, Symbol: "XYZ", Amount: d(1), PriceUSD: d(5)},
			{Time: base.AddDate(0, 0, 1), Direction: "out", Category: "transfer", Symbol: "XYZ", Amount: d(1), PriceUSD: d(6)},
		}
		got := Build8949(rows)
		if len(got) != 1 || got[0].BasisUnknown != c.unknown {
			t.Fatalf("category %q: want BasisUnknown=%v, got %+v", c.category, c.unknown, got)
		}
	}
}

// A wallet whose only acquisitions are known on-chain events (rewards, swaps,
// NFT mints/buys) is "pure on-chain" (INF-205): CountUnknownBasis is 0 and the
// native 8949 is fully correct on its own, no aggregator needed.
func TestPureOnChainWalletHasNoUnknownBasis(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	rows := []Row{
		{Time: base, Direction: "in", Category: "reward", Symbol: "ATOM", Amount: d(10), PriceUSD: d(2)},
		{Time: base.AddDate(0, 0, 5), Direction: "in", Category: "commission", Symbol: "ATOM", Amount: d(5), PriceUSD: d(2)},
		{Time: base.AddDate(0, 0, 10), Direction: "out", Category: "transfer", Symbol: "ATOM", Amount: d(3), PriceUSD: d(4)},
	}
	got := Build8949(rows)
	if CountUnknownBasis(got) != 0 {
		t.Fatalf("pure on-chain wallet should have zero unknown-basis lines, got %d: %+v", CountUnknownBasis(got), got)
	}
}

// Write8949CSV must carry the Address column so a multi-address report (several
// single-wallet responses concatenated) reads as per-wallet lots, never a
// pooled basis (Rev. Proc. 2024-28, see INF-204).
func TestWrite8949CSVIncludesAddress(t *testing.T) {
	rows := []Form8949Row{
		{Address: "cosmos1abc", Description: "1 ATOM", DateAcquired: "01/01/2026", DateSold: "02/01/2026", Proceeds: d(3), CostBasis: d(2), GainLoss: d(1)},
	}
	var buf strings.Builder
	if err := Write8949CSV(&buf, rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "Address,Part,") {
		t.Fatalf("header should lead with Address: %q", out)
	}
	if !strings.Contains(out, "cosmos1abc,I (short-term)") {
		t.Fatalf("row should carry the address: %q", out)
	}
}
