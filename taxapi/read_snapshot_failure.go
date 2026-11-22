package taxapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

// CommissionReadFailure retains exact parameters only in private immutable
// evidence. Error deliberately excludes both the address and driver text.
type CommissionReadFailure struct {
	Address            string
	Start              time.Time
	End                time.Time
	Phase              string
	Cause              string
	PhaseElapsedMillis int64
	TotalElapsedMillis int64
	PhaseMillis        map[string]int64
	wrappedErr         error
}

func (e *CommissionReadFailure) Error() string {
	return fmt.Sprintf("private read wallet_sha256=%x window=[%s,%s) phase=%s cause=%s phase_ms=%d cumulative_ms=%d phases_ms=%v", sha256.Sum256([]byte(e.Address)), e.Start.UTC().Format(time.RFC3339Nano), e.End.UTC().Format(time.RFC3339Nano), e.Phase, e.Cause, e.PhaseElapsedMillis, e.TotalElapsedMillis, e.PhaseMillis)
}
func (e *CommissionReadFailure) Unwrap() error { return e.wrappedErr }

type privateReadPhases struct {
	failure      CommissionReadFailure
	started      time.Time
	phaseStarted time.Time
}

func (s *Server) privateReadPhases(addresses []string, start, end time.Time, phase string) *privateReadPhases {
	if !s.readOnlyLedger {
		return nil
	}
	now := time.Now()
	started := s.readSnapshotStarted
	if started.IsZero() {
		started = now
	}
	address := ""
	if len(addresses) == 1 {
		address = addresses[0]
	}
	return &privateReadPhases{failure: CommissionReadFailure{Address: address, Start: start, End: end, Phase: phase, PhaseMillis: map[string]int64{}}, started: started, phaseStarted: now}
}

func (p *privateReadPhases) next(phase string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.failure.PhaseMillis[p.failure.Phase] += now.Sub(p.phaseStarted).Milliseconds()
	p.failure.Phase = phase
	p.phaseStarted = now
}
func (p *privateReadPhases) wrap(err error) error {
	if p == nil || err == nil {
		return err
	}
	var existing *CommissionReadFailure
	if errors.As(err, &existing) {
		return err
	}
	p.failure.PhaseElapsedMillis = time.Since(p.phaseStarted).Milliseconds()
	p.failure.TotalElapsedMillis = time.Since(p.started).Milliseconds()
	p.failure.PhaseMillis[p.failure.Phase] += p.failure.PhaseElapsedMillis
	p.failure.Cause = privateReadCause(err)
	p.failure.wrappedErr = err
	return &p.failure
}

func privateReadCause(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		if len(code) == 5 {
			valid := true
			for _, c := range code {
				valid = valid && (c >= '0' && c <= '9' || c >= 'A' && c <= 'Z')
			}
			if valid {
				return "sqlstate_" + code
			}
		}
	}
	return "other"
}
