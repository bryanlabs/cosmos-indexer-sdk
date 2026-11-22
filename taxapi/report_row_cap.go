package taxapi

import (
	"fmt"
	"strings"
	"time"
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
// privateWalletTaxEventsSQL, so the planner never scans the time index.
const reportRowCapSQL = `SELECT count(*) FROM (
 SELECT 1 FROM (SELECT timestamp FROM taxable_events WHERE from_addr = ? OR to_addr = ? OFFSET 0) wallet_events
 WHERE timestamp >= ? AND timestamp < ? LIMIT ?
) capped`

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
func (s *Server) checkReportRowCap(addresses []string, start, end time.Time) error {
	if s.reportRowCap <= 0 {
		return nil
	}
	var total int64
	for _, addr := range addresses {
		var n int64
		if err := s.db.Raw(reportRowCapSQL, addr, addr, start, end, s.reportRowCap-total+1).Scan(&n).Error; err != nil {
			return err
		}
		if total += n; total > s.reportRowCap {
			return reportTooLarge{Limit: s.reportRowCap}
		}
	}
	return nil
}
