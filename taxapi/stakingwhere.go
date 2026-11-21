package taxapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"
)

// GET /staking/where-atom-goes: what happened to the staking rewards
// delegators withdrew on cosmoshub-4, month by month (INF-748 option 1). Each
// withdrawn reward is followed to its first hop out of the wallet that
// received it, under two stated rules (first in first out, and pro-rata);
// the gap between the rules is the uncertainty. Rewards in wallets whose
// replayed balance does not match the chain are counted as not followed.
// Issuance and the split to delegators come from the inflation data. The file
// is produced by a replay of every liquid ATOM movement from the cosmoshub-4
// genesis and embedded until the daily job writes it to Postgres.

//go:embed stakingdata/where_atom_goes.json
var whereAtomGoesJSON []byte

// whereCategories is the order of the per-month category arrays in the file.
var whereCategories = []string{"restaked", "liquid_staking", "ibc", "fees", "other_modules", "other_sends", "left_in_wallet", "not_followed"}

type whereMonth struct {
	Month      string  `json:"month"`
	Withdrawn  int64   `json:"withdrawn"`
	Records    int64   `json:"records"`
	FIFO       []int64 `json:"fifo"`
	ProRata    []int64 `json:"prorata"`
	IBCFIFO    []int64 `json:"ibc_fifo"`
	IBCProRata []int64 `json:"ibc_prorata"`
}

type whereFile struct {
	Chain              string       `json:"chain"`
	AsOf               string       `json:"as_of"`
	FirstHeight        int64        `json:"first_height"`
	LastHeight         int64        `json:"last_height"`
	FirstDay           string       `json:"first_day"`
	LastDay            string       `json:"last_day"`
	BlocksCounted      int64        `json:"blocks_counted"`
	BlocksExpected     int64        `json:"blocks_expected"`
	CheckHeight        int64        `json:"check_height"`
	WalletsRewarded    int64        `json:"wallets_rewarded"`
	WalletsNotFollowed int64        `json:"wallets_not_followed"`
	WalletsChecked     int64        `json:"wallets_checked"`
	UnclaimedOpen      int64        `json:"unclaimed_open_uatom"`
	UnclaimedCloseNode int64        `json:"unclaimed_close_uatom"`
	ExchangeLabels     string       `json:"exchange_labels"`
	LiquidStakingRule  string       `json:"liquid_staking_rule"`
	Categories         []string     `json:"categories"`
	Chains             []string     `json:"chains"`
	Months             []whereMonth `json:"months"`
}

var (
	whereOnce sync.Once
	whereSet  *whereFile
	whereErr  error
)

func loadWhere() (*whereFile, error) {
	whereOnce.Do(func() { whereSet, whereErr = parseWhere(whereAtomGoesJSON) })
	return whereSet, whereErr
}

func parseWhere(raw []byte) (*whereFile, error) {
	var f whereFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if len(f.Categories) != len(whereCategories) {
		return nil, fmt.Errorf("where file has %d categories, want %d", len(f.Categories), len(whereCategories))
	}
	for i, c := range whereCategories {
		if f.Categories[i] != c {
			return nil, fmt.Errorf("where file category %d is %s, want %s", i, f.Categories[i], c)
		}
	}
	prev := ""
	for _, m := range f.Months {
		if _, err := time.Parse("2006-01", m.Month); err != nil || m.Month <= prev {
			return nil, fmt.Errorf("where file month %q out of order", m.Month)
		}
		prev = m.Month
		if len(m.FIFO) != len(whereCategories) || len(m.ProRata) != len(whereCategories) {
			return nil, fmt.Errorf("%s: category arrays have the wrong length", m.Month)
		}
		if len(m.IBCFIFO) != len(f.Chains) || len(m.IBCProRata) != len(f.Chains) {
			return nil, fmt.Errorf("%s: chain arrays have the wrong length", m.Month)
		}
		// every withdrawn uatom lands in exactly one category under each rule
		var sf, sp, cf, cp int64
		for i := range whereCategories {
			sf += m.FIFO[i]
			sp += m.ProRata[i]
		}
		for i := range f.Chains {
			cf += m.IBCFIFO[i]
			cp += m.IBCProRata[i]
		}
		if sf != m.Withdrawn || sp != m.Withdrawn {
			return nil, fmt.Errorf("%s: categories sum to %d and %d, withdrawn %d", m.Month, sf, sp, m.Withdrawn)
		}
		if cf != m.FIFO[2] || cp != m.ProRata[2] {
			return nil, fmt.Errorf("%s: chains sum to %d and %d, ibc %d and %d", m.Month, cf, cp, m.FIFO[2], m.ProRata[2])
		}
	}
	return &f, nil
}

type whereHop struct {
	Category      string   `json:"category"`
	FIFOAtom      *string  `json:"fifo_atom"`
	ProRataAtom   *string  `json:"pro_rata_atom"`
	LowAtom       *string  `json:"low_atom"`
	HighAtom      *string  `json:"high_atom"`
	FIFOShare     *float64 `json:"fifo_share"`
	ProRataShare  *float64 `json:"pro_rata_share"`
	Status        string   `json:"status,omitempty"`
	StatusDetails string   `json:"status_details,omitempty"`
}

type whereChain struct {
	Chain       string `json:"chain"`
	FIFOAtom    string `json:"fifo_atom"`
	ProRataAtom string `json:"pro_rata_atom"`
}

type whereRow struct {
	Period            string       `json:"period"`
	StartDate         string       `json:"start_date"`
	EndDate           string       `json:"end_date"`
	Minted            string       `json:"minted_atom"`
	ToCommunityPool   string       `json:"to_community_pool_atom"`
	ToCommission      string       `json:"to_validator_commission_atom"`
	ToDelegators      string       `json:"to_delegators_atom"`
	Withdrawn         string       `json:"withdrawn_delegator_rewards_atom"`
	Withdrawals       int64        `json:"withdrawals"`
	UnclaimedChange   string       `json:"unclaimed_change_atom"`
	UnclaimedEnd      string       `json:"unclaimed_end_atom"`
	FirstHop          []whereHop   `json:"first_hop"`
	IBCByChain        []whereChain `json:"ibc_by_chain"`
	NotFollowedShare  float64      `json:"not_followed_share"`
	FollowedThroughTo string       `json:"followed_to"`
}

var whereMethod = map[string]string{
	"question":       "What happened to the staking rewards delegators withdrew each month: where each withdrawn ATOM went on its first move out of the wallet that received it.",
	"source":         "Every cosmoshub-4 block from its first block (2021-02-18), read from our own copy of the chain's block results. A transaction counts only when it succeeded. Every liquid ATOM movement of every account is replayed in order from the cosmoshub-4 genesis balances.",
	"withdrawn":      "Every reward payout from the distribution module to a delegator's withdraw address in a successful transaction, including the automatic withdrawals the chain makes when a delegator delegates more, undelegates or redelegates, and withdrawals by interchain accounts that arrive in relayed packets. Commission withdrawals are not included. A reward belongs to the month of the block that paid it.",
	"rules":          "A wallet holds coins from many sources, so which coins left first is a choice. Under first in first out the oldest coins leave first. Under pro-rata every outflow takes from each source in proportion to its share of the balance. Under both rules, an outflow later in the same transaction as a withdrawal takes that withdrawal's rewards first (a withdraw-and-delegate transaction restakes its rewards). The difference between the two rules is the uncertainty, shown as low_atom and high_atom.",
	"restaked":       "Delegated again: sent to the staking module's bonded or not-bonded pool.",
	"liquid_staking": "Turned into liquid staking: sent over IBC in a transfer that asks the receiving chain to liquid-stake it (Stride autopilot), delegated in a transaction that also tokenizes delegation shares, or sent to the Hub's liquid staking module.",
	"ibc":            "Moved to another chain over IBC, by destination chain from the channel's light client.",
	"exchange":       "Sent to a labelled exchange address. Exchange labels are pending review, so this category is empty and those sends are counted in other_sends until labels are published. No exchange is named.",
	"fees":           "Paid as transaction fees.",
	"other_modules":  "Sent to another module: governance deposits, community pool funding and similar.",
	"other_sends":    "Sent to any other account.",
	"left_in_wallet": "Still in the wallet that received it at the end of the data.",
	"not_followed":   "Rewards received by a wallet whose replayed balance did not match the chain's balance at the check height, or that the replay could not follow (a payment larger than the replayed balance). They are shown instead of being guessed. A wallet stops being followed for all of its rewards.",
	"unclaimed":      "Rewards credited to delegators (every validator's rewards minus its commission, from the inflation data) minus rewards withdrawn. unclaimed_end_atom starts from the delegators' unclaimed rewards in the cosmoshub-4 genesis.",
	"first_hop":      "Only the first move is counted. Coins that came back later (an IBC transfer that timed out, an unbonding that completed) count as new coins of their own and are not reclassified.",
	"scope":          "cosmoshub-4 only. Monthly rows follow UTC months; the last month ends at last_day.",
}

func (s *Server) handleWhereAtomGoes(w http.ResponseWriter, r *http.Request) {
	f, err := loadWhere()
	if err != nil {
		http.Error(w, "where-ATOM-goes data unavailable", http.StatusInternalServerError)
		return
	}
	infl, err := loadInflation()
	if err != nil {
		http.Error(w, "inflation data unavailable", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	if view == "" {
		view = "monthly"
	}
	if view != "monthly" && view != "yearly" {
		http.Error(w, "view must be monthly or yearly", http.StatusBadRequest)
		return
	}
	from, to := q.Get("from"), q.Get("to")
	for _, v := range []string{from, to} {
		if v == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, padDate(v, "01")); err != nil {
			http.Error(w, "from and to are YYYY, YYYY-MM or YYYY-MM-DD", http.StatusBadRequest)
			return
		}
	}
	lo, hi := "", "9999-12-31"
	if from != "" {
		lo = padDate(from, "01")
	}
	if to != "" {
		hi = padDate(to, "31")
	}
	rows := whereRows(f, infl, view)
	out := make([]whereRow, 0, len(rows))
	for _, row := range rows {
		if row.EndDate < lo || row.StartDate > hi {
			continue
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{
		"chain": f.Chain,
		"as_of": f.AsOf,
		"view":  view,
		"unit":  "ATOM",
		"coverage": map[string]any{
			"first_height":         f.FirstHeight,
			"last_height":          f.LastHeight,
			"first_day":            f.FirstDay,
			"last_day":             f.LastDay,
			"blocks_counted":       f.BlocksCounted,
			"blocks_expected":      f.BlocksExpected,
			"complete":             f.BlocksCounted == f.BlocksExpected,
			"check_height":         f.CheckHeight,
			"wallets_rewarded":     f.WalletsRewarded,
			"wallets_checked":      f.WalletsChecked,
			"wallets_not_followed": f.WalletsNotFollowed,
			"exchange_labels":      f.ExchangeLabels,
			"unclaimed_check": map[string]any{
				"open_atom":           uatomString(f.UnclaimedOpen),
				"chain_at_check_atom": uatomString(f.UnclaimedCloseNode),
			},
		},
		"liquid_staking_rule": f.LiquidStakingRule,
		"method":              whereMethod,
		"rows":                out,
	})
}

// whereRows builds every period; unclaimed_end runs from the genesis opening
// through each period, so filtering by from and to does not change it.
func whereRows(f *whereFile, infl *inflationData, view string) []whereRow {
	n := 7
	if view == "yearly" {
		n = 4
	}
	type acc struct {
		key                                  string
		start, end                           string
		minted, pool, commission, delegators *big.Int
		withdrawn, records                   int64
		fifo, pr                             []int64
		ibcF, ibcP                           []int64
	}
	byKey := map[string]*acc{}
	var keys []string
	get := func(k string) *acc {
		a, ok := byKey[k]
		if !ok {
			a = &acc{key: k, minted: new(big.Int), pool: new(big.Int), commission: new(big.Int), delegators: new(big.Int),
				fifo: make([]int64, len(whereCategories)), pr: make([]int64, len(whereCategories)),
				ibcF: make([]int64, len(f.Chains)), ibcP: make([]int64, len(f.Chains))}
			byKey[k] = a
			keys = append(keys, k)
		}
		return a
	}
	for _, d := range infl.Days {
		if d.Date < f.FirstDay || d.Date > f.LastDay {
			continue
		}
		a := get(d.Date[:n])
		if a.start == "" {
			a.start = d.Date
		}
		a.end = d.Date
		a.minted.Add(a.minted, d.Minted)
		a.pool.Add(a.pool, d.CommunityPool)
		a.commission.Add(a.commission, d.CommissionEvents)
		a.delegators.Add(a.delegators, new(big.Int).Sub(d.RewardsEvents, d.CommissionEvents))
	}
	for _, m := range f.Months {
		a := get(m.Month[:n])
		a.withdrawn += m.Withdrawn
		a.records += m.Records
		for i := range whereCategories {
			a.fifo[i] += m.FIFO[i]
			a.pr[i] += m.ProRata[i]
		}
		for i := range f.Chains {
			a.ibcF[i] += m.IBCFIFO[i]
			a.ibcP[i] += m.IBCProRata[i]
		}
	}
	sort.Strings(keys)
	unclaimed := big.NewInt(f.UnclaimedOpen)
	rows := make([]whereRow, 0, len(keys))
	for _, k := range keys {
		a := byKey[k]
		change := new(big.Int).Sub(a.delegators, big.NewInt(a.withdrawn))
		unclaimed.Add(unclaimed, change)
		row := whereRow{Period: k, StartDate: a.start, EndDate: a.end, Minted: atomString(a.minted), ToCommunityPool: atomString(a.pool),
			ToCommission: atomString(a.commission), ToDelegators: atomString(a.delegators), Withdrawn: uatomString(a.withdrawn),
			Withdrawals: a.records, UnclaimedChange: atomString(change), UnclaimedEnd: atomString(unclaimed), FollowedThroughTo: f.LastDay}
		share := func(v int64) *float64 {
			x := 0.0
			if a.withdrawn > 0 {
				x = round6(float64(v) / float64(a.withdrawn))
			}
			return &x
		}
		str := func(v int64) *string { s := uatomString(v); return &s }
		for i, c := range whereCategories {
			hop := whereHop{Category: c, FIFOAtom: str(a.fifo[i]), ProRataAtom: str(a.pr[i]), LowAtom: str(min(a.fifo[i], a.pr[i])),
				HighAtom: str(max(a.fifo[i], a.pr[i])), FIFOShare: share(a.fifo[i]), ProRataShare: share(a.pr[i])}
			row.FirstHop = append(row.FirstHop, hop)
			if c == "ibc" {
				row.FirstHop = append(row.FirstHop, whereHop{Category: "sent_to_labelled_exchange_address", Status: "pending",
					StatusDetails: "Exchange labels are pending review; these sends are counted in other_sends until labels are published."})
			}
		}
		row.NotFollowedShare = *share(max(a.fifo[7], a.pr[7]))
		type ch struct {
			i    int
			size int64
		}
		var chs []ch
		for i := range f.Chains {
			if a.ibcF[i] != 0 || a.ibcP[i] != 0 {
				chs = append(chs, ch{i, max(a.ibcF[i], a.ibcP[i])})
			}
		}
		sort.SliceStable(chs, func(x, y int) bool { return chs[x].size > chs[y].size })
		row.IBCByChain = []whereChain{}
		for _, c := range chs {
			row.IBCByChain = append(row.IBCByChain, whereChain{Chain: f.Chains[c.i], FIFOAtom: uatomString(a.ibcF[c.i]), ProRataAtom: uatomString(a.ibcP[c.i])})
		}
		rows = append(rows, row)
	}
	return rows
}
