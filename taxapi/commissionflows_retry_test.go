package taxapi

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestFlowReadRetriesLockAborts(t *testing.T) {
	defer func(b func(int) time.Duration) { flowReadBackoff = b }(flowReadBackoff)
	flowReadBackoff = func(int) time.Duration { return 0 }
	for _, code := range []string{"40P01", "55P03", "40001"} {
		calls := 0
		v, err := flowRead(func() ([]int, error) {
			calls++
			if calls < 3 {
				// As readWalletInput returns it: the driver error inside the
				// private read failure.
				return []int{calls}, &CommissionReadFailure{Phase: "snapshot", wrappedErr: fmt.Errorf("fees: %w", &pgconn.PgError{Code: code})}
			}
			return []int{7, 8}, nil
		})
		if err != nil || calls != 3 || len(v) != 2 || v[0] != 7 || v[1] != 8 {
			t.Fatalf("%s: got %v %v after %d calls, want the third read's rows", code, v, err, calls)
		}
	}
}

func TestFlowReadKeepsOtherErrorsAndStops(t *testing.T) {
	defaultBackoff := flowReadBackoff
	defer func() { flowReadBackoff = defaultBackoff }()
	flowReadBackoff = func(int) time.Duration { return 0 }
	for _, err := range []error{errors.New("other"), &pgconn.PgError{Code: "57014"}, &pgconn.PgError{Code: "42P01"}} {
		calls := 0
		_, got := flowRead(func() (int, error) { calls++; return 0, err })
		if calls != 1 || !errors.Is(got, err) {
			t.Fatalf("%v: %d calls, error %v; want one call and the same error", err, calls, got)
		}
	}
	calls := 0
	_, got := flowRead(func() (int, error) { calls++; return 0, &pgconn.PgError{Code: "40P01"} })
	if calls != flowReadAttempts || !flowReadAborted(got) {
		t.Fatalf("%d calls, error %v; want %d calls and the deadlock", calls, got, flowReadAttempts)
	}
	total := time.Duration(0)
	for a := 0; a+1 < flowReadAttempts; a++ {
		total += defaultBackoff(a)
	}
	if total > 2*time.Minute {
		t.Fatalf("retries wait %v in all, over two minutes", total)
	}
}
