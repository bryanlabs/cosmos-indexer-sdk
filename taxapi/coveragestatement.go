package taxapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// CoverageStatement is what every report and export says about the index it
// was built from: the chains and heights it reads, how far the index reaches,
// and the known open items. It rides in index_coverage.statement and, as one
// line, in X-Coverage-Statement.
type CoverageStatement struct {
	GeneratedAt          time.Time        `json:"generated_at"`
	IndexedThroughHeight int64            `json:"indexed_through_height"`
	IndexedThroughTime   *time.Time       `json:"indexed_through_time"`
	Chains               []StatementChain `json:"chains"`
	// IndexHoles is null when the hole list could not be read.
	IndexHoles *StatementHoles `json:"index_holes"`
	// LedgerScanFrom is the first cosmoshub-4 height whose fee grant use and
	// index holes are checked, zero when it could not be read.
	LedgerScanFrom         int64    `json:"ledger_scan_from"`
	RecordOnlyMessageTypes []string `json:"record_only_message_types"`
	// UnreviewedMessageTypes counts indexed message types the tax layer has no
	// decision for; reports that touch one are held back.
	UnreviewedMessageTypes int `json:"unreviewed_message_types"`
	// TransactionsScanned and MessagesScanned count the cosmoshub-4
	// transactions and messages indexed in the period, from the coverage
	// rollup; null when they could not be counted quickly.
	TransactionsScanned *int64   `json:"transactions_scanned"`
	MessagesScanned     *int64   `json:"messages_scanned"`
	OpenItems           []string `json:"open_items"`
	// Text is the statement as printed on documents, one item per line.
	Text string `json:"text"`
	// Summary is the statement as one ASCII line, as in X-Coverage-Statement.
	Summary string `json:"summary"`
}

// StatementChain is one Hub chain a report reads, or does not read yet.
type StatementChain struct {
	ChainID     string     `json:"chain_id"`
	FirstHeight int64      `json:"first_height"`
	LastHeight  int64      `json:"last_height"`
	StartTime   *time.Time `json:"start_time"`
	EndTime     *time.Time `json:"end_time"`
	Status      string     `json:"status"`
	InReports   bool       `json:"in_reports"`
}

// StatementHoles counts the cosmoshub-4 blocks recorded as possibly missing a
// transaction. Flagged blocks stay recorded even once a source check clears
// them; Open excludes those, when the clearances can be read.
type StatementHoles struct {
	Flagged int64         `json:"flagged"`
	Cleared int64         `json:"cleared"`
	Open    int64         `json:"open"`
	ByYear  map[int]int64 `json:"open_by_year"`
	// ClearancesRead is false when the clearances could not be read, so
	// Open counts every flagged block.
	ClearancesRead bool `json:"clearances_read"`
}

const headerCoverageStatement = "X-Coverage-Statement"

// statementFactsTTL bounds how stale the database facts in a statement are.
const statementFactsTTL = 5 * time.Minute

type statementFacts struct {
	at         time.Time
	holes      *StatementHoles
	ledgerFrom int64
	unreviewed int
}

type statementCache struct {
	mu    sync.Mutex
	facts *statementFacts
}

// statementFacts reads the hole counts, ledger scan span and unreviewed type
// count, at most once per statementFactsTTL. A part that cannot be read is
// left empty and the statement says so.
func (s *Server) statementFacts() statementFacts {
	s.stmtCache.mu.Lock()
	defer s.stmtCache.mu.Unlock()
	if f := s.stmtCache.facts; f != nil && nowUTC().Sub(f.at) < statementFactsTTL {
		return *f
	}
	f := &statementFacts{at: nowUTC()}
	if s.db != nil {
		f.holes = s.statementHoles()
		if scan, err := s.ledgerScan(apiMainnet); err == nil {
			f.ledgerFrom = scan.FeeGrantsFrom
		}
		var unreviewed int64
		if err := s.db.Table("message_types").Where("message_type NOT IN ?", tax.ReviewedTypeURLs()).Count(&unreviewed).Error; err == nil {
			f.unreviewed = int(unreviewed)
		} else {
			f.unreviewed = -1
		}
	}
	s.stmtCache.facts = f
	return *f
}

func (s *Server) statementHoles() *StatementHoles {
	var rows []struct {
		Year    int
		Flagged int64
		Cleared int64
	}
	var readable bool
	if err := s.db.Raw(`SELECT coalesce(has_table_privilege(to_regclass('tax_index_hole_clearances'), 'SELECT'), false)`).Scan(&readable).Error; err != nil {
		readable = false
	}
	cleared := "0"
	if readable {
		cleared = "count(*) FILTER (WHERE EXISTS (SELECT 1 FROM tax_index_hole_clearances c WHERE c.chain = h.chain AND c.height = h.height))"
	}
	err := s.db.Raw(`SELECT coalesce(extract(year FROM b.time_stamp AT TIME ZONE 'UTC')::int, 0) AS year,
 count(*) AS flagged, `+cleared+` AS cleared
FROM tax_index_holes h
LEFT JOIN blocks b ON b.height = h.height AND b.chain_id = (SELECT id FROM chains WHERE chain_id = ?)
WHERE h.chain = ?
GROUP BY 1 ORDER BY 1`, apiHubChainID, apiMainnet).Scan(&rows).Error
	if err != nil {
		return nil
	}
	out := &StatementHoles{ByYear: map[int]int64{}, ClearancesRead: readable}
	for _, r := range rows {
		out.Flagged += r.Flagged
		out.Cleared += r.Cleared
		if open := r.Flagged - r.Cleared; open > 0 {
			out.ByYear[r.Year] += open
		}
	}
	out.Open = out.Flagged - out.Cleared
	return out
}

// coverageStatement builds the statement for a period from the current
// snapshot and the database facts.
func (s *Server) coverageStatement(snap *IndexCoverage, period PeriodCoverage) *CoverageStatement {
	f := s.statementFacts()
	st := &CoverageStatement{
		GeneratedAt:            nowUTC(),
		IndexHoles:             f.holes,
		LedgerScanFrom:         f.ledgerFrom,
		RecordOnlyMessageTypes: append([]string(nil), tax.NoTaxEffectTypeURLs...),
		UnreviewedMessageTypes: f.unreviewed,
		Chains:                 []StatementChain{},
		OpenItems:              []string{},
	}
	sort.Strings(st.RecordOnlyMessageTypes)
	if snap != nil {
		st.IndexedThroughHeight = snap.TipHeight
		st.IndexedThroughTime = timePtr(snap.TipTime)
	}
	for _, e := range erasCoverage(s.legacyEras(apiMainnet), snap) {
		st.Chains = append(st.Chains, StatementChain{
			ChainID: e.ChainID, FirstHeight: e.FirstHeight, LastHeight: e.LastHeight,
			StartTime: e.StartTime, EndTime: e.EndTime, Status: e.Status, InReports: e.InReports,
		})
	}
	if period.hasRange {
		ctx, cancel := context.WithTimeout(context.Background(), statementScanTimeout)
		if c, err := s.covRollup.periodCounts(ctx, apiMainnet, period.from, period.to); err == nil {
			tx, msgs := c.Transactions, c.Messages
			st.TransactionsScanned, st.MessagesScanned = &tx, &msgs
		}
		cancel()
	}
	st.OpenItems = statementOpenItems(st, period)
	st.Text = statementText(st, period)
	st.Summary = statementHeader(st)
	return st
}

func statementOpenItems(st *CoverageStatement, period PeriodCoverage) []string {
	var items []string
	switch {
	case st.IndexedThroughHeight == 0:
		items = append(items, "Index coverage was still being computed, so this document may not cover the whole period.")
	case !period.Complete && period.Status != "":
		items = append(items, "The requested period is not fully indexed yet: "+strings.TrimSpace(period.Warning))
	}
	for _, c := range st.Chains {
		if !c.InReports {
			items = append(items, fmt.Sprintf("%s (heights %d to %d) is not read by reports yet; its status is %s.", c.ChainID, c.FirstHeight, c.LastHeight, c.Status))
		}
	}
	switch h := st.IndexHoles; {
	case h == nil:
		items = append(items, "The list of cosmoshub-4 blocks flagged as possibly missing a transaction could not be read when this was generated.")
	case h.Open > 0:
		years := make([]int, 0, len(h.ByYear))
		for y := range h.ByYear {
			years = append(years, y)
		}
		sort.Ints(years)
		parts := make([]string, 0, len(years))
		for _, y := range years {
			parts = append(parts, fmt.Sprintf("%d in %d", h.ByYear[y], y))
		}
		msg := fmt.Sprintf("%d cosmoshub-4 blocks (%s) hold a transaction the chain included more than once, and the index kept the other copy. Balance history stops at these blocks for the wallets they may touch; they stay flagged until a source-verified repair.", h.Open, strings.Join(parts, ", "))
		if !h.ClearancesRead {
			msg += " Blocks since cleared by a source check could not be told apart, so all flagged blocks are counted."
		}
		items = append(items, msg)
	}
	if st.LedgerScanFrom > 0 {
		for _, c := range st.Chains {
			if c.ChainID == apiHubChainID && c.FirstHeight > 0 && st.LedgerScanFrom > c.FirstHeight {
				items = append(items, fmt.Sprintf("Fee grant use and flagged blocks are checked from cosmoshub-4 height %d; heights %d to %d are not checked for them yet.", st.LedgerScanFrom, c.FirstHeight, st.LedgerScanFrom-1))
			}
		}
	}
	switch {
	case st.UnreviewedMessageTypes < 0:
		items = append(items, "Whether every indexed message type has a tax decision could not be read when this was generated.")
	case st.UnreviewedMessageTypes > 0:
		items = append(items, fmt.Sprintf("%d indexed message types have no tax decision yet; a report that touches one is held back.", st.UnreviewedMessageTypes))
	}
	items = append(items, fmt.Sprintf("%d message types are recorded with no tax effect (for example votes, grants and validator edits); they appear in the history but add no income, gain or loss.", len(st.RecordOnlyMessageTypes)))
	return items
}

func statementDate(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func statementText(st *CoverageStatement, period PeriodCoverage) string {
	var b strings.Builder
	b.WriteString("COVERAGE STATEMENT\n")
	fmt.Fprintf(&b, "Generated %s from the live Cosmos Tax index.\n", statementDate(&st.GeneratedAt))
	if period.Start != "" || period.End != "" {
		fmt.Fprintf(&b, "Period: %s to %s, %s.\n", orUnbounded(period.Start), orUnbounded(period.End), periodWord(period))
	}
	if period.hasRange {
		b.WriteString(statementScanLine(st) + "\n")
	}
	for _, c := range st.Chains {
		read := "read by reports"
		if !c.InReports {
			read = "not read by reports yet"
		}
		if c.ChainID == apiHubChainID {
			fmt.Fprintf(&b, "%s: heights %d to %d, indexed through %s, %s.\n", c.ChainID, c.FirstHeight, st.IndexedThroughHeight, statementDate(st.IndexedThroughTime), read)
			continue
		}
		fmt.Fprintf(&b, "%s: heights %d to %d (%s to %s), %s, %s.\n", c.ChainID, c.FirstHeight, c.LastHeight, statementDay(c.StartTime), statementDay(c.EndTime), c.Status, read)
	}
	b.WriteString("Known open items:\n")
	for _, item := range st.OpenItems {
		b.WriteString("- " + item + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// statementScanLine states how many transactions the index holds for the
// period, which is what the statement's coverage claims rest on.
func statementScanLine(st *CoverageStatement) string {
	if st.TransactionsScanned == nil || st.MessagesScanned == nil {
		return "Transactions scanned: not counted when this was generated."
	}
	return fmt.Sprintf("Transactions scanned: %s %s transactions (%s messages) indexed in the period.",
		groupDigits(*st.TransactionsScanned), apiHubChainID, groupDigits(*st.MessagesScanned))
}

// groupDigits writes n with comma thousands separators.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func statementDay(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}

func orUnbounded(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

func periodWord(p PeriodCoverage) string {
	switch {
	case p.Complete:
		return "every block indexed"
	case p.Status == coverageStatusUnknown:
		return "coverage not known yet"
	}
	return "not fully indexed"
}

// statementHeader is the statement as one ASCII header line.
func statementHeader(st *CoverageStatement) string {
	parts := []string{}
	for _, c := range st.Chains {
		last := c.LastHeight
		if c.ChainID == apiHubChainID {
			last = st.IndexedThroughHeight
		}
		if c.InReports {
			parts = append(parts, fmt.Sprintf("%s %d-%d", c.ChainID, c.FirstHeight, last))
		}
	}
	line := fmt.Sprintf("chains %s; indexed through %d at %s", strings.Join(parts, ", "), st.IndexedThroughHeight, statementDate(st.IndexedThroughTime))
	if h := st.IndexHoles; h != nil {
		line += fmt.Sprintf("; %d flagged blocks open", h.Open)
	}
	if st.LedgerScanFrom > 0 {
		line += fmt.Sprintf("; ledger scan from %d", st.LedgerScanFrom)
	}
	line += fmt.Sprintf("; %d record-only message types; %d unreviewed; generated %s", len(st.RecordOnlyMessageTypes), st.UnreviewedMessageTypes, st.GeneratedAt.UTC().Format(time.RFC3339))
	return asciiHeader(line)
}

// handleCoverageStatement serves GET /coverage-statement[?start=&end=], the
// statement alone, as JSON or, with format=text, as plain text.
func (s *Server) handleCoverageStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := s.periodCoverage(dateParam(q.Get(apiStart), time.Time{}), dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1)))
	setPeriodCoverageHeaders(w, p)
	w.Header().Set("Cache-Control", "public, max-age=60")
	if q.Get("format") == "text" {
		w.Header().Set(apiContentType, "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(p.Statement.Text + "\n"))
		return
	}
	w.Header().Set(apiContentType, apiJSONContentType)
	writeJSON(w, p.Statement)
}
