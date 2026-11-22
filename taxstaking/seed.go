package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"gorm.io/gorm"
)

// The seed loads the extraction's exact history: the merged daily file the
// embedded inflation view was exported from (merge.py) and the per-validator
// pass (run_units.py, extract_v.py --by-validator). It applies the checks
// export_v.py applies before it writes the embedded income file, and loads
// the day columns verbatim so the API serves the same numbers from the
// tables as from the files.

type mergedFile struct {
	Rows           []map[string]json.RawMessage `json:"rows"`
	FirstBlockTime string                       `json:"first_block_time"`
	Report         struct {
		FirstHeight     int64             `json:"first_height"`
		LastHeight      int64             `json:"last_height"`
		HeightsCovered  int64             `json:"heights_covered"`
		HeightsExpected int64             `json:"heights_expected"`
		Gaps            []json.RawMessage `json:"gaps"`
		Overlaps        []json.RawMessage `json:"overlaps"`
		ProblemCount    int               `json:"problem_count"`
	} `json:"report"`
}

type byvalDay struct {
	Blocks      int64                 `json:"blocks"`
	Rewards     string                `json:"rewards"`
	Commission  string                `json:"commission"`
	Validators  *map[string][2]string `json:"validators"`
	WithdrawnC  *string               `json:"withdrawn_commission"`
	WithdrawnR  *string               `json:"withdrawn_rewards"`
	UnbondByDay map[string]string     `json:"unbond_by_completion"`
}

type byvalUnit struct {
	Unit    string              `json:"unit"`
	Start   int64               `json:"start"`
	End     int64               `json:"end"`
	Days    map[string]byvalDay `json:"days"`
	Missing []json.RawMessage   `json:"missing"`
	NoMint  []json.RawMessage   `json:"no_mint"`
	Bad     []json.RawMessage   `json:"bad"`
}

type seedData struct {
	State  taxapi.StakingState
	Days   []taxapi.StakingDayRow
	Vals   []taxapi.StakingValidatorRow
	Unbond []unbondDayRow
}

func rawString(m map[string]json.RawMessage, k string) (string, error) {
	v, ok := m[k]
	if !ok {
		return "", fmt.Errorf("no %s", k)
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return "", fmt.Errorf("%s: %v", k, err)
	}
	return n.String(), nil
}

func rawInt(m map[string]json.RawMessage, k string) (string, error) {
	s, err := rawString(m, k)
	if err != nil {
		return "", err
	}
	if _, ok := new(big.Int).SetString(s, 10); !ok {
		return "", fmt.Errorf("%s: %q is not a whole number", k, s)
	}
	return s, nil
}

func rawDecimal(m map[string]json.RawMessage, k string) (string, error) {
	s, err := rawString(m, k)
	if err != nil {
		return "", err
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return "", fmt.Errorf("%s: %q is not a number", k, s)
	}
	return s, nil
}

func readMerged(path string) (*mergedFile, []taxapi.StakingDayRow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var m mergedFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("merged: %v", err)
	}
	rep := m.Report
	if len(rep.Gaps) > 0 || len(rep.Overlaps) > 0 || rep.ProblemCount != 0 || rep.HeightsCovered != rep.HeightsExpected ||
		rep.HeightsExpected != rep.LastHeight-rep.FirstHeight+1 {
		return nil, nil, fmt.Errorf("merged: incomplete (gaps %d overlaps %d problems %d covered %d expected %d)",
			len(rep.Gaps), len(rep.Overlaps), rep.ProblemCount, rep.HeightsCovered, rep.HeightsExpected)
	}
	if len(m.Rows) == 0 {
		return nil, nil, errors.New("merged: no rows")
	}
	var rows []taxapi.StakingDayRow
	var total int64
	for i, r := range m.Rows {
		var d taxapi.StakingDayRow
		var errs []error
		str := func(k string, f func(map[string]json.RawMessage, string) (string, error)) string {
			v, err := f(r, k)
			errs = append(errs, err)
			return v
		}
		d.Day = str("date", rawString)
		fh, lh, bl := str("first_height", rawInt), str("last_height", rawInt), str("blocks", rawInt)
		d.Minted = str("minted_uatom", rawInt)
		d.CommunityPool = str("community_pool_uatom", rawInt)
		d.Stakers = str("stakers_uatom", rawInt)
		d.Commission = str("commission_uatom", rawInt)
		d.RewardsEvents = str("rewards_events_uatom", rawInt)
		d.CommissionEvents = str("commission_events_uatom", rawInt)
		d.SupplyStart = str("supply_start_uatom", rawInt)
		d.InflationStart = str("inflation_param_start", rawDecimal)
		d.InflationAvg = str("inflation_param_avg", rawDecimal)
		d.BondedAvg = str("bonded_ratio_avg", rawDecimal)
		d.CommunityTax = str("community_tax_start", rawDecimal)
		if err := errors.Join(errs...); err != nil {
			return nil, nil, fmt.Errorf("merged row %d: %v", i, err)
		}
		fmt.Sscan(fh, &d.FirstHeight)
		fmt.Sscan(lh, &d.LastHeight)
		fmt.Sscan(bl, &d.Blocks)
		var props [][3]json.RawMessage
		if err := json.Unmarshal(r["proposals"], &props); err != nil || props == nil {
			return nil, nil, fmt.Errorf("merged row %s proposals: %v", d.Day, err)
		}
		var pj bytes.Buffer
		if err := json.Compact(&pj, r["proposals"]); err != nil {
			return nil, nil, fmt.Errorf("merged row %s proposals: %v", d.Day, err)
		}
		d.Proposals = pj.String()
		if _, err := time.Parse(time.DateOnly, d.Day); err != nil {
			return nil, nil, fmt.Errorf("merged row %d date %q", i, d.Day)
		}
		if d.Blocks != d.LastHeight-d.FirstHeight+1 || d.Blocks <= 0 {
			return nil, nil, fmt.Errorf("merged %s: %d blocks for heights %d-%d", d.Day, d.Blocks, d.FirstHeight, d.LastHeight)
		}
		if i == 0 && d.FirstHeight != rep.FirstHeight {
			return nil, nil, fmt.Errorf("merged %s starts at %d, report at %d", d.Day, d.FirstHeight, rep.FirstHeight)
		}
		if i > 0 {
			p := rows[i-1]
			pd, _ := time.Parse(time.DateOnly, p.Day)
			if d.Day != pd.AddDate(0, 0, 1).Format(time.DateOnly) || d.FirstHeight != p.LastHeight+1 {
				return nil, nil, fmt.Errorf("merged %s does not follow %s", d.Day, p.Day)
			}
		}
		total += d.Blocks
		rows = append(rows, d)
	}
	if rows[len(rows)-1].LastHeight != rep.LastHeight || total != rep.HeightsExpected {
		return nil, nil, errors.New("merged: day block counts do not add up to the height range")
	}
	if _, err := time.Parse(time.RFC3339, m.FirstBlockTime); err != nil {
		return nil, nil, fmt.Errorf("merged first_block_time %q", m.FirstBlockTime)
	}
	return &m, rows, nil
}

type byvalTotals struct {
	blocks     int64
	rewards    *big.Int
	commission *big.Int
	wc, wr     *big.Int
	unbond     map[string]*big.Int
	vals       map[string]*[2]*big.Int
}

// readByval sums the per-validator units by day, exactly, skipping a unit
// name seen before (two runs may meet in a range), as export_v.py does.
func readByval(paths []string) (map[string]*byvalTotals, [][2]int64, int, error) {
	names := map[string]bool{}
	days := map[string]*byvalTotals{}
	var seen [][2]int64
	problems := 0
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, 0, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<30)
		line := 0
		for sc.Scan() {
			line++
			var u byvalUnit
			if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
				f.Close()
				return nil, nil, 0, fmt.Errorf("%s line %d: %v", path, line, err)
			}
			if names[u.Unit] {
				continue
			}
			names[u.Unit] = true
			seen = append(seen, [2]int64{u.Start, u.End})
			if len(u.Missing) > 0 || len(u.NoMint) > 0 || len(u.Bad) > 0 {
				problems++
			}
			for k, d := range u.Days {
				if d.Validators == nil {
					f.Close()
					return nil, nil, 0, fmt.Errorf("%s unit %s day %s: no validators (not a per-validator pass)", path, u.Unit, k)
				}
				t := days[k]
				if t == nil {
					t = &byvalTotals{rewards: new(big.Int), commission: new(big.Int), wc: new(big.Int), wr: new(big.Int),
						unbond: map[string]*big.Int{}, vals: map[string]*[2]*big.Int{}}
					days[k] = t
				}
				add := func(dst *big.Int, s string) error {
					v, err := parseFix(s)
					if err != nil {
						return err
					}
					dst.Add(dst, v)
					return nil
				}
				var errs []error
				t.blocks += d.Blocks
				errs = append(errs, add(t.rewards, d.Rewards), add(t.commission, d.Commission))
				if d.WithdrawnC != nil {
					errs = append(errs, add(t.wc, *d.WithdrawnC))
				}
				if d.WithdrawnR != nil {
					errs = append(errs, add(t.wr, *d.WithdrawnR))
				}
				for ck, cv := range d.UnbondByDay {
					if t.unbond[ck] == nil {
						t.unbond[ck] = new(big.Int)
					}
					errs = append(errs, add(t.unbond[ck], cv))
				}
				for op, rc := range *d.Validators {
					x := t.vals[op]
					if x == nil {
						x = &[2]*big.Int{new(big.Int), new(big.Int)}
						t.vals[op] = x
					}
					errs = append(errs, add(x[0], rc[0]), add(x[1], rc[1]))
				}
				if err := errors.Join(errs...); err != nil {
					f.Close()
					return nil, nil, 0, fmt.Errorf("%s unit %s day %s: %v", path, u.Unit, k, err)
				}
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, nil, 0, err
		}
	}
	sort.Slice(seen, func(i, j int) bool {
		return seen[i][0] < seen[j][0] || seen[i][0] == seen[j][0] && seen[i][1] < seen[j][1]
	})
	return days, seen, problems, nil
}

// prepareSeed reads and checks both files and returns the rows to load.
func prepareSeed(chain, mergedPath string, byvalPaths []string) (*seedData, error) {
	m, rows, err := readMerged(mergedPath)
	if err != nil {
		return nil, err
	}
	days, seen, problems, err := readByval(byvalPaths)
	if err != nil {
		return nil, err
	}
	if len(seen) == 0 {
		return nil, errors.New("per-validator pass: no units")
	}
	var gaps, overlaps int
	var covered int64
	for i, s := range seen {
		covered += s[1] - s[0] + 1
		if i > 0 {
			p := seen[i-1]
			if s[0] > p[1]+1 {
				gaps++
			}
			if s[0] <= p[1] {
				overlaps++
			}
		}
	}
	rep := m.Report
	var bad []string
	byDay := map[string]taxapi.StakingDayRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	one := new(big.Int).Set(fixOne)
	within1 := func(fix *big.Int, whole string) bool {
		w, _ := new(big.Int).SetString(whole, 10)
		d := new(big.Int).Sub(fix, new(big.Int).Mul(w, fixOne))
		return d.Abs(d).Cmp(one) <= 0
	}
	for _, k := range keys {
		t := days[k]
		r, ok := byDay[k]
		sr, sc := new(big.Int), new(big.Int)
		for _, x := range t.vals {
			sr.Add(sr, x[0])
			sc.Add(sc, x[1])
		}
		if !ok || t.blocks != r.Blocks || sr.Cmp(t.rewards) != 0 || sc.Cmp(t.commission) != 0 ||
			!within1(t.rewards, r.RewardsEvents) || !within1(t.commission, r.CommissionEvents) {
			bad = append(bad, k)
		}
	}
	complete := gaps == 0 && overlaps == 0 && problems == 0 && len(bad) == 0 && covered == rep.HeightsExpected &&
		seen[0][0] == rep.FirstHeight && seen[len(seen)-1][1] == rep.LastHeight && len(days) == len(rows)
	if !complete {
		first := ""
		if len(bad) > 0 {
			first = " first bad day " + bad[0]
		}
		return nil, fmt.Errorf("per-validator pass does not cover the merged file exactly: units %d covered %d of %d, gaps %d, overlaps %d, units with problems %d, days %d of %d, bad days %d%s",
			len(seen), covered, rep.HeightsExpected, gaps, overlaps, problems, len(days), len(rows), len(bad), first)
	}
	sd := &seedData{Days: rows}
	for i := range sd.Days {
		t := days[sd.Days[i].Day]
		sd.Days[i].WithdrawnCommission = fixString(t.wc)
		sd.Days[i].WithdrawnRewards = fixString(t.wr)
		ops := make([]string, 0, len(t.vals))
		for op := range t.vals {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			x := t.vals[op]
			sd.Vals = append(sd.Vals, taxapi.StakingValidatorRow{Day: sd.Days[i].Day, Operator: op, Rewards: fixString(x[0]), Commission: fixString(x[1])})
		}
		cks := make([]string, 0, len(t.unbond))
		for ck := range t.unbond {
			if _, err := time.Parse(time.DateOnly, ck); err != nil {
				return nil, fmt.Errorf("day %s: unbonding without a readable completion date (%q)", sd.Days[i].Day, trim(ck))
			}
			cks = append(cks, ck)
		}
		sort.Strings(cks)
		for _, ck := range cks {
			sd.Unbond = append(sd.Unbond, unbondDayRow{Day: sd.Days[i].Day, unbondRow: unbondRow{CompletionDay: ck, Amount: fixString(t.unbond[ck])}})
		}
	}
	fbt, _ := time.Parse(time.RFC3339, m.FirstBlockTime)
	last := rows[len(rows)-1]
	sd.State = taxapi.StakingState{Chain: chain, FirstDay: rows[0].Day, FirstHeight: rows[0].FirstHeight, FirstBlockTime: fbt.UTC(),
		LastDay: last.Day, LastHeight: last.LastHeight, Source: "seed", Version: taxapi.StakingTablesVersion}
	return sd, nil
}

// loadSeed writes the whole history in one transaction, only into empty tables.
func loadSeed(db *gorm.DB, sd *seedData) error {
	chain := sd.State.Chain
	return writeTx(db, func(tx *gorm.DB) error {
		var n int64
		if err := tx.Raw(`SELECT (SELECT count(*) FROM tax_staking_state WHERE chain = ?)
 + (SELECT count(*) FROM (SELECT 1 FROM tax_staking_days WHERE chain = ? LIMIT 1) d)
 + (SELECT count(*) FROM (SELECT 1 FROM tax_staking_validator_days WHERE chain = ? LIMIT 1) v)
 + (SELECT count(*) FROM (SELECT 1 FROM tax_staking_unbonding WHERE chain = ? LIMIT 1) u)`, chain, chain, chain, chain).Row().Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("refusing seed: %s already has staking rows (undo first, see the runbook)", chain)
		}
		if err := insertDays(tx, chain, sd.Days); err != nil {
			return err
		}
		if err := insertValidators(tx, chain, sd.Vals); err != nil {
			return err
		}
		if err := insertUnbonding(tx, chain, sd.Unbond); err != nil {
			return err
		}
		st := sd.State
		return tx.Exec(`INSERT INTO tax_staking_state (chain, first_day, first_height, first_block_time, last_day, last_height,
 updated_at, source, version) VALUES (?, ?::date, ?, ?, ?::date, ?, ?, ?, ?)`, chain, st.FirstDay, st.FirstHeight,
			st.FirstBlockTime, st.LastDay, st.LastHeight, time.Now().UTC(), st.Source, st.Version).Error
	})
}
