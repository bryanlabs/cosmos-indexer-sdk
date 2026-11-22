package taxapi

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded file must cover every block it names, once, with every day's
// split adding back to what was minted.
func TestInflationFileIsComplete(t *testing.T) {
	d, err := loadInflation()
	if err != nil {
		t.Fatal(err)
	}
	if d.BlocksCounted != d.BlocksExpected || d.BlocksExpected != d.LastHeight-d.FirstHeight+1 {
		t.Fatalf("counted %d of %d blocks", d.BlocksCounted, d.BlocksExpected)
	}
	var blocks int64
	for i, day := range d.Days {
		blocks += day.Blocks
		if day.LastHeight-day.FirstHeight+1 != day.Blocks {
			t.Fatalf("%s: %d blocks for heights %d to %d", day.Date, day.Blocks, day.FirstHeight, day.LastHeight)
		}
		if i > 0 && day.FirstHeight != d.Days[i-1].LastHeight+1 {
			t.Fatalf("%s does not start right after %s", day.Date, d.Days[i-1].Date)
		}
		split := new(big.Int).Add(day.CommunityPool, day.Stakers)
		if split.Cmp(day.Minted) != 0 {
			t.Fatalf("%s: pool %s plus stakers %s is not minted %s", day.Date, day.CommunityPool, day.Stakers, day.Minted)
		}
		if day.Commission.Cmp(day.Stakers) > 0 {
			t.Fatalf("%s: commission above stakers' share", day.Date)
		}
	}
	if blocks != d.BlocksCounted {
		t.Fatalf("days hold %d blocks, file says %d", blocks, d.BlocksCounted)
	}
}

func TestInflationMonthlyAddsUp(t *testing.T) {
	d, err := loadInflation()
	if err != nil {
		t.Fatal(err)
	}
	rows := groupInflation(d.Days, "monthly")
	total, want := new(big.Rat), new(big.Rat)
	for _, r := range rows {
		v, _ := new(big.Rat).SetString(r.Minted)
		total.Add(total, v)
	}
	for _, day := range d.Days {
		want.Add(want, new(big.Rat).SetFrac(day.Minted, big.NewInt(1_000_000)))
	}
	if total.Cmp(want) != 0 {
		t.Fatalf("monthly minted %s, days %s", total.FloatString(6), want.FloatString(6))
	}
	for _, r := range rows {
		if r.AprPaid <= 0 || r.BondedRatio <= 0 || r.BondedRatio >= 1 || r.InflationActual <= 0 {
			t.Fatalf("%s: implausible rates %+v", r.Period, r)
		}
		if r.AprAfterCommission > r.AprPaid || r.AprReal >= r.AprPaid {
			t.Fatalf("%s: after-commission or real APR above paid", r.Period)
		}
	}
}

func TestSummarizeArithmetic(t *testing.T) {
	day := func(date string, minted, pool, supply int64) inflationDay {
		return inflationDay{Date: date, FirstHeight: 1, LastHeight: 10, Blocks: 10,
			Minted: big.NewInt(minted), CommunityPool: big.NewInt(pool), Stakers: big.NewInt(minted - pool),
			Commission: big.NewInt((minted - pool) / 10), RewardsEvents: big.NewInt(minted - pool),
			CommissionEvents: big.NewInt((minted - pool) / 10), SupplyStart: big.NewInt(supply),
			InflationAvg: 0.1, BondedAvg: 0.5, CommunityTax: "0.02", Seconds: 86400}
	}
	days := []inflationDay{day("2025-01-01", 1_000_000, 20_000, 365_000_000)}
	next := day("2025-01-02", 0, 0, 366_000_000)
	r := summarize("2025-01-01", days, &next)
	if r.Minted != "1.000000" || r.ToCommunityPool != "0.020000" || r.ToDelegators != "0.882000" {
		t.Fatalf("split %+v", r)
	}
	if r.OtherSupplyChange != "0.000000" {
		t.Fatalf("other supply change %s", r.OtherSupplyChange)
	}
	if r.InflationActual != 1 {
		t.Fatalf("actual inflation %v, want 1 (365 times a day's 1/365)", r.InflationActual)
	}
	if r.AprStated != round6(0.1*0.98/0.5) {
		t.Fatalf("stated APR %v", r.AprStated)
	}
	if r.AprPaid != round6(0.98/0.5) || r.AprAfterCommission != round6(0.98/0.5*0.9) {
		t.Fatalf("paid %v after commission %v", r.AprPaid, r.AprAfterCommission)
	}
}

func TestStakingInflationHandler(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		query string
		code  int
	}{{"", 200}, {"?view=yearly", 200}, {"?view=daily&from=2026-11-06&to=2026-11-07", 200}, {"?view=weekly", 400}, {"?from=26-1", 400}} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/staking/inflation"+c.query, nil))
		if rec.Code != c.code {
			t.Fatalf("%s: status %d", c.query, rec.Code)
		}
		if c.code != 200 {
			continue
		}
		var body struct {
			Coverage map[string]any    `json:"coverage"`
			Changes  []inflationChange `json:"changes"`
			Rows     []inflationRow    `json:"rows"`
			Method   map[string]string `json:"method"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Coverage["complete"] != true || len(body.Changes) != 5 || len(body.Rows) == 0 || body.Method["apr_real"] == "" {
			t.Fatalf("%s: body %s", c.query, rec.Body.String()[:200])
		}
	}
}

// Rounding remainders that exceed a period's fees show as zero fees, never negative.
func TestFeesToStakersNeverNegative(t *testing.T) {
	if got := feesToStakers(big.NewInt(999_815), big.NewInt(1_000_000)); got.Sign() != 0 {
		t.Fatalf("got %s, want 0", got)
	}
	if got := feesToStakers(big.NewInt(1_000_250), big.NewInt(1_000_000)); got.Int64() != 250 {
		t.Fatalf("got %s, want 250", got)
	}
	d, err := loadInflation()
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"daily", "monthly", "yearly"} {
		for _, r := range groupInflation(d.Days, view) {
			if strings.HasPrefix(r.FeeShare, "-") {
				t.Fatalf("%s %s: fees_to_stakers %s", view, r.Period, r.FeeShare)
			}
		}
	}
}
