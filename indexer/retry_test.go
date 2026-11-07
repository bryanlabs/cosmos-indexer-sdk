package indexer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryTransientRetriesDeadlocks(t *testing.T) {
	calls := 0
	err := retryTransient(func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40P01"})
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("got err %v after %d calls, want success after 3", err, calls)
	}
}

func TestRetryTransientReturnsOtherErrorsAtOnce(t *testing.T) {
	calls := 0
	want := &pgconn.PgError{Code: "23505"}
	err := retryTransient(func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("got err %v after %d calls, want the unique violation after 1", err, calls)
	}
}
