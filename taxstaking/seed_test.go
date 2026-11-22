package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"github.com/stretchr/testify/require"
)

const fixtureMerged = `{"first_block_time":"2026-01-01T00:00:05Z","report":{"first_height":1000,"last_height":1011,"heights_covered":12,"heights_expected":12,"gaps":[],"overlaps":[],"problem_count":0},
"rows":[
 {"date":"2026-01-01","first_height":1000,"last_height":1005,"blocks":6,"minted_uatom":12015,"community_pool_uatom":240,"stakers_uatom":11775,"commission_uatom":100,"rewards_events_uatom":16,"commission_events_uatom":0,"supply_start_uatom":3650000,"inflation_param_start":"0.100000000000000000","inflation_param_avg":0.1,"bonded_ratio_avg":0.6352234,"community_tax_start":"0.02","proposals":[]},
 {"date":"2026-01-02","first_height":1006,"last_height":1011,"blocks":6,"minted_uatom":12051,"community_pool_uatom":241,"stakers_uatom":11810,"commission_uatom":101,"rewards_events_uatom":7,"commission_events_uatom":1,"supply_start_uatom":3650120,"inflation_param_start":"0.100000000000000000","inflation_param_avg":1e-05,"bonded_ratio_avg":0.6,"community_tax_start":"0.10","proposals":[[1009,"88","proposal_passed"]]}]}`

func byvalLine(unit string, start, end int64, days map[string]any) string {
	b, _ := json.Marshal(map[string]any{"unit": unit, "start": start, "end": end, "days": days, "missing": []int{}, "no_mint": []int{}, "bad": []any{}})
	return string(b)
}

func byvalDayJSON(blocks int, rewards, commission string, vals map[string][2]string, wc, wr string, unb map[string]string) map[string]any {
	return map[string]any{"blocks": blocks, "rewards": rewards, "commission": commission, "validators": vals,
		"withdrawn_commission": wc, "withdrawn_rewards": wr, "unbond_by_completion": unb}
}

func writeSeedFixture(t *testing.T, lines ...string) (string, string) {
	dir := t.TempDir()
	m := filepath.Join(dir, "merged.json")
	b := filepath.Join(dir, "byval.jsonl")
	require.NoError(t, os.WriteFile(m, []byte(fixtureMerged), 0o600))
	require.NoError(t, os.WriteFile(b, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return m, b
}

// Two units meeting inside day two, the second listed twice (a rerun).
func goodByval() []string {
	u1 := byvalLine("cache:1000-1007", 1000, 1007, map[string]any{
		"2026-01-01": byvalDayJSON(6, "16.5", "0.6", map[string][2]string{"valA": {"9", "0.6"}, "valB": {"7.5", "0"}}, "0", "5.000000000000000001", map[string]string{"2026-01-22": "10"}),
		"2026-01-02": byvalDayJSON(2, "3", "0.5", map[string][2]string{"valA": {"3", "0.5"}}, "1.5", "0", map[string]string{}),
	})
	u2 := byvalLine("cache:1008-1011", 1008, 1011, map[string]any{
		"2026-01-02": byvalDayJSON(4, "4.25", "0.75", map[string][2]string{"valA": {"4", "0.75"}, "valC": {"0.25", "0"}}, "2", "0", map[string]string{"2026-01-23": "3.5"}),
	})
	return []string{u1, u2, u2}
}

func TestPrepareSeed(t *testing.T) {
	m, b := writeSeedFixture(t, goodByval()...)
	sd, err := prepareSeed(fakeChain, m, []string{b})
	require.NoError(t, err)
	require.Equal(t, "2026-01-01", sd.State.FirstDay)
	require.Equal(t, "2026-01-02", sd.State.LastDay)
	require.Equal(t, int64(1011), sd.State.LastHeight)
	require.Equal(t, "2026-01-01T00:00:05Z", sd.State.FirstBlockTime.Format("2006-01-02T15:04:05Z"))
	require.Len(t, sd.Days, 2)
	d := sd.Days[1]
	require.Equal(t, "1e-05", d.InflationAvg, "merged values load verbatim")
	require.Equal(t, "0.10", d.CommunityTax)
	require.Equal(t, `[[1009,"88","proposal_passed"]]`, d.Proposals)
	require.Equal(t, "3.5", d.WithdrawnCommission)
	require.Equal(t, "5.000000000000000001", sd.Days[0].WithdrawnRewards)
	require.Equal(t, []taxapi.StakingValidatorRow{
		{Day: "2026-01-01", Operator: "valA", Rewards: "9", Commission: "0.6"},
		{Day: "2026-01-01", Operator: "valB", Rewards: "7.5", Commission: "0"},
		{Day: "2026-01-02", Operator: "valA", Rewards: "7", Commission: "1.25"},
		{Day: "2026-01-02", Operator: "valC", Rewards: "0.25", Commission: "0"},
	}, sd.Vals)
	require.Len(t, sd.Unbond, 2)
}

func TestPrepareSeedRefuses(t *testing.T) {
	g := goodByval()
	cases := map[string][]string{
		"missing unit":                  {g[0]},
		"validators do not add up":      {g[0], strings.Replace(g[1], `"valC":["0.25","0"]`, `"valC":["0.5","0"]`, 1)},
		"rewards more than 1 uatom off": {strings.ReplaceAll(strings.ReplaceAll(g[0], `"16.5"`, `"18"`), `"valB":["7.5","0"]`, `"valB":["9","0"]`), g[1]},
		"blocks differ":                 {strings.Replace(g[0], `"blocks":2`, `"blocks":3`, 1), g[1]},
		"unit with a problem":           {strings.Replace(g[0], `"missing":[]`, `"missing":[1003]`, 1), g[1]},
		"unknown completion":            {strings.Replace(g[0], `"2026-01-22"`, `"?"`, 1), g[1]},
		"not per validator":             {strings.Replace(g[0], `"validators":{"valA":["3","0.5"]}`, `"validators":null`, 1), g[1]},
	}
	for name, lines := range cases {
		m, b := writeSeedFixture(t, lines...)
		_, err := prepareSeed(fakeChain, m, []string{b})
		require.Error(t, err, name)
		t.Logf("%s: %v", name, err)
	}
	// An incomplete merged file is refused before anything else.
	dir := t.TempDir()
	bad := filepath.Join(dir, "m.json")
	require.NoError(t, os.WriteFile(bad, []byte(strings.Replace(fixtureMerged, `"problem_count":0`, `"problem_count":1`, 1)), 0o600))
	_, err := prepareSeed(fakeChain, bad, []string{filepath.Join(dir, "none")})
	require.ErrorContains(t, err, "merged: incomplete")
}

func TestLoadSeed(t *testing.T) {
	db := testDB(t)
	require.NoError(t, db.Exec(`DELETE FROM tax_staking_state`).Error)
	m, b := writeSeedFixture(t, goodByval()...)
	sd, err := prepareSeed(fakeChain, m, []string{b})
	require.NoError(t, err)
	require.NoError(t, loadSeed(db, sd))
	st, ok, err := taxapi.ReadStakingState(db, fakeChain)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "seed", st.Source)
	require.Equal(t, int64(1011), st.LastHeight)
	days, err := taxapi.ReadStakingDays(db, fakeChain, "2026-01-01", "2026-01-02")
	require.NoError(t, err)
	require.Equal(t, sd.Days[0].BondedAvg, days[0].BondedAvg)
	require.Equal(t, "0.00001", days[1].InflationAvg)
	require.Equal(t, "0.10", days[1].CommunityTax)
	vals, err := taxapi.ReadStakingValidatorDays(db, fakeChain, "2026-01-01", "2026-01-02")
	require.NoError(t, err)
	require.Equal(t, sd.Vals, vals)
	// A second seed is refused and changes nothing.
	require.ErrorContains(t, loadSeed(db, sd), "already has staking rows")
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM tax_staking_validator_days`).Row().Scan(&n))
	require.Equal(t, int64(4), n)
}

// With the real merged file (STAKING_MERGED_JSON), the rows the seed loads
// are the rows of the inflation file the API embeds.
func TestSeedRowsMatchEmbeddedInflation(t *testing.T) {
	path := os.Getenv("STAKING_MERGED_JSON")
	if path == "" {
		t.Skip("set STAKING_MERGED_JSON to the extraction's merged.json")
	}
	_, rows, err := readMerged(path)
	require.NoError(t, err)
	raw, err := os.ReadFile("../taxapi/stakingdata/inflation_daily.json")
	require.NoError(t, err)
	var emb struct {
		Columns []string            `json:"columns"`
		Days    [][]json.RawMessage `json:"days"`
	}
	require.NoError(t, json.Unmarshal(raw, &emb))
	require.Len(t, rows, len(emb.Days))
	for i, r := range rows {
		got := []string{r.Day, fmt.Sprint(r.FirstHeight), fmt.Sprint(r.LastHeight), fmt.Sprint(r.Blocks), r.Minted, r.CommunityPool,
			r.Stakers, r.Commission, r.RewardsEvents, r.CommissionEvents, r.SupplyStart, r.InflationAvg, r.BondedAvg, r.CommunityTax}
		for j, cell := range emb.Days[i] {
			var s string
			if json.Unmarshal(cell, &s) != nil {
				s = string(cell)
			}
			require.Equal(t, s, got[j], "%s %s", r.Day, emb.Columns[j])
		}
	}
}
