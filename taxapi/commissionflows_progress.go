package taxapi

import "time"

// This is execution telemetry, never a financial acceptance receipt or a
// bound on the undiscovered graph. Telemetry never includes wallet identifiers.
type commissionFlowProgress struct {
	Version             string `json:"version"`
	Build               string `json:"build"`
	Validator           string `json:"validator"`
	Phase               string `json:"phase"`
	Sequence            int    `json:"sequence"`
	ElapsedMilliseconds int64  `json:"elapsed_ms"`
	AttemptedWallets    int    `json:"attempted_wallets"`
	LoadedWallets       int    `json:"loaded_wallets"`
	PendingEvents       int    `json:"pending_events"`
	AppliedQueuedEvents int    `json:"applied_queued_events"`
	DiscoveryComplete   bool   `json:"discovery_complete"`
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
		PendingEvents: t.queue.Len(), AppliedQueuedEvents: t.appliedQueuedEvents,
		DiscoveryComplete: t.discoveryComplete, ClosingRemaining: closingRemaining,
	}
	t.progress(p)
}
