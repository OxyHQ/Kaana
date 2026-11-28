// Package pgerror classifies PostgreSQL refusals Kaana answers in one way
// everywhere, whichever store or surface met them.
package pgerror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ReadOnlySQLState is read_only_sql_transaction: the database accepted the
// statement but refuses every write, as it does while it is frozen with
// default_transaction_read_only for a migration or a failover.
const ReadOnlySQLState = "25006"

// ReadOnlyRetryAfter is how long a caller is told to wait out a read-only
// database before retrying, and how long a background loop waits before its
// next write. A freeze is planned to last under a minute, so a few seconds
// keeps the retries few without holding work back once it lifts.
const ReadOnlyRetryAfter = 5 * time.Second

// ReadOnly reports whether err, through any wrapping, is PostgreSQL refusing a
// write because the database or transaction is read-only.
func ReadOnly(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == ReadOnlySQLState
}

// RetryAfterSeconds is ReadOnlyRetryAfter as an HTTP Retry-After value.
func RetryAfterSeconds() string {
	return strconv.Itoa(int(ReadOnlyRetryAfter / time.Second))
}

// WaitOutReadOnly runs write until it does anything other than meet a
// read-only database, waiting every between refusals, and logs the first
// refusal once at warn. Any other outcome, success or failure, is returned at
// once. When ctx ends first, the last refusal is returned, still matching
// ReadOnly, so the caller fails as it would have without waiting.
func WaitOutReadOnly(ctx context.Context, logger *slog.Logger, operation string, every time.Duration, write func(context.Context) error) error {
	for refusals := 0; ; refusals++ {
		err := write(ctx)
		if !ReadOnly(err) {
			return err
		}
		if refusals == 0 && logger != nil {
			logger.Warn("the database is read-only; waiting it out", "operation", operation, "retryInMs", every.Milliseconds(), "errorType", "database_read_only")
		}
		timer := time.NewTimer(every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("pgerror: %s: the database stayed read-only: %w", operation, err)
		case <-timer.C:
		}
	}
}
