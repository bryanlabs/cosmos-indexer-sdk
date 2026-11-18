package taxapi

import (
	"fmt"
	"sort"
	"strings"
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
}

// withEras returns the coverage reports see: the cosmoshub-4 snapshot, plus
// the earlier Hub chains in reports. With none in reports it is snap itself,
// unchanged. Otherwise genesis_date moves to the first day of the earliest
// chain in reports, its years are added (a year that spans a restart is
// counted across both chains, with each chain's heights under chains), and
// its days count as covered when every one of its heights is imported.
// genesis_height, tip and indexed_range stay cosmoshub-4's.
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
		e   legacyEra
		seg eraYearSegment
	}
	parts := map[int][]part{}
	var years []int
	for _, e := range st.effective {
		for _, seg := range e.years {
			if _, ok := parts[seg.Year]; !ok {
				years = append(years, seg.Year)
			}
			parts[seg.Year] = append(parts[seg.Year], part{e, seg})
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
		for _, p := range parts[year] {
			indexed := overlap(p.e.spans, p.seg.Lo, p.seg.Hi)
			if y.Year == 0 {
				y = YearCoverage{Year: year, firstDay: p.seg.From, Gaps: []CoverageGap{}}
			}
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
	return fmt.Sprintf("Every Cosmos Hub block from %s genesis (%s) to the %s tip is indexed, across the chain restarts.%s", c.earliestEra, c.GenesisDate, apiHubChainID, tail)
}

// eraGenesisNote is genesisNote while earlier Hub chains are in reports.
func eraGenesisNote(c *IndexCoverage) string {
	if c.earliestEra == hub1ChainID {
		return fmt.Sprintf("This period starts before the Cosmos Hub launched (%s genesis, %s), so this report starts at that day.", c.earliestEra, c.GenesisDate)
	}
	return fmt.Sprintf("This period starts before %s genesis (%s). Cosmos Hub activity before that day is on earlier Hub chains (%s), which are not in reports yet, so this report starts at %s genesis.", c.earliestEra, c.GenesisDate, earlierChainsThan(c.earliestEra), c.earliestEra)
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
