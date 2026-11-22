package taxapi

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// DefaultReportRowCap is the largest wallet history, in taxable event rows,
// that a report is built from on request. A report holds its whole input in
// memory before pricing. Measured on 2026-11-22, the input costs about 1 KB
// per row (599,936 rows came to 618 MB), and the API pod's limit is 512 MiB,
// shared with every other request. 250,000 rows is about 250 MB, under half
// the limit, which leaves room for the priced rows and the CSV built after.
const DefaultReportRowCap = 250000

// reportRowCapSQL counts a wallet's rows in the period, stopping one past the
// cap. The OFFSET 0 fence keeps the address indexes in charge, as in
// privateWalletTaxEventsSQL, so the planner never scans the time index. It is
// the exact count, run only when the index-only bounds below cannot decide.
const reportRowCapSQL = `SELECT count(*) FROM (
 SELECT 1 FROM (SELECT timestamp FROM taxable_events WHERE from_addr = ? OR to_addr = ? OFFSET 0) wallet_events
 WHERE timestamp >= ? AND timestamp < ? LIMIT ?
) capped`

// reportRowCapFromSQL counts the wallet's sent rows in the period, and among
// them its self-transfers, from the (from_addr, denom, timestamp) INCLUDE
// (category, to_addr) index without reading the table.
const reportRowCapFromSQL = `SELECT count(*) AS rows, count(*) FILTER (WHERE to_addr = ?) AS self FROM (
 SELECT to_addr FROM taxable_events WHERE from_addr = ? AND timestamp >= ? AND timestamp < ? LIMIT ?
) capped`

// reportRowCapToSQL counts the wallet's received rows over all time from the
// to_addr index. That index has no timestamp, so this is the period's count
// only when the period spans every stored row, and an upper bound otherwise.
const reportRowCapToSQL = `SELECT count(*) FROM (SELECT 1 FROM taxable_events WHERE to_addr = ? LIMIT ?) capped`

// reportRowCapSpanSQL is the stored rows' first and last time, read from the
// two ends of the time index, and whether any row has no time (such a row is
// in the all-time received count but in no period).
const reportRowCapSpanSQL = `SELECT min(timestamp) AS first, max(timestamp) AS last,
 EXISTS (SELECT 1 FROM taxable_events WHERE timestamp IS NULL) AS undated FROM taxable_events`

// reportRowCapWorkMem lets the exact count's bitmap stay exact on a wallet
// with hundreds of thousands of rows, instead of going lossy at the default
// 4MB and rechecking every row of each heap page it touches.
const reportRowCapWorkMem = "64MB"

// reportTooLarge is returned before any report input is read when the
// wallets have more rows than a report can hold in memory on request.
type reportTooLarge struct {
	Limit int64
}

func (e reportTooLarge) Error() string {
	return fmt.Sprintf("This wallet history has more than %s transaction records, more than a report can be built from on request (limit %s records). Choose a shorter period, such as one year or one month.",
		groupedCount(e.Limit), groupedCount(e.Limit))
}

func groupedCount(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// SetReportRowCap sets the largest wallet history a report reads on request.
// Zero or less turns the check off; that is the default, so batch tools that
// build reports outside the API keep reading every row.
func (s *Server) SetReportRowCap(rows int64) {
	if rows < 0 {
		rows = 0
	}
	s.reportRowCap = rows
}

// checkReportRowCap refuses a report whose wallets together have more rows in
// [start, end) than the cap, before the report input is read. The count is an
// upper bound on the report's own rows: it spans every chain the database
// holds and counts a self-transfer once.
//
// Each wallet is first bounded from index-only counts. Its rows in the period
// are its sent rows plus its received rows less its self-transfers. The sent
// rows and self-transfers in the period are exact; the received rows are
// counted over all time, which is exact when the period spans every stored
// row and an upper bound otherwise. The wallets are refused when the lower
// bounds already pass the cap, allowed when the upper bounds stay under it,
// and only a request between the two runs the exact count. The verdict is
// always the exact count's.
func (s *Server) checkReportRowCap(addresses []string, start, end time.Time) error {
	if s.reportRowCap <= 0 {
		return nil
	}
	bounds, err := s.reportRowBounds(addresses, start, end)
	if err != nil {
		return err
	}
	var lower, upper int64
	for _, b := range bounds {
		lower += b.lower
		if upper >= 0 && b.upper >= 0 {
			upper += b.upper
		} else {
			upper = -1
		}
	}
	switch {
	case lower > s.reportRowCap:
		return reportTooLarge{Limit: s.reportRowCap}
	case upper >= 0 && upper <= s.reportRowCap:
		return nil
	}
	return s.exactReportRowCap(addresses, start, end)
}

// reportRowBound is one wallet's row count in the period: at least lower, at
// most upper, and upper is -1 when no bound under the cap is known.
type reportRowBound struct {
	lower, upper int64
}

func (s *Server) reportRowBounds(addresses []string, start, end time.Time) ([]reportRowBound, error) {
	limit := s.reportRowCap + 1
	var span struct {
		First, Last *time.Time
		Undated     bool
	}
	if err := s.db.Raw(reportRowCapSpanSQL).Scan(&span).Error; err != nil {
		return nil, err
	}
	whole := !span.Undated && (span.First == nil || (!start.After(*span.First) && end.After(*span.Last)))
	out := make([]reportRowBound, len(addresses))
	for i, addr := range addresses {
		var sent struct {
			Rows, Self int64
		}
		if err := s.db.Raw(reportRowCapFromSQL, addr, addr, start, end, limit).Scan(&sent).Error; err != nil {
			return nil, err
		}
		var received int64
		if err := s.db.Raw(reportRowCapToSQL, addr, limit).Scan(&received).Error; err != nil {
			return nil, err
		}
		b := reportRowBound{lower: sent.Rows, upper: -1}
		if whole && received > b.lower {
			b.lower = received
		}
		if sent.Rows < limit && received < limit {
			b.upper = sent.Rows + received - sent.Self
			if whole {
				b.lower = b.upper
			}
		}
		out[i] = b
	}
	return out, nil
}

// exactReportRowCap is the exact count, for a request the bounds cannot
// decide. It runs in one transaction so its work_mem stays local to it.
func (s *Server) exactReportRowCap(addresses []string, start, end time.Time) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL work_mem = '" + reportRowCapWorkMem + "'").Error; err != nil {
			return err
		}
		var total int64
		for _, addr := range addresses {
			var n int64
			if err := tx.Raw(reportRowCapSQL, addr, addr, start, end, s.reportRowCap-total+1).Scan(&n).Error; err != nil {
				return err
			}
			if total += n; total > s.reportRowCap {
				return reportTooLarge{Limit: s.reportRowCap}
			}
		}
		return nil
	})
}
