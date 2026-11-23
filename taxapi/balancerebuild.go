package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BalanceRebuild is what RebuildBalanceDaysFor replaced, or would replace.
type BalanceRebuild struct {
	From, To  time.Time
	Addresses int
	Before    int
	After     int
	Written   bool
}

// balanceDayLine is a stored day as RebuildBalanceDaysFor writes it out.
type balanceDayLine struct {
	Chain        string          `json:"chain"`
	Address      string          `json:"address"`
	Day          string          `json:"day"`
	Height       int64           `json:"height"`
	AnchorHeight int64           `json:"anchor_height"`
	Status       string          `json:"status"`
	Reason       string          `json:"reason"`
	Holdings     json.RawMessage `json:"holdings"`
}

// RebuildBalanceDaysFor rebuilds the stored days of the addresses with the
// given address ids over the whole built span, after a correction to their
// history, through the same walk-back BuildBalanceDays runs. Every wallet is
// walked back first, writing nothing, fee outcomes included. With write set
// the rows are then replaced in one transaction that holds the build row's
// lock, so no build or append runs over it and readers see every corrected
// wallet or none. before receives each stored row of the span that the
// rebuild replaces and after each row it stores, as JSON lines, and rollback
// a SQL script that puts the before rows back; all three are written ahead of
// the commit. A wallet too heavy to rebuild stops it before anything is
// written. Each of uatomOnly, ids from addressIDs, must keep every stored
// value but its liquid uatom on every day of the span, or nothing is written.
func (s *Server) RebuildBalanceDaysFor(chain, prefix string, node *ledgerNode, addressIDs, uatomOnly []int64, workers int, write bool, before, after, rollback io.Writer, logf func(string, ...any)) (BalanceRebuild, error) {
	var out BalanceRebuild
	addrs, err := s.rebuildAddresses(prefix, addressIDs)
	if err != nil {
		return out, err
	}
	held, err := s.uatomOnlyAddresses(addressIDs, uatomOnly)
	if err != nil {
		return out, err
	}
	build, err := s.balanceBuild(chain)
	if err != nil {
		return out, err
	}
	if build == nil {
		return out, errors.New("no balance build has finished yet")
	}
	// A running append writes only days after the finished span, so a dry
	// run may read beside it; the write refuses any build or append.
	if err := rebuildableBuild(*build); err != nil && (write || build.AppendingTo == nil) {
		return out, err
	}
	from, to := *build.FromDay, *build.ToDay
	out.From, out.To, out.Addresses = from, to, len(addrs)
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return out, err
	}
	var days []time.Time
	var ends []heightTime
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		ht, err := s.dayEndBlock(ids, d)
		if err != nil {
			return out, fmt.Errorf("end of %s: %w", d.Format(apiDateLayout), err)
		}
		days, ends = append(days, d), append(ends, ht)
	}

	reader := NewServer(s.db, s.oracle, s.native)
	reader.readOnlyLedger = true
	rows := make([][]BalanceDay, len(addrs))
	var mu sync.Mutex
	var firstErr error
	work := make(chan int)
	var wg sync.WaitGroup
	done := 0
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				a := addrs[i]
				n, err := reader.walletRows(a, ledgerHeavyRows)
				if err == nil && n > ledgerHeavyRows {
					err = fmt.Errorf("names %d event attribute rows, too many to rebuild", n)
				}
				var r []BalanceDay
				if err == nil {
					r, err = reader.walletDayRows(chain, ids, node, a, days, ends)
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", a, err)
				}
				rows[i] = r
				done++
				if done%50 == 0 {
					logf("balance rebuild: %d of %d addresses walked back", done, len(addrs))
				}
				mu.Unlock()
			}
		}()
	}
	for i := range addrs {
		work <- i
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return out, firstErr
	}

	replace := func(tx *gorm.DB) error {
		var old []BalanceDay
		if err := tx.Where("chain = ? AND address IN ? AND day BETWEEN ? AND ?", chain, addrs, from.Format(apiDateLayout), to.Format(apiDateLayout)).Order("address, day").Find(&old).Error; err != nil {
			return err
		}
		out.Before = len(old)
		if err := writeDayLines(before, old); err != nil {
			return fmt.Errorf("before rows: %w", err)
		}
		var all []BalanceDay
		for _, r := range rows {
			all = append(all, r...)
		}
		out.After = len(all)
		if err := writeDayLines(after, all); err != nil {
			return fmt.Errorf("after rows: %w", err)
		}
		if err := writeRollback(rollback, chain, addrs, from, to, old); err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
		for _, w := range []io.Writer{before, after, rollback} {
			if f, ok := w.(interface{ Sync() error }); ok {
				if err := f.Sync(); err != nil {
					return err
				}
			}
		}
		for i, a := range addrs {
			id, ok := held[a]
			if !ok {
				continue
			}
			n, first, err := daysChangedBeyondUatom(a, old, rows[i], days)
			if err != nil {
				return fmt.Errorf("address id %d: %w", id, err)
			}
			logf("balance rebuild: address id %d changes values other than liquid uatom on %d of %d days", id, n, len(days))
			if n > 0 {
				return fmt.Errorf("address id %d changes values other than liquid uatom, first on %s", id, first)
			}
		}
		if !write {
			return nil
		}
		for i, a := range addrs {
			if err := replaceWalletDays(tx, chain, a, days, rows[i]); err != nil {
				return fmt.Errorf("%s: %w", a, err)
			}
		}
		return nil
	}
	if !write {
		return out, replace(s.db)
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var b BalanceBuild
		if err := buildFields(tx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("chain = ?", chain).Take(&b).Error; err != nil {
			return err
		}
		if err := rebuildableBuild(b); err != nil {
			return err
		}
		if !b.FromDay.Equal(from) || !b.ToDay.Equal(to) {
			return fmt.Errorf("the built span moved to %s to %s during the walk-back", b.FromDay.Format(apiDateLayout), b.ToDay.Format(apiDateLayout))
		}
		return replace(tx)
	})
	if err != nil {
		return out, err
	}
	out.Written = true
	logf("balance rebuild: %d addresses, %s to %s, %d rows replaced by %d", len(addrs), from.Format(apiDateLayout), to.Format(apiDateLayout), out.Before, out.After)
	return out, nil
}

// rebuildableBuild refuses a build that has no finished span or has a build
// or an append in progress.
func rebuildableBuild(b BalanceBuild) error {
	switch {
	case b.FromDay == nil || b.ToDay == nil:
		return errors.New("no balance build has finished yet")
	case b.BuildingFrom != nil:
		return fmt.Errorf("a build of %s to %s is unfinished", b.BuildingFrom.Format(apiDateLayout), b.BuildingTo.Format(apiDateLayout))
	case b.AppendingTo != nil:
		return fmt.Errorf("an append through %s is unfinished", b.AppendingTo.Format(apiDateLayout))
	}
	return nil
}

// rebuildAddresses resolves address ids to account addresses, refusing ids
// that are missing, repeated or not accounts of the prefix.
func (s *Server) rebuildAddresses(prefix string, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("no address ids")
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("address id %d is listed twice", id)
		}
		seen[id] = true
	}
	var found []struct {
		ID      int64
		Address string
	}
	if err := s.db.Table("addresses").Select("id, address").Where("id IN ?", ids).Find(&found).Error; err != nil {
		return nil, err
	}
	if len(found) != len(ids) {
		return nil, fmt.Errorf("%d of %d address ids exist", len(found), len(ids))
	}
	addrs := make([]string, 0, len(found))
	for _, f := range found {
		if hrp, _, err := bech32.DecodeAndConvert(f.Address); err != nil || hrp != prefix {
			return nil, fmt.Errorf("address id %d is not an account", f.ID)
		}
		addrs = append(addrs, f.Address)
	}
	sort.Strings(addrs)
	return addrs, nil
}

// uatomOnlyAddresses maps the address of each of ids, which must all be in
// the rebuild's list, to its id.
func (s *Server) uatomOnlyAddresses(list, ids []int64) (map[string]int64, error) {
	listed := map[int64]bool{}
	for _, id := range list {
		listed[id] = true
	}
	for _, id := range ids {
		if !listed[id] {
			return nil, fmt.Errorf("address id %d is not in the rebuild's list", id)
		}
	}
	out := map[string]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	var found []struct {
		ID      int64
		Address string
	}
	if err := s.db.Table("addresses").Select("id, address").Where("id IN ?", ids).Find(&found).Error; err != nil {
		return nil, err
	}
	for _, f := range found {
		out[f.Address] = f.ID
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("%d of %d uatom-only address ids exist", len(out), len(ids))
	}
	return out, nil
}

// daysChangedBeyondUatom counts the days of the span on which addr's rebuilt
// rows, read the way the API reads them (the latest row on or before the
// day), differ from the stored ones in status, reason or any holding other
// than liquid uatom, and names the first. Every stored row must also have a
// rebuilt row on its day at the same height.
func daysChangedBeyondUatom(addr string, old, rebuilt []BalanceDay, days []time.Time) (int, string, error) {
	var was []BalanceDay
	for _, r := range old {
		if r.Address == addr {
			was = append(was, r)
		}
	}
	sort.Slice(was, func(i, j int) bool { return was[i].Day.Before(was[j].Day) })
	now := append([]BalanceDay(nil), rebuilt...)
	sort.Slice(now, func(i, j int) bool { return now[i].Day.Before(now[j].Day) })
	byDay := map[string]BalanceDay{}
	for _, r := range now {
		byDay[r.Day.Format(apiDateLayout)] = r
	}
	n, first := 0, ""
	mark := func(d string) {
		if n == 0 {
			first = d
		}
		n++
	}
	for _, r := range was {
		d := r.Day.Format(apiDateLayout)
		if nr, ok := byDay[d]; !ok || nr.Height != r.Height {
			mark(d)
		}
	}
	held := func(rs []BalanceDay, d time.Time) *BalanceDay {
		i := sort.Search(len(rs), func(i int) bool { return rs[i].Day.After(d) })
		if i == 0 {
			return nil
		}
		return &rs[i-1]
	}
	for _, d := range days {
		a, b := held(was, d), held(now, d)
		if (a == nil) != (b == nil) {
			mark(d.Format(apiDateLayout))
			continue
		}
		if a == nil {
			continue
		}
		ha, err := holdingsWithoutUatom(a.Holdings)
		if err != nil {
			return 0, "", err
		}
		hb, err := holdingsWithoutUatom(b.Holdings)
		if err != nil {
			return 0, "", err
		}
		if a.Status != b.Status || a.Reason != b.Reason || !reflect.DeepEqual(ha, hb) {
			mark(d.Format(apiDateLayout))
		}
	}
	return n, first, nil
}

// holdingsWithoutUatom decodes a holdings document and drops liquid uatom,
// and liquid itself when nothing else is left in it.
func holdingsWithoutUatom(raw string) (map[string]any, error) {
	h := map[string]any{}
	if raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return nil, err
		}
	}
	if liq, ok := h["liquid"].(map[string]any); ok {
		delete(liq, "uatom")
		if len(liq) == 0 {
			delete(h, "liquid")
		}
	}
	return h, nil
}

func writeDayLines(w io.Writer, rows []BalanceDay) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(balanceDayLine{Chain: r.Chain, Address: r.Address, Day: r.Day.Format(apiDateLayout), Height: r.Height, AnchorHeight: r.AnchorHeight, Status: r.Status, Reason: r.Reason, Holdings: json.RawMessage(r.Holdings)}); err != nil {
			return err
		}
	}
	return nil
}

// writeRollback writes a script that replaces the span's rows of addrs with
// rows, the ones stored before the rebuild.
func writeRollback(w io.Writer, chain string, addrs []string, from, to time.Time, rows []BalanceDay) error {
	q := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	list := make([]string, len(addrs))
	for i, a := range addrs {
		list[i] = q(a)
	}
	var b strings.Builder
	b.WriteString("BEGIN;\n")
	fmt.Fprintf(&b, "DELETE FROM tax_balance_days WHERE chain = %s AND day BETWEEN %s AND %s AND address IN (%s);\n", q(chain), q(from.Format(apiDateLayout)), q(to.Format(apiDateLayout)), strings.Join(list, ","))
	for _, r := range rows {
		fmt.Fprintf(&b, "INSERT INTO tax_balance_days (chain, address, day, height, anchor_height, status, reason, holdings) VALUES (%s, %s, %s, %d, %d, %s, %s, %s);\n", q(r.Chain), q(r.Address), q(r.Day.Format(apiDateLayout)), r.Height, r.AnchorHeight, q(r.Status), q(r.Reason), q(r.Holdings))
	}
	b.WriteString("COMMIT;\n")
	_, err := io.WriteString(w, b.String())
	return err
}
