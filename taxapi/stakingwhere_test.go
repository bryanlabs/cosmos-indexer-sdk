package taxapi

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded file must load: every month's categories add up to what was
// withdrawn under both rules, and the IBC chains add up to the IBC category.
func TestWhereFileLoads(t *testing.T) {
	f, err := loadWhere()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Months) == 0 {
		t.Fatal("no months")
	}
	if f.ExchangeLabels != "pending" {
		t.Fatalf("exchange labels %q: publishing them needs the review first", f.ExchangeLabels)
	}
}

func TestWhereParseRejectsMonthsThatDoNotAddUp(t *testing.T) {
	good := `{"categories":["restaked","liquid_staking","ibc","fees","other_modules","other_sends","left_in_wallet","not_followed"],
	"chains":["osmosis-1"],"months":[{"month":"2025-01","withdrawn":10,"records":1,"fifo":[1,0,2,0,0,3,4,0],"prorata":[2,0,1,0,0,3,4,0],"ibc_fifo":[2],"ibc_prorata":[1]}]}`
	if _, err := parseWhere([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"withdrawn":10`, `"withdrawn":11`, 1),
		strings.Replace(good, `"ibc_fifo":[2]`, `"ibc_fifo":[3]`, 1),
		strings.Replace(good, `"restaked","liquid_staking"`, `"liquid_staking","restaked"`, 1),
	} {
		if _, err := parseWhere([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestWhereRowsUnclaimedAndYearly(t *testing.T) {
	f := &whereFile{FirstDay: "2025-01-30", LastDay: "2025-02-01", UnclaimedOpen: 1_000_000, Chains: []string{"osmosis-1"},
		Categories: whereCategories, Months: []whereMonth{
			{Month: "2025-01", Withdrawn: 4_000_000, Records: 2, FIFO: []int64{1_000_000, 0, 2_000_000, 0, 0, 0, 1_000_000, 0}, ProRata: []int64{2_000_000, 0, 1_000_000, 0, 0, 0, 1_000_000, 0}, IBCFIFO: []int64{2_000_000}, IBCProRata: []int64{1_000_000}},
			{Month: "2025-02", Withdrawn: 1_000_000, Records: 1, FIFO: []int64{0, 0, 0, 0, 0, 0, 0, 1_000_000}, ProRata: []int64{0, 0, 0, 0, 0, 0, 0, 1_000_000}, IBCFIFO: []int64{0}, IBCProRata: []int64{0}},
		}}
	day := func(date string, rewards, commission int64) inflationDay {
		return inflationDay{Date: date, Minted: big.NewInt(rewards + 1), CommunityPool: big.NewInt(1), RewardsEvents: big.NewInt(rewards), CommissionEvents: big.NewInt(commission)}
	}
	infl := &inflationData{Days: []inflationDay{day("2025-01-29", 99, 0), day("2025-01-30", 3_000_000, 500_000), day("2025-01-31", 3_000_000, 500_000), day("2025-02-01", 2_000_000, 0)}}
	rows := whereRows(f, infl, "monthly")
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	jan := rows[0]
	if jan.StartDate != "2025-01-30" || jan.ToDelegators != "5.000000" || jan.Withdrawn != "4.000000" {
		t.Fatalf("january %+v", jan)
	}
	if jan.UnclaimedChange != "1.000000" || jan.UnclaimedEnd != "2.000000" {
		t.Fatalf("january unclaimed %s %s", jan.UnclaimedChange, jan.UnclaimedEnd)
	}
	if rows[1].UnclaimedEnd != "3.000000" {
		t.Fatalf("february unclaimed end %s", rows[1].UnclaimedEnd)
	}
	var restaked, exchange *whereHop
	for i := range jan.FirstHop {
		switch jan.FirstHop[i].Category {
		case "restaked":
			restaked = &jan.FirstHop[i]
		case "sent_to_labelled_exchange_address":
			exchange = &jan.FirstHop[i]
		}
	}
	if *restaked.LowAtom != "1.000000" || *restaked.HighAtom != "2.000000" || *restaked.ProRataShare != 0.5 {
		t.Fatalf("restaked %+v", restaked)
	}
	if exchange == nil || exchange.Status != "pending" || exchange.FIFOAtom != nil {
		t.Fatalf("exchange %+v", exchange)
	}
	if len(jan.IBCByChain) != 1 || jan.IBCByChain[0].FIFOAtom != "2.000000" {
		t.Fatalf("ibc %+v", jan.IBCByChain)
	}
	if rows[1].NotFollowedShare != 1 {
		t.Fatalf("not followed share %v", rows[1].NotFollowedShare)
	}
	yr := whereRows(f, infl, "yearly")
	if len(yr) != 1 || yr[0].Withdrawn != "5.000000" || yr[0].UnclaimedEnd != "3.000000" || yr[0].Withdrawals != 3 {
		t.Fatalf("yearly %+v", yr)
	}
}

func TestWhereAtomGoesRoute(t *testing.T) {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /staking/where-atom-goes", s.handleWhereAtomGoes)
	for _, tc := range []struct {
		q    string
		code int
	}{{"", 200}, {"?view=yearly", 200}, {"?view=daily", 400}, {"?from=2021-03&to=2021", 200}, {"?from=bad", 400}} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/staking/where-atom-goes"+tc.q, nil))
		if rec.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.q, rec.Code, rec.Body.String())
		}
		if tc.code != 200 {
			continue
		}
		var body struct {
			Coverage map[string]any    `json:"coverage"`
			Method   map[string]string `json:"method"`
			Rows     []whereRow        `json:"rows"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Rows) == 0 || body.Method["rules"] == "" || body.Coverage["wallets_not_followed"] == nil {
			t.Fatalf("%s: incomplete body", tc.q)
		}
		for _, r := range body.Rows {
			for _, h := range r.FirstHop {
				if strings.Contains(strings.ToLower(h.Category), "sold") {
					t.Fatalf("category %s", h.Category)
				}
			}
		}
	}
}
