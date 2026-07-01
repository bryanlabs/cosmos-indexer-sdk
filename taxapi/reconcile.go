package taxapi

import (
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
	Gaps []CoverageRow `json:"gaps"`
}

// typeCount is one row of the reconciliation query: a message type and how many
// of those messages exist / were classified in the range.
type typeCount struct {
	MessageType string
	Total       int64
	Classified  int64
}

// coverage runs the reconciliation query over [start,end) and summarizes it.
func (s *Server) coverage(start, end time.Time) (*CoverageReport, error) {
	var rows []typeCount
	if err := s.db.Table("messages").
		Select("message_types.message_type AS message_type, COUNT(DISTINCT messages.id) AS total, COUNT(DISTINCT taxable_events.message_id) AS classified").
		Joins("JOIN message_types ON message_types.id = messages.message_type_id").
		Joins("JOIN txes ON txes.id = messages.tx_id").
		Joins("JOIN blocks ON blocks.id = txes.block_id").
		Joins("LEFT JOIN taxable_events ON taxable_events.message_id = messages.id").
		Where("blocks.time_stamp >= ? AND blocks.time_stamp < ?", start, end).
		Group("message_types.message_type").
		Order("total DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	return summarizeCoverage(rows, start, end), nil
}

// summarizeCoverage turns raw per-type counts into the reconciliation report,
// splitting types into supported vs gaps. Pure (no DB) so it is unit-testable.
func summarizeCoverage(rows []typeCount, start, end time.Time) *CoverageReport {
	rep := &CoverageReport{
		Start: start.UTC().Format("2006-01-02"),
		End:   end.UTC().Format("2006-01-02"),
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
