package taxapi

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// YearChainCoverage is one Hub chain's part of a calendar year. Heights
// restart on every chain, so a year that spans a restart lists each chain's
// heights separately.
type YearChainCoverage struct {
	ChainID        string `json:"chain_id"`
	FirstHeight    int64  `json:"first_height"`
	LastHeight     int64  `json:"last_height"`
	ExpectedBlocks int64  `json:"expected_blocks"`
	IndexedBlocks  int64  `json:"indexed_blocks"`
	From           string `json:"from"`
	Through        string `json:"through"`
	// NotInReports marks an earlier Hub chain that is not in reports yet:
	// its blocks count as expected and none as indexed.
	NotInReports bool `json:"not_in_reports,omitempty"`
}

// hubLaunchDay is the UTC day of the first cosmoshub-1 block.
var hubLaunchDay = time.Date(2019, 3, 13, 0, 0, 0, 0, time.UTC)

// missingEras are the earlier Hub chains before the earliest one in
// reports, ascending.
func (st *legacyEraState) missingEras() []legacyEra {
	var out []legacyEra
	for _, e := range st.eras {
		if e.Era < st.earliest().Era {
			out = append(out, e)
		}
	}
	return out
}

// missingSegments places an earlier Hub chain that is not in reports in its
// calendar years. A chain whose first and last blocks fall in one year is
// that year whole. One placed across years needs its year boundaries, which
// are read only for a published chain; without them the chain is counted in
// the year the reports start, with no block count.
func missingSegments(e legacyEra, first legacyEra) []eraYearSegment {
	switch {
	case len(e.years) > 0:
		return e.years
	case e.placed() && e.Start.UTC().Year() == e.End.UTC().Year():
		return []eraYearSegment{{Year: e.Start.UTC().Year(), Lo: e.FirstHeight, Hi: e.ExportHeight, From: e.startDay(), Through: e.endDay(), ExpectedBlocks: e.ExportHeight - e.FirstHeight + 1}}
	}
	return []eraYearSegment{{Year: first.Start.UTC().Year()}}
}

func dayStringOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return dayString(t)
}

// withEras returns the coverage reports see: the cosmoshub-4 snapshot, plus
// the earlier Hub chains in reports. With none in reports it is snap itself,
// unchanged. Otherwise genesis_date moves to the first day of the earliest
// chain in reports, its years are added (a year that spans a restart is
// counted across both chains, with each chain's heights under chains), and
// its days count as covered when every one of its heights is imported.
// A year the chains before the earliest one in reports also ran in is
// partial: their blocks count as expected and none as indexed, and the days
// they ran are not covered. genesis_height, tip and indexed_range stay
// cosmoshub-4's.
func withEras(snap *IndexCoverage, st *legacyEraState) *IndexCoverage {
	if snap == nil || !st.active() {
		return snap
	}
	c := *snap
	c.earliestEra = st.earliest().ChainID
	c.GenesisDate = dayString(st.earliest().startDay())
	hub4 := map[int]YearCoverage{}
	for _, y := range snap.Years {
		hub4[y.Year] = y
	}
	type part struct {
		e       legacyEra
		seg     eraYearSegment
		missing bool
	}
	parts := map[int][]part{}
	var years []int
	add := func(e legacyEra, seg eraYearSegment, missing bool) {
		if _, ok := parts[seg.Year]; !ok {
			years = append(years, seg.Year)
		}
		parts[seg.Year] = append(parts[seg.Year], part{e, seg, missing})
	}
	if missing := st.missingEras(); len(missing) > 0 {
		c.missingFrom = hubLaunchDay
		if missing[0].placed() {
			c.missingFrom = missing[0].startDay()
		}
		c.missingChains = earlierChainsThan(c.earliestEra)
		for _, e := range missing {
			for _, seg := range missingSegments(e, st.earliest()) {
				add(e, seg, true)
			}
		}
	}
	for _, e := range st.effective {
		for _, seg := range e.years {
			add(e, seg, false)
		}
	}
	for _, y := range snap.Years {
		if _, ok := parts[y.Year]; !ok {
			years = append(years, y.Year)
		}
	}
	sort.Ints(years)
	var ranges []dayRange
	complete := snap.Complete
	c.Years = make([]YearCoverage, 0, len(years))
	for _, year := range years {
		h4, hasHub4 := hub4[year]
		var y YearCoverage
		var legacyRanges []dayRange
		var chains []YearChainCoverage
		partial := false
		for _, p := range parts[year] {
			if y.Year == 0 {
				y = YearCoverage{Year: year, firstDay: p.seg.From, Gaps: []CoverageGap{}}
			}
			if p.missing {
				partial = true
				y.ExpectedBlocks += p.seg.ExpectedBlocks
				chains = append(chains, YearChainCoverage{ChainID: p.e.ChainID, FirstHeight: p.seg.Lo, LastHeight: p.seg.Hi, ExpectedBlocks: p.seg.ExpectedBlocks, From: dayStringOrEmpty(p.seg.From), Through: dayStringOrEmpty(p.seg.Through), NotInReports: true})
				continue
			}
			indexed := overlap(p.e.spans, p.seg.Lo, p.seg.Hi)
			y.FirstHeight, y.LastHeight = p.seg.Lo, p.seg.Hi
			y.lastDay = p.seg.Through
			y.ExpectedBlocks += p.seg.ExpectedBlocks
			y.IndexedBlocks += indexed
			y.TxIndexedBlocks += indexed
			y.BlockEventsIndexedBlocks += indexed
			chains = append(chains, YearChainCoverage{ChainID: p.e.ChainID, FirstHeight: p.seg.Lo, LastHeight: p.seg.Hi, ExpectedBlocks: p.seg.ExpectedBlocks, IndexedBlocks: indexed, From: dayString(p.seg.From), Through: dayString(p.seg.Through)})
			if indexed == p.seg.ExpectedBlocks {
				legacyRanges = joinDayRanges(legacyRanges, []dayRange{{p.seg.From, p.seg.Through}})
			} else {
				complete = false
			}
		}
		if hasHub4 {
			if y.Year == 0 {
				y = h4
				y.Chains = []YearChainCoverage{hub4ChainCoverage(h4)}
			} else {
				// The year spans the cosmoshub-3 to 4 restart. Heights stay
				// cosmoshub-4's at the top level, as before the earlier
				// chains joined; chains lists both.
				y.FirstHeight, y.LastHeight = h4.FirstHeight, h4.LastHeight
				y.lastDay = h4.lastDay
				y.ExpectedBlocks += h4.ExpectedBlocks
				y.IndexedBlocks += h4.IndexedBlocks
				y.TxIndexedBlocks += h4.TxIndexedBlocks
				y.BlockEventsIndexedBlocks += h4.BlockEventsIndexedBlocks
				y.InProgress = h4.InProgress
				y.Gaps, y.GapCount = h4.Gaps, h4.GapCount
				y.ranges = joinDayRanges(legacyRanges, h4.ranges)
				chains = append(chains, hub4ChainCoverage(h4))
				y.Chains = chains
				finishEraYear(&y)
			}
		} else {
			y.ranges = legacyRanges
			y.Chains = chains
			finishEraYear(&y)
		}
		if partial {
			y.Complete, y.TxComplete, y.BlockEventsComplete = false, false, false
			y.PercentIndexed = min(y.PercentIndexed, 99.9)
			c.partialYears = append(c.partialYears, year)
		}
		ranges = joinDayRanges(ranges, y.ranges)
		c.Years = append(c.Years, y)
	}
	c.ranges = ranges
	c.Complete = complete
	c.Note = coverageNote(&c)
	return &c
}

func hub4ChainCoverage(y YearCoverage) YearChainCoverage {
	return YearChainCoverage{ChainID: apiHubChainID, FirstHeight: y.FirstHeight, LastHeight: y.LastHeight, ExpectedBlocks: y.ExpectedBlocks, IndexedBlocks: y.IndexedBlocks, From: dayString(y.firstDay), Through: dayString(y.lastDay)}
}

// finishEraYear sets the derived fields of a year built across chains.
func finishEraYear(y *YearCoverage) {
	y.Complete = y.IndexedBlocks == y.ExpectedBlocks
	y.TxComplete = y.TxIndexedBlocks == y.ExpectedBlocks
	y.BlockEventsComplete = y.BlockEventsIndexedBlocks == y.ExpectedBlocks
	y.PercentIndexed = floorPercent(y.IndexedBlocks, y.ExpectedBlocks)
	y.CoveredRangeCount = len(y.ranges)
	y.CoveredRanges = []CoverageRange{}
	for _, r := range y.ranges {
		if len(y.CoveredRanges) < coverageMaxListed {
			y.CoveredRanges = append(y.CoveredRanges, CoverageRange{dayString(r.From), dayString(r.Through)})
		}
	}
	y.EarliestCoveredDate, y.LatestCoveredDate = nil, nil
	if n := len(y.ranges); n > 0 {
		y.EarliestCoveredDate, y.LatestCoveredDate = dayPtr(y.ranges[0].From), dayPtr(y.ranges[n-1].Through)
	}
	if y.Gaps == nil {
		y.Gaps = []CoverageGap{}
	}
}

// earlierChainsThan names the Hub chains before chainID.
func earlierChainsThan(chainID string) string {
	var out []string
	for _, id := range []string{hub1ChainID, hub2ChainID, hub3ChainID} {
		if eraRank(id) < eraRank(chainID) {
			out = append(out, id)
		}
	}
	return strings.Join(out, " and ")
}

// eraCoverageNote is coverageNote while earlier Hub chains are in reports.
func eraCoverageNote(c *IndexCoverage) string {
	tail := ""
	if c.earliestEra != hub1ChainID {
		tail = fmt.Sprintf(" Earlier Hub chains (%s) are not in reports yet.", earlierChainsThan(c.earliestEra))
	}
	if len(c.partialYears) > 0 {
		var ys []string
		for _, y := range c.partialYears {
			ys = append(ys, fmt.Sprint(y))
		}
		tail += fmt.Sprintf(" %s is partial until they are: Hub activity from %s to %s genesis is not in reports.", strings.Join(ys, " and "), dayString(c.missingFrom), c.earliestEra)
	}
	return fmt.Sprintf("Every Cosmos Hub block from %s genesis (%s) to the %s tip is indexed, across the chain restarts.%s", c.earliestEra, c.GenesisDate, apiHubChainID, tail)
}

// eraGenesisNote is genesisNote while earlier Hub chains are in reports.
func eraGenesisNote(c *IndexCoverage) string {
	if c.earliestEra == hub1ChainID {
		return fmt.Sprintf("This period starts before the Cosmos Hub launched (%s genesis, %s), so this report starts at that day.", c.earliestEra, c.GenesisDate)
	}
	return fmt.Sprintf("This period starts before %s genesis (%s). Cosmos Hub activity before that day is on earlier Hub chains (%s), which are not in reports yet, so this report starts at %s genesis.", c.earliestEra, c.GenesisDate, earlierChainsThan(c.earliestEra), c.earliestEra)
}

// eraPartialWarning explains a period that reaches back into days the
// earlier Hub chains not in reports yet ran.
func eraPartialWarning(c *IndexCoverage) string {
	return fmt.Sprintf("This period reaches back before %s genesis (%s). Cosmos Hub activity from %s to that day is on earlier Hub chains (%s), which are not in reports yet, so it is missing from this report.", c.earliestEra, c.GenesisDate, dayString(c.missingFrom), c.missingChains)
}

// eraPreGenesisWarning is preGenesisWarning while earlier Hub chains are in
// reports.
func eraPreGenesisWarning(c *IndexCoverage) string {
	if c.earliestEra == hub1ChainID {
		return fmt.Sprintf("This period is entirely before the Cosmos Hub launched (%s genesis, %s), so there is no Hub activity to report.", c.earliestEra, c.GenesisDate)
	}
	return fmt.Sprintf("This period is entirely before %s genesis (%s). Earlier Hub chains (%s) are not in reports yet, so this report has no data for it.", c.earliestEra, c.GenesisDate, earlierChainsThan(c.earliestEra))
}

// coverageSnapshot is the coverage reports are judged against: the
// cosmoshub-4 scan with the earlier Hub chains in reports.
func (s *Server) coverageSnapshot() *IndexCoverage {
	sc := s.indexCoverage()
	if sc == nil {
		return nil
	}
	return withEras(sc.Snapshot(), s.legacyEras(apiMainnet))
}
