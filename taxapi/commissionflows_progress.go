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
	if span := t.AsOf.Sub(t.From); !t.frontier.IsZero() && span > 0 {
		p.EventTimeFraction = float64(t.frontier.Sub(t.From)) / float64(span)
	}
	t.progress(p)
}
