package taxapi

import (
	"errors"
	"time"
)

// flowReadAttempts bounds how often the trace reruns one read that Postgres
// aborted for a deadlock or a lock timeout. Schema changes on the tables a
// trace reads (a trigger install takes their locks) abort its reads this way;
// the trace only reads, and the same read over the same finished window
// returns the same rows, so rerunning it leaves the result unchanged.
const flowReadAttempts = 8

var flowReadBackoff = func(attempt int) time.Duration {
	return min(250*time.Millisecond<<attempt, 30*time.Second)
}

// flowReadAborted is true for a read Postgres ended for a deadlock (40P01),
// a lock timeout (55P03) or a serialization failure (40001).
func flowReadAborted(err error) bool {
	var state interface{ SQLState() string }
	if !errors.As(err, &state) {
		return false
	}
	switch state.SQLState() {
	case "40P01", "55P03", "40001":
		return true
	}
	return false
}

func flowRead[T any](read func() (T, error)) (T, error) {
	var v T
	var err error
	for attempt := 0; attempt < flowReadAttempts; attempt++ {
		if v, err = read(); err == nil || !flowReadAborted(err) {
			return v, err
		}
		if attempt+1 < flowReadAttempts {
			time.Sleep(flowReadBackoff(attempt))
		}
	}
	return v, err
}
