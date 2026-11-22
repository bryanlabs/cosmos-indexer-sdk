package taxapi

import "time"

// This is execution telemetry, never a financial acceptance receipt or a
// bound on the undiscovered graph. Telemetry never includes wallet identifiers.
type commissionFlowProgress struct {
	Version                     string `json:"version"`
	Build                       string `json:"build"`
	Validator                   string `json:"validator"`
	Phase                       string `json:"phase"`
	Sequence                    int    `json:"sequence"`
	ElapsedMilliseconds         int64  `json:"elapsed_ms"`
	AttemptedWallets            int    `json:"attempted_wallets"`
	LoadedWallets               int    `json:"loaded_wallets"`
	ClosingReadCompletedWallets int    `json:"closing_read_completed_wallets"`
	// Archive counters are job-cumulative, not attributed to one root. Minus one means unavailable.
	JobArchiveCallAttempts int64 `json:"job_archive_call_attempts"`
	JobHeaderCacheReuses   int64 `json:"job_header_cache_reuses"`
	PendingEvents          int   `json:"pending_events"`
	AppliedQueuedEvents    int   `json:"applied_queued_events"`
	// How far through [From, AsOf) the applied events have reached, by block
	// time; -1 before the first. Later months hold more wallets, so it runs
	// ahead of the work done.
	EventTimeFraction float64 `json:"event_time_fraction"`
	DiscoveryComplete bool    `json:"discovery_complete"`
	// -1 means unknown, including every phase before discovery completes.
	ClosingRemaining int `json:"closing_remaining"`
	// What the read-ahead did with the queued transfers it looked at, and how
	// many wallet openings it served or the trace read itself.
	AheadCounts       map[string]int `json:"ahead_counts,omitempty"`
	ReadAheadOpenings int64          `json:"read_ahead_openings"`
	InlineOpenings    int64          `json:"inline_openings"`
}

// readAheadOpenings is how many openings the trace took from its read-ahead
// and how many it read itself; both are zero without a read-ahead.
func (l *dbFlowLoader) readAheadOpenings() (int64, int64) {
	if l.pre == nil {
		return 0, 0
	}
	return l.pre.used.Load(), l.pre.missed.Load()
}

func (t *flowTrace) reportProgress(phase, _ string, closingRemaining int) {
	if t.progress == nil {
		return
	}
	t.progressSequence++
	p := commissionFlowProgress{
		Validator: t.Validator, Phase: phase, Sequence: t.progressSequence,
		ElapsedMilliseconds: time.Since(t.started).Milliseconds(),
		AttemptedWallets:    len(t.attempted), LoadedWallets: len(t.order),
		ClosingReadCompletedWallets: t.closingReadCompletedWallets,
		PendingEvents:               t.queue.Len(), AppliedQueuedEvents: t.appliedQueuedEvents,
		DiscoveryComplete: t.discoveryComplete, ClosingRemaining: closingRemaining,
		EventTimeFraction: -1,
	}
	if len(t.ahead.counts) > 0 {
		p.AheadCounts = make(map[string]int, len(t.ahead.counts))
		for k, v := range t.ahead.counts {
			p.AheadCounts[k] = v
		}
	}
	if l, ok := t.load.(interface{ readAheadOpenings() (int64, int64) }); ok {
		p.ReadAheadOpenings, p.InlineOpenings = l.readAheadOpenings()
	}
	if span := t.AsOf.Sub(t.From); !t.frontier.IsZero() && span > 0 {
		p.EventTimeFraction = float64(t.frontier.Sub(t.From)) / float64(span)
	}
	t.progress(p)
}
