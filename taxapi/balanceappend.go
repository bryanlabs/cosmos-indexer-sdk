package taxapi

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// AppendBalanceDays extends the built span with the finished days after it,
// up to through. A wallet's holdings at the end of a day change only through
// what the chain records for it: an event naming it or its operator address,
// a fee it paid or a grant it covered, an index hole that names it, a change
// in the tokens per share of a validator it is staked with, or a slash
// reaching its unbonding entries. Only those wallets, and those whose last
// stored day could not be rebuilt, are rebuilt for the new days; every other
// wallet's last stored row stays in force, as it does within the span. A
// window this cannot bound (an index hole whose addresses are unknown, a
// slash of an unknown validator, or a double sign, which also reaches
// redelegations) is built for every address instead.
//
// Days are appended only once their blocks are all indexed and the ledger
// scan and the validator rates cover them; until then nothing is appended
// and those days keep walking back live.
func (s *Server) AppendBalanceDays(chain, prefix string, node *ledgerNode, through time.Time, workers int, logf func(string, ...any)) error {
	var build BalanceBuild
	if err := buildFields(s.db).Where("chain = ?", chain).Limit(1).Find(&build).Error; err != nil {
		return err
	}
	if build.ToDay == nil {
		logf("balance days: no build has finished yet, nothing appended")
		return nil
	}
	if build.BuildingFrom != nil {
		if !build.BuildingFrom.Equal(build.ToDay.AddDate(0, 0, 1)) {
			logf("balance days: a build of %s to %s is unfinished, nothing appended", build.BuildingFrom.Format(apiDateLayout), build.BuildingTo.Format(apiDateLayout))
			return nil
		}
		logf("balance days: finishing the build of %s to %s", build.BuildingFrom.Format(apiDateLayout), build.BuildingTo.Format(apiDateLayout))
		return s.BuildBalanceDays(chain, prefix, node, *build.BuildingFrom, *build.BuildingTo, 2000, workers, logf)
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return err
	}
	scan, err := s.ledgerScan(chain)
	if err != nil {
		return err
	}
	last, err := s.dayEndBlock(ids, *build.ToDay)
	if err != nil {
		return fmt.Errorf("end of %s: %w", build.ToDay.Format(apiDateLayout), err)
	}
	through = through.UTC().Truncate(24 * time.Hour)
	var days []time.Time
	var ends []heightTime
	for d := build.ToDay.AddDate(0, 0, 1); !d.After(through); d = d.AddDate(0, 0, 1) {
		ht, err := s.dayEndBlock(ids, d)
		if errors.Is(err, errBalanceNotReached) {
			break
		}
		if err != nil {
			return fmt.Errorf("end of %s: %w", d.Format(apiDateLayout), err)
		}
		if ht.Height > scan.FeeGrantsTo {
			logf("balance days: the ledger scan has not reached the end of %s yet", d.Format(apiDateLayout))
			break
		}
		var recorded int64
		if err := s.db.Model(&ValidatorRateSample{}).Where("chain = ? AND height = ?", chain, ht.Height).Count(&recorded).Error; err != nil {
			return err
		}
		if recorded == 0 {
			logf("balance days: validator rates at the end of %s are not recorded yet", d.Format(apiDateLayout))
			break
		}
		days, ends = append(days, d), append(ends, ht)
	}
	if len(days) == 0 {
		logf("balance days: nothing to append after %s yet", build.ToDay.Format(apiDateLayout))
		return nil
	}
	from, to := days[0], days[len(days)-1]
	lo, hi := last.Height, ends[len(ends)-1].Height
	if missing, err := s.windowIndexed(ids, lo, hi); err != nil {
		return err
	} else if missing > 0 {
		logf("balance days: %d blocks of %s to %s are not indexed yet, nothing appended", missing, from.Format(apiDateLayout), to.Format(apiDateLayout))
		return nil
	}

	wallets, why, err := s.appendWallets(chain, prefix, ids, node, scan, build, lo, ends)
	if err != nil {
		return err
	}
	if why != "" {
		logf("balance days: %s, so %s to %s is built for every address", why, from.Format(apiDateLayout), to.Format(apiDateLayout))
		return s.BuildBalanceDays(chain, prefix, node, from, to, 2000, workers, logf)
	}

	err = s.lockBalanceBuild(chain, func(tx *gorm.DB, b *BalanceBuild) error {
		if b.BuildingFrom != nil || !b.ToDay.Equal(*build.ToDay) {
			return errors.New("the build changed while the append was prepared")
		}
		b.AppendingTo = &to
		return tx.Save(b).Error
	})
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var firstErr error
	var stored int
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range work {
				n, err := s.buildWalletDays(chain, ids, node, a, days, ends)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", a, err)
				}
				stored += n
				mu.Unlock()
			}
		}()
	}
	for _, a := range wallets {
		work <- a
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	err = s.lockBalanceBuild(chain, func(tx *gorm.DB, b *BalanceBuild) error {
		b.ToDay, b.AppendingTo = &to, nil
		return tx.Save(b).Error
	})
	if err != nil {
		return err
	}
	logf("balance days: appended %s to %s, %d addresses rebuilt, %d rows stored", from.Format(apiDateLayout), to.Format(apiDateLayout), len(wallets), stored)
	return nil
}

// appendWallets lists, sorted, the wallets whose holdings at the end of one
// of the days ending at ends may differ from their last stored row, the one
// in force at the end of block lo, or why the window cannot be bounded.
func (s *Server) appendWallets(chain, prefix string, ids []int64, node *ledgerNode, scan LedgerScan, build BalanceBuild, lo int64, ends []heightTime) ([]string, string, error) {
	hi := ends[len(ends)-1].Height
	tip, err := s.ledgerTip(ids, node, scan.FeeGrantsTo)
	if err != nil {
		return nil, "", err
	}
	out := map[string]bool{}
	add := func(v string) {
		hrp, _, err := bech32.DecodeAndConvert(v)
		switch {
		case err != nil:
		case hrp == prefix:
			out[v] = true
		case hrp == prefix+apiValoper:
			if acct, err := OperatorAccount(v); err == nil {
				out[acct] = true
			}
		}
	}

	var unknownHoles int64
	if err := s.db.Model(&IndexHole{}).Where("chain = ? AND height > ? AND height <= ? AND NOT addresses_known", chain, lo, hi).Count(&unknownHoles).Error; err != nil {
		return nil, "", err
	}
	if unknownHoles > 0 {
		return nil, fmt.Sprintf("%d blocks in the window are missing transactions whose addresses are unknown", unknownHoles), nil
	}
	slashes, err := s.ledgerSlashes(ids, lo, hi, node, tip.Height, nil)
	var inc balanceIncomplete
	if errors.As(err, &inc) {
		return nil, inc.reason, nil
	}
	if err != nil {
		return nil, "", err
	}
	affected := map[string]bool{}
	for _, sl := range slashes {
		if sl.DoubleSign {
			return nil, fmt.Sprintf("%s was slashed for a double sign at height %d", sl.Validator, sl.Height), nil
		}
		affected[sl.Validator] = true
	}

	// Everything named from two blocks before the window, where a downtime
	// slash in it can reach new entries, up to the tip the stakers are read
	// at, so a staker who left a validator after the window is still found.
	params := map[string]any{"ids": ids, "lo": lo - 2, "hi": tip.Height, "pattern": prefix + "%"}
	var named []string
	// Left to itself the planner scans every event attribute for a day's
	// blocks; walked from the blocks through the indexes it takes seconds.
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL enable_seqscan = off; SET LOCAL enable_hashjoin = off; SET LOCAL enable_mergejoin = off").Error; err != nil {
			return err
		}
		for _, q := range []string{`
SELECT DISTINCT a.value FROM blocks b
JOIN txes x ON x.block_id = b.id
JOIN messages m ON m.tx_id = x.id
JOIN message_events me ON me.message_id = m.id
JOIN message_event_attributes a ON a.message_event_id = me.id
WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi AND a.value LIKE @pattern`, `
SELECT DISTINCT a.value FROM blocks b
JOIN block_events be ON be.block_id = b.id
JOIN block_event_attributes a ON a.block_event_id = be.id
WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi AND a.value LIKE @pattern`, `
SELECT DISTINCT ad.address FROM blocks b
JOIN txes x ON x.block_id = b.id
JOIN fees f ON f.tx_id = x.id
JOIN addresses ad ON ad.id = f.payer_address_id
WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi`, `
SELECT DISTINCT g.granter FROM blocks b
JOIN txes x ON x.block_id = b.id
JOIN tax_fee_grants g ON g.tx_id = x.id
WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi`} {
			named = named[:0]
			if err := tx.Raw(q, params).Scan(&named).Error; err != nil {
				return err
			}
			for _, v := range named {
				add(v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("named addresses: %w", err)
	}
	named = named[:0]
	if err := s.db.Model(&IndexHoleAddress{}).Where("chain = ? AND height > ? AND height <= ?", chain, lo, hi).Distinct().Pluck("address", &named).Error; err != nil {
		return nil, "", err
	}
	for _, v := range named {
		add(v)
	}

	heights := []int64{lo}
	for _, e := range ends {
		heights = append(heights, e.Height)
	}
	changed, err := s.changedRates(chain, heights)
	if err != nil {
		return nil, "", err
	}
	for v := range changed {
		affected[v] = true
	}
	for v := range affected {
		stakers, err := node.stakers(v, tip.Height)
		if err != nil {
			return nil, "", fmt.Errorf("stakers of %s at height %d: %w", v, tip.Height, err)
		}
		for _, a := range stakers {
			add(a)
		}
	}

	named = named[:0]
	if err := s.db.Raw(`
SELECT d.address FROM tax_balance_days d
WHERE d.chain = @chain AND d.status <> '' AND d.day BETWEEN @from AND @to
AND NOT EXISTS (SELECT 1 FROM tax_balance_days n
  WHERE n.chain = d.chain AND n.address = d.address AND n.day > d.day AND n.day <= @to)`,
		map[string]any{"chain": chain, "from": build.FromDay.Format(apiDateLayout), "to": build.ToDay.Format(apiDateLayout)}).Scan(&named).Error; err != nil {
		return nil, "", fmt.Errorf("unbuilt wallets: %w", err)
	}
	for _, v := range named {
		add(v)
	}

	wallets := make([]string, 0, len(out))
	for a := range out {
		wallets = append(wallets, a)
	}
	sort.Strings(wallets)
	return wallets, "", nil
}

// changedRates is every validator whose tokens per share differ between two
// consecutive heights, or that is recorded at one and not the other.
func (s *Server) changedRates(chain string, heights []int64) (map[string]bool, error) {
	var rows []ValidatorRateSample
	if err := s.db.Raw(`SELECT chain, operator, height, tokens::text AS tokens, shares::text AS shares FROM tax_validator_rates WHERE chain = ? AND height IN ?`, chain, heights).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("validator rates: %w", err)
	}
	at := map[int64]map[string][2]decimal.Decimal{}
	for _, r := range rows {
		t, err := decimal.NewFromString(r.Tokens)
		if err != nil {
			return nil, fmt.Errorf("validator rate tokens %q: %w", r.Tokens, err)
		}
		sh, err := decimal.NewFromString(r.Shares)
		if err != nil {
			return nil, fmt.Errorf("validator rate shares %q: %w", r.Shares, err)
		}
		if at[r.Height] == nil {
			at[r.Height] = map[string][2]decimal.Decimal{}
		}
		at[r.Height][r.Operator] = [2]decimal.Decimal{t, sh}
	}
	for _, h := range heights {
		if len(at[h]) == 0 {
			return nil, fmt.Errorf("no validator rates are recorded at height %d", h)
		}
	}
	out := map[string]bool{}
	for i := 1; i < len(heights); i++ {
		a, b := at[heights[i-1]], at[heights[i]]
		for v, x := range a {
			y, ok := b[v]
			if !ok || !x[0].Mul(y[1]).Equal(y[0].Mul(x[1])) {
				out[v] = true
			}
		}
		for v := range b {
			if _, ok := a[v]; !ok {
				out[v] = true
			}
		}
	}
	return out, nil
}

// stakers lists the delegators and unbonding delegators of validator v at
// height.
func (n *ledgerNode) stakers(v string, height int64) ([]string, error) {
	var out []string
	err := n.paged("/cosmos/staking/v1beta1/validators/"+v+"/delegations", height, func(get func(any) error) (pageResp, error) {
		var body struct {
			DelegationResponses []struct {
				Delegation struct {
					DelegatorAddress string `json:"delegator_address"`
				} `json:"delegation"`
			} `json:"delegation_responses"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, d := range body.DelegationResponses {
			out = append(out, d.Delegation.DelegatorAddress)
		}
		return body.Pagination, nil
	})
	if err != nil {
		return nil, fmt.Errorf("delegations: %w", err)
	}
	err = n.paged("/cosmos/staking/v1beta1/validators/"+v+"/unbonding_delegations", height, func(get func(any) error) (pageResp, error) {
		var body struct {
			UnbondingResponses []struct {
				DelegatorAddress string `json:"delegator_address"`
			} `json:"unbonding_responses"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, u := range body.UnbondingResponses {
			out = append(out, u.DelegatorAddress)
		}
		return body.Pagination, nil
	})
	if err != nil {
		return nil, fmt.Errorf("unbonding delegations: %w", err)
	}
	return out, nil
}
