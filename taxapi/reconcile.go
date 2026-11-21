package taxapi

import (
	"context"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// supportedTypes is the set of message type URLs the tax parser classifies.
var supportedTypes = func() map[string]bool {
	m := make(map[string]bool, len(tax.MessageTypeURLs))
	for _, u := range tax.MessageTypeURLs {
		m[u] = true
	}
	return m
}()

// CoverageRow is per-message-type coverage over a block range.
type CoverageRow struct {
	MessageType  string `json:"message_type"`
	Total        int64  `json:"total"`        // messages of this type in range
	Classified   int64  `json:"classified"`   // of those, how many produced a taxable event
	Supported    bool   `json:"supported"`    // is this type handled by the parser at all
	Unclassified int64  `json:"unclassified"` // Total - Classified
}

// CoverageReport is the reconciliation summary an enterprise user needs to trust
// that every taxable event has been accounted for: it lists every message type
// seen on chain in the range, how many we classified, and which types are gaps
// (seen but unsupported, e.g. CosmWasm MsgExecuteContract).
type CoverageReport struct {
	Start            string        `json:"start"`
	End              string        `json:"end"`
	TotalMessages    int64         `json:"total_messages"`
	ClassifiedMsgs   int64         `json:"classified_messages"`
	CoveragePercent  float64       `json:"coverage_percent"`
	SupportedTypes   int           `json:"supported_types_seen"`
	UnsupportedTypes int           `json:"unsupported_types_seen"`
	Rows             []CoverageRow `json:"rows"`
	// Gaps are types that appeared on chain but the parser does not handle —
	// the explicit "here is what we are NOT yet reporting" list.
	Gaps                 []CoverageRow `json:"gaps"`
	ParserErrors         int64         `json:"parser_errors"`
	DecodeErrors         int64         `json:"decode_errors"`
	OpaqueMessages       int64         `json:"opaque_messages"`
	UnreviewedMessages   int64         `json:"unreviewed_messages"`
	ParserDecodeComplete bool          `json:"parser_decode_complete"`
	// TransactionsScanned is the number of indexed transactions in the
	// period; null when /coverage was counted live.
	TransactionsScanned *int64 `json:"transactions_scanned,omitempty"`
	// CountedFrom says how far the coverage rollup was built when it was
	// summed; null when counted live.
	CountedFrom *CoverageRollupInfo `json:"counted_from,omitempty"`
}

// typeCount is one row of the reconciliation query: a message type and how many
// of those messages exist / were classified in the range.
type typeCount struct {
	MessageType string
	Total       int64
	Classified  int64
}

// coverage counts messages by type over [start,end) by summing the coverage
// rollup (coveragerollup.go). It never counts every message live: that scan
// wrote up to 86 GB of temp files. Until the rollup covers the period it
// returns errCoverageRollupUnavailable, which /coverage answers with 503.
func (s *Server) coverage(chain string, start, end time.Time) (*CoverageReport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coverageRollupTimeout+10*time.Second)
	defer cancel()
	counts, err := s.covRollup.periodCounts(ctx, chain, start, end)
	if err != nil {
		return nil, err
	}
	readiness, err := s.classificationReadiness(chain, nil, start, end)
	if err != nil {
		return nil, err
	}
	report := summarizeCoverage(counts.Types, start, end)
	report.ParserErrors, report.DecodeErrors = readiness.ParserErrors, readiness.DecodeErrors
	report.OpaqueMessages = readiness.OpaqueMessages
	report.UnreviewedMessages = readiness.UnreviewedMessages
	report.ParserDecodeComplete = readiness.ParserDecodeComplete
	n := counts.Transactions
	info := counts.Rollup
	report.TransactionsScanned, report.CountedFrom = &n, &info
	return report, nil
}

// summarizeCoverage turns raw per-type counts into the reconciliation report,
// splitting types into supported vs gaps. Pure (no DB) so it is unit-testable.
func summarizeCoverage(rows []typeCount, start, end time.Time) *CoverageReport {
	rep := &CoverageReport{
		Start: start.UTC().Format(apiDateLayout),
		End:   end.UTC().Format(apiDateLayout),
		Rows:  make([]CoverageRow, 0, len(rows)),
		Gaps:  []CoverageRow{},
	}
	for _, r := range rows {
		cr := CoverageRow{
			MessageType:  r.MessageType,
			Total:        r.Total,
			Classified:   r.Classified,
			Supported:    supportedTypes[r.MessageType],
			Unclassified: r.Total - r.Classified,
		}
		rep.Rows = append(rep.Rows, cr)
		rep.TotalMessages += r.Total
		rep.ClassifiedMsgs += r.Classified
		if cr.Supported {
			rep.SupportedTypes++
		} else {
			rep.UnsupportedTypes++
			rep.Gaps = append(rep.Gaps, cr)
		}
	}
	if rep.TotalMessages > 0 {
		rep.CoveragePercent = float64(rep.ClassifiedMsgs) / float64(rep.TotalMessages) * 100
	}
	return rep
}
