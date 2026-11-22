package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"reflect"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"gorm.io/gorm"
)

type tableDays struct {
	days   []taxapi.StakingDayRow
	vals   map[string]map[string][2]string
	unbond map[string]map[string]string
}

func readTableDays(db *gorm.DB, chain, from, to string) (*tableDays, error) {
	out := &tableDays{vals: map[string]map[string][2]string{}, unbond: map[string]map[string]string{}}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET TRANSACTION READ ONLY`).Error; err != nil {
			return err
		}
		var err error
		if out.days, err = taxapi.ReadStakingDays(tx, chain, from, to); err != nil {
			return err
		}
		vals, err := taxapi.ReadStakingValidatorDays(tx, chain, from, to)
		if err != nil {
			return err
		}
		for _, v := range vals {
			if out.vals[v.Day] == nil {
				out.vals[v.Day] = map[string][2]string{}
			}
			out.vals[v.Day][v.Operator] = [2]string{v.Rewards, v.Commission}
		}
		rows, err := tx.Raw(`SELECT day::text, completion_day::text, amount_uatom::text FROM tax_staking_unbonding
 WHERE chain = ? AND day BETWEEN ?::date AND ?::date`, chain, from, to).Rows()
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d, c, a string
			if err := rows.Scan(&d, &c, &a); err != nil {
				return err
			}
			if out.unbond[d] == nil {
				out.unbond[d] = map[string]string{}
			}
			out.unbond[d][c] = a
		}
		return rows.Err()
	})
	return out, err
}

func numEqual(a, b string) bool {
	x, ok1 := new(big.Rat).SetString(a)
	y, ok2 := new(big.Rat).SetString(b)
	return ok1 && ok2 && x.Cmp(y) == 0
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// compareDay lists every difference between a table day and the node's.
func compareDay(t taxapi.StakingDayRow, tv map[string][2]string, tu map[string]string, c computedDay) []string {
	var diffs []string
	n := c.Row
	ints := func(name string, a, b int64) {
		if a != b {
			diffs = append(diffs, fmt.Sprintf("%s table=%d node=%d", name, a, b))
		}
	}
	ints("first_height", t.FirstHeight, n.FirstHeight)
	ints("last_height", t.LastHeight, n.LastHeight)
	ints("blocks", t.Blocks, n.Blocks)
	for _, f := range []struct{ name, a, b string }{
		{"minted_uatom", t.Minted, n.Minted}, {"community_pool_uatom", t.CommunityPool, n.CommunityPool},
		{"stakers_uatom", t.Stakers, n.Stakers}, {"commission_uatom", t.Commission, n.Commission},
		{"rewards_events_uatom", t.RewardsEvents, n.RewardsEvents}, {"commission_events_uatom", t.CommissionEvents, n.CommissionEvents},
		{"supply_start_uatom", t.SupplyStart, n.SupplyStart}, {"inflation_param_start", t.InflationStart, n.InflationStart},
		{"inflation_param_avg", t.InflationAvg, n.InflationAvg}, {"bonded_ratio_avg", t.BondedAvg, n.BondedAvg},
		{"community_tax_start", t.CommunityTax, n.CommunityTax},
		{"withdrawn_commission_uatom", t.WithdrawnCommission, n.WithdrawnCommission},
		{"withdrawn_rewards_uatom", t.WithdrawnRewards, n.WithdrawnRewards},
	} {
		if !numEqual(f.a, f.b) {
			diffs = append(diffs, fmt.Sprintf("%s table=%s node=%s", f.name, f.a, f.b))
		}
	}
	if !jsonEqual(t.Proposals, n.Proposals) {
		diffs = append(diffs, fmt.Sprintf("proposals table=%s node=%s", t.Proposals, n.Proposals))
	}
	nv := map[string][2]string{}
	for _, v := range c.Vals {
		nv[v.Operator] = [2]string{v.Rewards, v.Commission}
	}
	vd := 0
	for op, a := range tv {
		b, ok := nv[op]
		if !ok || !numEqual(a[0], b[0]) || !numEqual(a[1], b[1]) {
			vd++
		}
	}
	for op := range nv {
		if _, ok := tv[op]; !ok {
			vd++
		}
	}
	if vd > 0 {
		diffs = append(diffs, fmt.Sprintf("validators differ=%d table=%d node=%d", vd, len(tv), len(nv)))
	}
	nu := map[string]string{}
	for _, u := range c.Unbond {
		nu[u.CompletionDay] = u.Amount
	}
	ud := 0
	for k, a := range tu {
		if b, ok := nu[k]; !ok || !numEqual(a, b) {
			ud++
		}
	}
	for k := range nu {
		if _, ok := tu[k]; !ok {
			ud++
		}
	}
	if ud > 0 {
		diffs = append(diffs, fmt.Sprintf("unbonding differ=%d table=%d node=%d", ud, len(tu), len(nu)))
	}
	return diffs
}

func runCheck(ctx context.Context, n *node, chain string, workers int, args []string) int {
	from, to, err := dayArgs(args)
	if err != nil {
		log.Printf("staking-check error: %v", err)
		return 2
	}
	db, closeDB, err := connect()
	if err != nil {
		log.Printf("staking-check error: db: %v", err)
		return 1
	}
	defer closeDB()
	td, err := readTableDays(db, chain, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if err != nil {
		log.Printf("staking-check error: read tables: %v", err)
		return 1
	}
	want := int(to.Sub(from).Hours()/24) + 1
	if len(td.days) != want {
		log.Printf("staking-check error: tables hold %d of the %d days %s..%s", len(td.days), want, from.Format(time.DateOnly), to.Format(time.DateOnly))
		return 1
	}
	days, err := nodeDays(ctx, n, chain, from, to)
	if err != nil {
		log.Printf("staking-check error: %v", err)
		return 1
	}
	cs, err := computeDays(ctx, n, days, workers, true)
	if err != nil {
		log.Printf("staking-check error: %v", err)
		return 1
	}
	bad := 0
	for i, t := range td.days {
		diffs := compareDay(t, td.vals[t.Day], td.unbond[t.Day], cs[i])
		if t.Day != cs[i].Range.Day {
			diffs = append([]string{"day order " + cs[i].Range.Day}, diffs...)
		}
		for _, d := range diffs {
			log.Printf("staking-check differ day=%s %s", t.Day, d)
		}
		if len(diffs) > 0 {
			bad++
		}
	}
	if bad > 0 {
		log.Printf("staking-check error: %d of %d days differ", bad, len(td.days))
		return 1
	}
	log.Printf("staking-check ok chain=%s from=%s to=%s days=%d", chain, from.Format(time.DateOnly), to.Format(time.DateOnly), len(td.days))
	return 0
}
