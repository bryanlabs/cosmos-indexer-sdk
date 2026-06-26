package taxapi

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(i int64) decimal.Decimal { return decimal.NewFromInt(i) }

func TestBuild8949FIFOGain(t *testing.T) {
	day1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	day10 := day1.AddDate(0, 0, 10)
	rows := []Row{
		{Time: day1, Direction: "in", Category: "transfer", Symbol: "ATOM", Amount: d(10), PriceUSD: d(2)},
		{Time: day10, Direction: "out", Category: "transfer", Symbol: "ATOM", Amount: d(4), PriceUSD: d(3)},
	}
	got := Build8949(rows)
	if len(got) != 1 {
		t.Fatalf("want 1 disposal, got %d: %+v", len(got), got)
	}
	r := got[0]
	if !r.Proceeds.Equal(d(12)) || !r.CostBasis.Equal(d(8)) || !r.GainLoss.Equal(d(4)) || r.LongTerm {
		t.Fatalf("wrong 8949 calc: %+v", r)
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
}
