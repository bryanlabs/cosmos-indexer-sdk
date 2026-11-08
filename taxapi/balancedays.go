package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BalanceDay is a wallet's holdings at the end of one UTC day, stored only
// when they, or the reason they cannot be rebuilt, differ from the day
// before. A day with no row has the holdings of the wallet's latest row
// before it, so a wallet's first row in the built span is always stored.
type BalanceDay struct {
	Chain        string    `gorm:"primaryKey"`
	Address      string    `gorm:"primaryKey"`
	Day          time.Time `gorm:"primaryKey;type:date"`
	Height       int64
	AnchorHeight int64
	// Status is empty when Holdings is the rebuilt state, otherwise the
	// status the walk-back gave the day, with its reason.
	Status   string
	Reason   string
	Holdings string `gorm:"type:jsonb"`
}

func (BalanceDay) TableName() string { return "tax_balance_days" }

// BalanceBuild is the span of days for which every address was built, and
// the build in progress with the last address it finished.
type BalanceBuild struct {
	Chain        string     `gorm:"primaryKey"`
	FromDay      *time.Time `gorm:"type:date"`
	ToDay        *time.Time `gorm:"type:date"`
	BuildingFrom *time.Time `gorm:"type:date"`
	BuildingTo   *time.Time `gorm:"type:date"`
	Cursor       string
	// AppendingTo is set while the days after ToDay up to it are appended.
	AppendingTo *time.Time `gorm:"type:date"`
	UpdatedAt   time.Time
}

func (BalanceBuild) TableName() string { return "tax_balance_builds" }

// storedHoldings is ledgerHoldings as stored, in base units.
type storedHoldings struct {
	Liquid          map[string]string `json:"liquid,omitempty"`
	Delegations     map[string]string `json:"delegations,omitempty"`
	Unbonding       map[string]string `json:"unbonding,omitempty"`
	DelegationError map[string]string `json:"delegation_error,omitempty"`
	UnbondingError  map[string]string `json:"unbonding_error,omitempty"`
}

func intStrings(m map[string]*big.Int) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

func stringInts(m map[string]string) (map[string]*big.Int, error) {
	out := make(map[string]*big.Int, len(m))
	for k, v := range m {
		n, ok := new(big.Int).SetString(v, 10)
		if !ok {
			return nil, fmt.Errorf("stored amount %q for %s is not an integer", v, k)
		}
		out[k] = n
	}
	return out, nil
}

// encodeHoldings is canonical: encoding/json sorts map keys, so equal
// holdings encode to equal text.
func encodeHoldings(h ledgerHoldings) (string, error) {
	b, err := json.Marshal(storedHoldings{
		Liquid: intStrings(h.Liquid), Delegations: intStrings(h.Delegations), Unbonding: intStrings(h.Unbonding),
		DelegationError: intStrings(h.DelegationError), UnbondingError: intStrings(h.UnbondingError),
	})
	return string(b), err
}

func decodeHoldings(height int64, text string) (ledgerHoldings, error) {
	var st storedHoldings
	if err := json.Unmarshal([]byte(text), &st); err != nil {
		return ledgerHoldings{}, err
	}
	h := ledgerHoldings{Height: height}
	var err error
	for _, f := range []struct {
		dst *map[string]*big.Int
		src map[string]string
	}{{&h.Liquid, st.Liquid}, {&h.Delegations, st.Delegations}, {&h.Unbonding, st.Unbonding}, {&h.DelegationError, st.DelegationError}, {&h.UnbondingError, st.UnbondingError}} {
		if *f.dst, err = stringInts(f.src); err != nil {
			return ledgerHoldings{}, err
		}
	}
	return h, nil
}

// LedgerAddresses lists, ascending, the account addresses with the chain's
// prefix above after that indexed event attributes name, reading up to limit
// distinct values from each attribute table. It steps through the attribute
// value indexes one value at a time. next is where the following page
// starts, empty once every value has been read.
func (s *Server) LedgerAddresses(prefix, after string, limit int) (addrs []string, next string, err error) {
	lo, hi := prefix+"1", prefix+"2"
	if after > lo {
		lo = after
	}
	merged := map[string]bool{}
	// A table that filled the page has values past its last one unread, so
	// the page ends there, whatever the other table read beyond it.
	bound := hi
	for _, table := range []string{"message_event_attributes", "block_event_attributes"} {
		var values []string
		if err := s.db.Raw(fmt.Sprintf(`
WITH RECURSIVE r(v, n) AS (
  (SELECT left(value, 100), 1 FROM %[1]s WHERE left(value, 100) > @lo AND left(value, 100) < @hi ORDER BY left(value, 100) LIMIT 1)
  UNION ALL
  SELECT (SELECT left(value, 100) FROM %[1]s WHERE left(value, 100) > r.v AND left(value, 100) < @hi ORDER BY left(value, 100) LIMIT 1), n + 1
  FROM r WHERE r.v IS NOT NULL AND n < @limit)
SELECT v FROM r WHERE v IS NOT NULL ORDER BY n`, table), map[string]any{"lo": lo, "hi": hi, "limit": limit}).Scan(&values).Error; err != nil {
			return nil, "", fmt.Errorf("%s values: %w", table, err)
		}
		if len(values) == limit && values[len(values)-1] < bound {
			bound = values[len(values)-1]
		}
		for _, v := range values {
			if hrp, _, err := bech32.DecodeAndConvert(v); err == nil && hrp == prefix {
				merged[v] = true
			}
		}
	}
	out := make([]string, 0, len(merged))
	for v := range merged {
		if v <= bound {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if bound != hi {
		next = bound
	}
	return out, next, nil
}

// NewLedgerNode is the serving node the API walks back from, configured by
// NODE_REST_API and NODE_RPC_API.
func NewLedgerNode() (*ledgerNode, error) { return newLedgerNode() }

// buildWalletDays stores addr's holdings at the end of each day that differ
// from the day before. Rows already stored in the span are replaced, so a
// rebuild after a correction leaves no stale row.
func (s *Server) buildWalletDays(chain string, ids []int64, node *ledgerNode, addr string, days []time.Time, ends []heightTime) (int, error) {
	rebuilt, anchor, err := s.rebuildHoldings(chain, ids, node, addr, ends)
	if err != nil {
		return 0, err
	}
	var prev BalanceDay
	res := s.db.Where("chain = ? AND address = ? AND day < ?", chain, addr, days[0].Format(apiDateLayout)).Order("day DESC").Limit(1).Find(&prev)
	if res.Error != nil {
		return 0, res.Error
	}
	all := make([]BalanceDay, len(rebuilt))
	for i, r := range rebuilt {
		all[i] = BalanceDay{Chain: chain, Address: addr, Day: days[i], Height: ends[i].Height, AnchorHeight: anchor, Status: r.Status, Reason: r.Reason, Holdings: "{}"}
		if r.Status == "" {
			if all[i].Holdings, err = encodeHoldings(r.Holdings); err != nil {
				return 0, err
			}
		}
	}
	var before *BalanceDay
	if res.RowsAffected > 0 {
		before = &prev
	}
	rows := changedDays(before, all)
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("chain = ? AND address = ? AND day BETWEEN ? AND ?", chain, addr, days[0].Format(apiDateLayout), days[len(days)-1].Format(apiDateLayout)).Delete(&BalanceDay{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.CreateInBatches(rows, 1000).Error
	})
	return len(rows), err
}

// changedDays keeps the days whose holdings, status or reason differ from
// the day before; before is the latest row stored ahead of the span.
func changedDays(before *BalanceDay, all []BalanceDay) []BalanceDay {
	var rows []BalanceDay
	for _, row := range all {
		if before != nil && before.Status == row.Status && before.Reason == row.Reason && sameJSON(before.Holdings, row.Holdings) {
			continue
		}
		rows = append(rows, row)
		before = &rows[len(rows)-1]
	}
	return rows
}

// sameJSON compares stored holdings, which Postgres returns from jsonb in its
// own key order, with freshly encoded ones.
func sameJSON(a, b string) bool {
	if a == b {
		return true
	}
	var x, y storedHoldings
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	ea, _ := json.Marshal(x)
	eb, _ := json.Marshal(y)
	return string(ea) == string(eb)
}

// BuildBalanceDays stores every address's holdings at the end of each day
// from from to to, through the same walk-back the API runs. Addresses are
// built in batches; the build records the last address of each finished
// batch, so a restarted build with the same span resumes there. When every
// address is done the span joins the built span, which it must touch.
func (s *Server) BuildBalanceDays(chain, prefix string, node *ledgerNode, from, to time.Time, batch, workers int, logf func(string, ...any)) error {
	from, to = from.UTC().Truncate(24*time.Hour), to.UTC().Truncate(24*time.Hour)
	if to.Before(from) {
		return errors.New("the span ends before it starts")
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return err
	}
	var days []time.Time
	var ends []heightTime
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		ht, err := s.dayEndBlock(ids, d)
		if err != nil {
			return fmt.Errorf("end of %s: %w", d.Format(apiDateLayout), err)
		}
		days, ends = append(days, d), append(ends, ht)
	}
	var build BalanceBuild
	cursor := ""
	err = s.lockBalanceBuild(chain, func(tx *gorm.DB, b *BalanceBuild) error {
		if b.AppendingTo != nil {
			return fmt.Errorf("an append through %s is unfinished; finish it with -days-append first", b.AppendingTo.Format(apiDateLayout))
		}
		if b.FromDay != nil && (from.After(b.ToDay.AddDate(0, 0, 1)) || to.Before(b.FromDay.AddDate(0, 0, -1))) {
			return fmt.Errorf("the span %s to %s does not touch the built span %s to %s", from.Format(apiDateLayout), to.Format(apiDateLayout), b.FromDay.Format(apiDateLayout), b.ToDay.Format(apiDateLayout))
		}
		if b.BuildingFrom != nil && b.BuildingFrom.Equal(from) && b.BuildingTo.Equal(to) {
			cursor = b.Cursor
		} else if b.BuildingFrom != nil {
			return fmt.Errorf("a build of %s to %s is unfinished", b.BuildingFrom.Format(apiDateLayout), b.BuildingTo.Format(apiDateLayout))
		}
		b.BuildingFrom, b.BuildingTo, b.Cursor = &from, &to, cursor
		build = *b
		return tx.Save(b).Error
	})
	if err != nil {
		return err
	}
	if cursor != "" {
		logf("balance days: resuming %s to %s after %s", from.Format(apiDateLayout), to.Format(apiDateLayout), cursor)
	}
	// A wallet first built here, before the built span, has no row on the
	// span's first day, so its rows here would wrongly stay in force through
	// the span. It is built through the whole span instead.
	var fillDays []time.Time
	var fillEnds []heightTime
	if build.FromDay != nil && to.Before(*build.FromDay) {
		for d := *build.FromDay; !d.After(*build.ToDay); d = d.AddDate(0, 0, 1) {
			ht, err := s.dayEndBlock(ids, d)
			if err != nil {
				return fmt.Errorf("end of %s: %w", d.Format(apiDateLayout), err)
			}
			fillDays, fillEnds = append(fillDays, d), append(fillEnds, ht)
		}
	}
	walletDays := func(addr string) ([]time.Time, []heightTime, error) {
		if fillDays == nil {
			return days, ends, nil
		}
		var n int64
		if err := s.db.Model(&BalanceDay{}).Where("chain = ? AND address = ? AND day = ?", chain, addr, build.FromDay.Format(apiDateLayout)).Count(&n).Error; err != nil {
			return nil, nil, err
		}
		if n > 0 {
			return days, ends, nil
		}
		return append(append([]time.Time(nil), days...), fillDays...), append(append([]heightTime(nil), ends...), fillEnds...), nil
	}
	var built, stored int
	for {
		addrs, next, err := s.LedgerAddresses(prefix, cursor, batch)
		if err != nil {
			return err
		}
		var mu sync.Mutex
		var firstErr error
		work := make(chan string)
		var wg sync.WaitGroup
		for w := 0; w < max(workers, 1); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for a := range work {
					wd, we, err := walletDays(a)
					n := 0
					if err == nil {
						n, err = s.buildWalletDays(chain, ids, node, a, wd, we)
					}
					mu.Lock()
					if err != nil && firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", a, err)
					}
					stored += n
					mu.Unlock()
				}
			}()
		}
		for _, a := range addrs {
			work <- a
		}
		close(work)
		wg.Wait()
		if firstErr != nil {
			return firstErr
		}
		built += len(addrs)
		if next == "" {
			break
		}
		cursor = next
		if err := s.db.Model(&BalanceBuild{}).Where("chain = ?", chain).Update("cursor", cursor).Error; err != nil {
			return err
		}
		logf("balance days: %d addresses built, %d rows stored", built, stored)
	}
	err = s.lockBalanceBuild(chain, func(tx *gorm.DB, b *BalanceBuild) error {
		if b.FromDay == nil || from.Before(*b.FromDay) {
			b.FromDay = &from
		}
		if b.ToDay == nil || to.After(*b.ToDay) {
			b.ToDay = &to
		}
		b.BuildingFrom, b.BuildingTo, b.Cursor = nil, nil, ""
		build = *b
		return tx.Save(b).Error
	})
	if err != nil {
		return err
	}
	logf("balance days: built %s to %s for %d addresses, %d rows stored; built span %s to %s", from.Format(apiDateLayout), to.Format(apiDateLayout), built, stored, build.FromDay.Format(apiDateLayout), build.ToDay.Format(apiDateLayout))
	return nil
}

// lockBalanceBuild runs change on the chain's build row, created when
// missing, holding the row's lock, so a build and an append never both start.
func (s *Server) lockBalanceBuild(chain string, change func(tx *gorm.DB, b *BalanceBuild) error) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&BalanceBuild{Chain: chain}).Error; err != nil {
			return err
		}
		var b BalanceBuild
		if err := buildFields(tx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("chain = ?", chain).First(&b).Error; err != nil {
			return err
		}
		return change(tx, &b)
	})
}

// buildFields names BalanceBuild's columns instead of selecting *, so a column
// a newer image's migration adds to tax_balance_builds does not change the
// result type of statements a running API has cached (SQLSTATE 0A000).
func buildFields(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{QueryFields: true}) }

// balanceBuild is the chain's stored build state, or nil before any build.
func (s *Server) balanceBuild(chain string) (*BalanceBuild, error) {
	var build BalanceBuild
	res := buildFields(s.db).Where("chain = ?", chain).Limit(1).Find(&build)
	if res.Error != nil || res.RowsAffected == 0 || build.FromDay == nil {
		return nil, res.Error
	}
	return &build, nil
}

// storedBalanceDay is addr's stored row in force at the end of day: its
// latest row on or before day, when day is in the built span.
func (s *Server) storedBalanceDay(build *BalanceBuild, addr string, day time.Time) (BalanceDay, bool, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	if build == nil || day.Before(*build.FromDay) || day.After(*build.ToDay) {
		return BalanceDay{}, false, nil
	}
	var row BalanceDay
	res := s.db.Where("chain = ? AND address = ? AND day BETWEEN ? AND ?", build.Chain, addr, build.FromDay.Format(apiDateLayout), day.Format(apiDateLayout)).Order("day DESC").Limit(1).Find(&row)
	if res.Error != nil || res.RowsAffected == 0 {
		return BalanceDay{}, false, res.Error
	}
	return row, true, nil
}
