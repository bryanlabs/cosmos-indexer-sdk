package taxapi

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// A wallet whose history in a period is over the report row cap is read in
// windows: stretches of the period, in time order, each holding no more
// than the cap. Each window is read in its own snapshot and priced and
// reviewed before the next is read, so memory holds one window's rows and
// what is kept from the windows before it, not the whole history.
//
// Windows are cut on whole days, and a day over the cap on distinct
// timestamps. Every row of a transaction shares its block time, so a
// transaction, its fee and its swap legs always fall in one window. A single
// timestamp over the cap is refused as too large.

// reportWindow is one stretch [Start, End) of a report period.
type reportWindow struct {
	Start, End time.Time
}

const secondsPerDay = 86400

// reportWindowDaysSQL counts a wallet's rows per UTC day in the period, with
// the OFFSET 0 fence of reportRowCapSQL so the address indexes stay in
// charge. A self-transfer counts once, as in the cap.
const reportWindowDaysSQL = `SELECT floor(extract(epoch FROM timestamp) / 86400)::bigint AS day, count(*) AS n FROM (
 SELECT timestamp FROM (SELECT timestamp FROM taxable_events WHERE from_addr = ? OR to_addr = ? OFFSET 0) wallet_events
 WHERE timestamp >= ? AND timestamp < ?
) period_events GROUP BY 1 ORDER BY 1`

// reportWindowInstantsSQL counts a wallet's rows per timestamp in part of
// one day.
const reportWindowInstantsSQL = `SELECT timestamp AS at, count(*) AS n FROM (
 SELECT timestamp FROM (SELECT timestamp FROM taxable_events WHERE from_addr = ? OR to_addr = ? OFFSET 0) wallet_events
 WHERE timestamp >= ? AND timestamp < ?
) period_events GROUP BY 1 ORDER BY 1`

type windowDayCount struct {
	Day int64
	N   int64
}

type windowInstantCount struct {
	At time.Time
	N  int64
}

// overReportRowCap reports whether the wallets have more rows in the period
// than the row cap; never when the cap is off.
func (s *Server) overReportRowCap(addresses []string, start, end time.Time) (bool, error) {
	err := s.checkReportRowCap(addresses, start, end)
	if _, tooLarge := err.(reportTooLarge); tooLarge {
		return true, nil
	}
	return false, err
}

// reportWindows cuts [start, end) into windows of at most limit rows of the
// wallets together, in time order, covering the period with no gap.
func (s *Server) reportWindows(addresses []string, start, end time.Time, limit int64) ([]reportWindow, error) {
	if limit <= 0 {
		return []reportWindow{{start, end}}, nil
	}
	perDay := map[int64]int64{}
	for _, addr := range addresses {
		var counts []windowDayCount
		if err := s.db.Raw(reportWindowDaysSQL, addr, addr, start, end).Scan(&counts).Error; err != nil {
			return nil, err
		}
		for _, c := range counts {
			perDay[c.Day] += c.N
		}
	}
	days := make([]int64, 0, len(perDay))
	for day := range perDay {
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })

	var windows []reportWindow
	from, rows := start, int64(0)
	// cut closes the window being built at the given time and starts the
	// next one there. A cut at or before the window's start is no cut.
	cut := func(at time.Time) {
		if !at.After(from) {
			return
		}
		windows = append(windows, reportWindow{from, at})
		from, rows = at, 0
	}
	for _, day := range days {
		n := perDay[day]
		dayStart := time.Unix(day*secondsPerDay, 0).UTC()
		dayEnd := dayStart.Add(secondsPerDay * time.Second)
		if n <= limit {
			if rows+n > limit {
				cut(dayStart)
			}
			rows += n
			continue
		}
		// The day alone is over the limit: cut it on its timestamps.
		cut(dayStart)
		if err := s.cutReportDay(addresses, laterTime(dayStart, start), earlierTime(dayEnd, end), limit, &from, &rows, cut); err != nil {
			return nil, err
		}
	}
	windows = append(windows, reportWindow{from, end})
	return windows, nil
}

// cutReportDay continues the windows across [dayStart, dayEnd), one day
// over the limit, cutting between its timestamps.
func (s *Server) cutReportDay(addresses []string, dayStart, dayEnd time.Time, limit int64, from *time.Time, rows *int64, cut func(time.Time)) error {
	perInstant := map[int64]int64{}
	at := map[int64]time.Time{}
	for _, addr := range addresses {
		var counts []windowInstantCount
		if err := s.db.Raw(reportWindowInstantsSQL, addr, addr, dayStart, dayEnd).Scan(&counts).Error; err != nil {
			return err
		}
		for _, c := range counts {
			key := c.At.UnixNano()
			perInstant[key] += c.N
			at[key] = c.At.UTC()
		}
	}
	keys := make([]int64, 0, len(perInstant))
	for key := range perInstant {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		n := perInstant[key]
		if n > limit {
			return reportTooLarge{Limit: limit}
		}
		if *rows+n > limit {
			cut(at[key])
		}
		*rows += n
	}
	return nil
}

func laterTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlierTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// windowedReview is what /asset-review answers from for a report read in
// windows: the rows a person reviews, in report order, and what every other
// row contributes to the answer.
type windowedReview struct {
	// Rows are the flagged receipts and the rows waiting on a semantic
	// review, in the order a report read whole lists them.
	Rows []Row
	// FeeOutcomeUnknown counts every row of the report whose fee outcome is
	// unknown, kept or not.
	FeeOutcomeUnknown int
	// Used holds every decision key a row of the report was decided by.
	Used map[string]bool
}

// readiness is readinessForRows over every row of the report: the counts
// and reasons it reads come only from kept rows, except the unknown fee
// outcomes, counted across all of them.
func (r windowedReview) readiness() ReportReadiness {
	readiness := readinessForRows(r.Rows)
	readiness.FeeOutcomeUnknown = r.FeeOutcomeUnknown
	return readiness
}

// keptForReview is whether /asset-review answers with the row.
func keptForReview(row Row) bool {
	return row.AssetIdentity != nil || semanticReviewReason(row) != ""
}

// reviewedWindows prices and reviews each window's rows as reviewedInputRows
// does for a report read whole, keeping the rows keptForReview. read reads
// one window. The rows a report read whole lists wallet by wallet, each
// wallet's events in time order and then its fees, so the kept rows are
// gathered per wallet and per kind and joined in that order at the end.
func (s *Server) reviewedWindows(chain string, addresses []string, windows []reportWindow, decisions AssetDecisions, read func(reportWindow) (reportInput, error)) (windowedReview, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return windowedReview{}, assetReviewInputError{err}
	}
	match := newDecisionMatch()
	review := windowedReview{Used: map[string]bool{}}
	keptEvents := make([][]Row, len(addresses))
	keptFees := make([][]Row, len(addresses))
	for _, window := range windows {
		input, err := read(window)
		if err != nil {
			return windowedReview{}, err
		}
		if len(input.Wallets) != len(addresses) {
			return windowedReview{}, fmt.Errorf("report window %s to %s read %d wallets of %d", window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339), len(input.Wallets), len(addresses))
		}
		var rows []Row
		events := make([]int, len(addresses))
		sizes := make([]int, len(addresses))
		for i, addr := range addresses {
			priced := s.priceWalletRows(chain, addr, input.Wallets[i])
			events[i] = len(priced) - len(input.Wallets[i].Fees)
			sizes[i] = len(priced)
			rows = append(rows, priced...)
		}
		input = reportInput{}
		if hasUserPriceRowKeys(decisions) {
			if err := s.loadMessageIndexes(rows); err != nil {
				return windowedReview{}, err
			}
		}
		if rows, err = match.apply(rows, decisions); err != nil {
			return windowedReview{}, assetReviewInputError{err}
		}
		markDecisionKeysUsed(rows, review.Used)
		offset := 0
		for i := range addresses {
			for j, row := range rows[offset : offset+sizes[i]] {
				if row.FeeOutcomeUnknown {
					review.FeeOutcomeUnknown++
				}
				if !keptForReview(row) {
					continue
				}
				if j < events[i] {
					keptEvents[i] = append(keptEvents[i], row)
				} else {
					keptFees[i] = append(keptFees[i], row)
				}
			}
			offset += sizes[i]
		}
	}
	if err := match.check(decisions); err != nil {
		return windowedReview{}, assetReviewInputError{err}
	}
	for i := range addresses {
		review.Rows = append(review.Rows, keptEvents[i]...)
		review.Rows = append(review.Rows, keptFees[i]...)
	}
	return review, nil
}

// errReviewHistoryChanged refuses a review read in windows whose history
// changed between the plan and the last window.
var errReviewHistoryChanged = errors.New("financial history changed while the review was read; retry")

// readReviewWindows reads a report period over the row cap for review, one
// window per snapshot. The windows, the readiness gate for the whole period
// and the review fingerprint are read in one snapshot before the first
// window, and the fingerprint again after the last. A change between them
// refuses the answer, so every window shows the history the windows were
// planned on, and a blocked report names what blocks the whole period.
func (s *Server) readReviewWindows(chain string, addresses []string, start, end time.Time, decisions AssetDecisions) (windowedReview, error) {
	var windows []reportWindow
	var before string
	if err := s.withReadSnapshotTimeout(reportJobReadSnapshotTimeout, func(reader *Server) error {
		var err error
		if windows, err = reader.reportWindows(addresses, start, end, s.reportRowCap); err != nil {
			return err
		}
		if _, err = reader.reportReadiness(chain, addresses, start, end); err != nil {
			return err
		}
		before, err = reader.reviewFingerprintInSnapshot(chain, addresses)
		return err
	}); err != nil {
		return windowedReview{}, err
	}
	eras := s.legacyEras(chain)
	review, err := s.reviewedWindows(chain, addresses, windows, decisions, func(window reportWindow) (reportInput, error) {
		input, err := s.readWalletInput(chain, addresses, window.Start, window.End, false, eras, reportJobReadSnapshotTimeout)
		var required reportReadinessRequired
		if errors.As(err, &required) {
			return input, s.periodReadinessError(chain, addresses, start, end)
		}
		return input, err
	})
	if err != nil {
		return windowedReview{}, err
	}
	var after string
	if err := s.withReadSnapshotTimeout(reportJobReadSnapshotTimeout, func(reader *Server) error {
		var err error
		after, err = reader.reviewFingerprintInSnapshot(chain, addresses)
		return err
	}); err != nil {
		return windowedReview{}, err
	}
	if after != before {
		return windowedReview{}, errReviewHistoryChanged
	}
	return review, nil
}

// periodReadinessError answers a window the readiness gate blocked, which
// the whole period passed when the windows were planned: history changed
// while the review was read. It names what blocks the whole period now, as
// a report read whole would, or asks for a retry if nothing does.
func (s *Server) periodReadinessError(chain string, addresses []string, start, end time.Time) error {
	if err := s.withReadSnapshotTimeout(reportJobReadSnapshotTimeout, func(reader *Server) error {
		_, err := reader.reportReadiness(chain, addresses, start, end)
		return err
	}); err != nil {
		return err
	}
	return errReviewHistoryChanged
}
