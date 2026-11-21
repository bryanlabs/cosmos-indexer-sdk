package taxapi

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Per-validator commission in the embedded file must add back to the Hub-wide
// commission events the inflation view serves, within a uatom per validator.
func TestValidatorIncomeAddsUpToHubCommission(t *testing.T) {
	d, err := loadIncome()
	if err != nil {
		t.Fatal(err)
	}
	infl, err := loadInflation()
	if err != nil {
		t.Fatal(err)
	}
	byDate := map[string]inflationDay{}
	for _, day := range infl.Days {
		byDate[day.Date] = day
	}
	complete := d.File.BlocksCounted == d.File.BlocksExpected
	for _, p := range incomePeriods(d.Days, "monthly") {
		hub := new(big.Int)
		for i := p.lo; i < p.hi; i++ {
			hub.Add(hub, byDate[d.Days[i]].CommissionEvents)
		}
		var sum, rewards int64
		for _, v := range d.Validators {
			sum += v.commissionIn(p.lo, p.hi)
			rewards += v.Rewards[p.key]
		}
		if !complete && sum == 0 {
			continue
		}
		diff := new(big.Int).Sub(hub, big.NewInt(sum))
		if diff.CmpAbs(big.NewInt(int64(len(d.Validators)))) > 0 {
			t.Fatalf("%s: validators sum %d, hub %s", p.key, sum, hub)
		}
		if rewards <= 0 {
			t.Fatalf("%s: no rewards", p.key)
		}
	}
}

func TestIncomeRowGroupsAndPrices(t *testing.T) {
	d := &incomeData{
		File: incomeFile{Withdrawn: [][2]int64{{5_000_000, 7_000_000}, {0, 1_000_000}}, ExchangeRunRule: "test rule"},
		Days: []string{"2025-01-01", "2025-01-02"},
	}
	mk := func(c0, c1, rewards int64, ex bool) incomeValidator {
		v := incomeValidator{First: 0, Commission: []int64{c0, c1}, Rewards: map[string]int64{"2025-01": rewards}, ExchangeRun: ex}
		v.commissionCS = []int64{0, c0, c0 + c1}
		return v
	}
	for i := 0; i < 12; i++ {
		d.Validators = append(d.Validators, mk(int64(1_000_000*(i+1)), 0, int64(100*(i+1)), i == 11))
	}
	d.Validators = append(d.Validators, mk(0, 0, 0, false))
	infl := map[string]inflationDay{
		"2025-01-01": {CommissionEvents: big.NewInt(78_000_000), RewardsEvents: big.NewInt(780_000_000)},
		"2025-01-02": {CommissionEvents: big.NewInt(0), RewardsEvents: big.NewInt(0)},
	}
	prices := map[string]float64{"2025-01-01": 2, "2025-01-02": 3}
	row := incomeRowFor(d, incomePeriod{key: "2025-01", lo: 0, hi: 2}, infl, prices, true)
	if row.Validators != 12 || row.EarnedCommissionAtom != "78.000000" || *row.EarnedCommissionUSD != 156 {
		t.Fatalf("row %+v", row)
	}
	if row.WithdrawnCommissionAtom != "5.000000" || *row.WithdrawnCommissionUSD != 10 || row.WithdrawnRewardsAtom != "8.000000" {
		t.Fatalf("withdrawn %+v", row)
	}
	if row.PerValidatorAtom.Median != 6.5 || row.PerValidatorUSD.Median != 13 {
		t.Fatalf("spread %+v %+v", row.PerValidatorAtom, row.PerValidatorUSD)
	}
	n := 0
	for _, g := range row.ByStake {
		n += g.Validators
	}
	if n != 12 || row.ByStake[0].Validators != 10 || row.ByStake[0].MedianAtom != "7.500000" {
		t.Fatalf("bands %+v", row.ByStake)
	}
	if row.ExchangeRun == nil || row.ExchangeRun.Validators != 1 || row.ExchangeRun.CommissionAtom != "12.000000" {
		t.Fatalf("exchange-run %+v", row.ExchangeRun)
	}
	delete(prices, "2025-01-02")
	row = incomeRowFor(d, incomePeriod{key: "2025-01", lo: 0, hi: 2}, infl, prices, true)
	if row.EarnedCommissionUSD != nil || row.PerValidatorUSD != nil || row.PriceDaysMissing != 1 {
		t.Fatalf("a missing price day must drop dollars: %+v", row)
	}
}

func TestValidatorIncomeHandlers(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		path string
		code int
	}{
		{"/staking/validator-income", 200}, {"/staking/validator-income?view=yearly", 200},
		{"/staking/validator-income?view=daily", 400}, {"/staking/validator-income?from=21-3", 400},
		{"/staking/validator-income/break-even?atom_usd=10&cost_usd=500", 200},
		{"/staking/validator-income/break-even?atom_usd=10&inflation=2", 400},
		{"/staking/validator-income/break-even?month=1999-01&atom_usd=10", 400},
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code {
			t.Fatalf("%s: status %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/staking/validator-income/break-even?atom_usd=10&cost_usd=500", nil))
	var body struct {
		Validators int              `json:"validators"`
		Below      int              `json:"below_break_even"`
		Curve      []breakEvenPoint `json:"curve"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Validators == 0 || len(body.Curve) != 81 || body.Curve[0].Below > body.Validators {
		t.Fatalf("break-even %+v", body)
	}
	for i := 1; i < len(body.Curve); i++ {
		if body.Curve[i].Below > body.Curve[i-1].Below {
			t.Fatalf("more validators below break-even at higher inflation: %+v", body.Curve[i-1:i+1])
		}
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/staking/validator-income/break-even?atom_usd=10&cost_usd=0", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Below != 0 {
		t.Fatalf("nobody is below a zero cost, got %d", body.Below)
	}
}
