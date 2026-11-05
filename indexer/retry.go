package indexer

import (
	"errors"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const transientAttempts = 6

// Parallel indexers upserting the same lookup rows can deadlock, and Postgres
// then aborts one of the transactions. Rerunning that write succeeds, so it is
// retried here instead of counting as a failed write.
func retryTransient(write func() error) error {
	var err error
	for attempt := 0; attempt < transientAttempts; attempt++ {
		if err = write(); err == nil || !isTransient(err) {
			return err
		}
		backoff := time.Duration(100<<attempt) * time.Millisecond
		time.Sleep(backoff/2 + time.Duration(rand.Int63n(int64(backoff))))
	}
	return err
}

func isTransient(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}
