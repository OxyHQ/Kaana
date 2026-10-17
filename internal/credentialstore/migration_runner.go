package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The migration runner's lock discipline, the same contract @oxy.so/db 1.2.0
// gives every Oxy TypeScript backend (oxy#1778):
//
//   - One transaction per migration. A migration's locks are released when it
//     commits, instead of being held while every later pending migration runs.
//     A failure leaves the earlier migrations of the run applied, so every
//     migration must stand on its own (they already do: each is re-checked
//     against the ledger before it is applied).
//   - lock_timeout on every statement, so a migration that cannot take its lock
//     gives up instead of queueing every reader of the table behind its wait. A
//     migration may still raise it for itself with SET LOCAL.
//   - statement_timeout on every statement. A migration that needs longer says
//     so in its own file: `-- oxy:statement-timeout=600s`.
//   - A lock timeout (55P03) or deadlock (40P01) rolls the migration back and
//     retries it from the top, with full jitter over an exponential backoff.
const (
	migrationLockTimeout      = 3 * time.Second
	migrationStatementTimeout = 120 * time.Second
	migrationMaxTimeout       = time.Hour
	migrationLockAttempts     = 8
	migrationRetryBase        = time.Second
	migrationRetryCeiling     = 30 * time.Second
)

const createMigrationLedger = `
	CREATE TABLE IF NOT EXISTS kaana_schema_migrations (
		version TEXT PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`

var statementTimeoutMarker = regexp.MustCompile(`(?m)^--\s*oxy:statement-timeout=(.*)$`)
var statementTimeoutValue = regexp.MustCompile(`^([1-9][0-9]{0,3})s$`)

// migrationStatementTimeoutFor reads a migration's own statement timeout.
// An illegible or out-of-range marker is an error, refused before any
// connection is opened, never silently replaced by the default.
func migrationStatementTimeoutFor(version, body string) (time.Duration, error) {
	markers := statementTimeoutMarker.FindAllStringSubmatch(body, -1)
	switch len(markers) {
	case 0:
		return migrationStatementTimeout, nil
	case 1:
	default:
		return 0, fmt.Errorf("credential store: migration %s declares oxy:statement-timeout more than once", version)
	}
	value := statementTimeoutValue.FindStringSubmatch(markers[0][1])
	if value == nil {
		return 0, fmt.Errorf("credential store: migration %s oxy:statement-timeout=%q is not whole seconds like 600s", version, markers[0][1])
	}
	seconds, _ := strconv.Atoi(value[1])
	timeout := time.Duration(seconds) * time.Second
	if timeout > migrationMaxTimeout {
		return 0, fmt.Errorf("credential store: migration %s oxy:statement-timeout exceeds %s", version, migrationMaxTimeout)
	}
	return timeout, nil
}

// isLockContention reports a failure that a retry of the whole migration can
// cure: lock_not_available (a lock_timeout fired) or deadlock_detected.
func isLockContention(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "55P03" || pgErr.Code == "40P01"
}

// lockRetryDelay is full jitter over 1 s doubling to a 30 s ceiling: attempt 1
// waits up to 1 s, attempt 2 up to 2 s, and so on. random returns [0, 1).
func lockRetryDelay(attempt int, random func() float64) time.Duration {
	ceiling := migrationRetryCeiling
	if attempt < 6 {
		if scaled := migrationRetryBase << (attempt - 1); scaled < ceiling {
			ceiling = scaled
		}
	}
	return time.Duration(random() * float64(ceiling))
}

// Migrate applies schema and grants with the dedicated migrator identity. Role
// creation and password rotation are a separate master-authority one-shot.
func (p *Postgres) Migrate(ctx context.Context) error {
	migrations := schemaMigrations()
	timeouts := make([]time.Duration, len(migrations))
	for index, migration := range migrations {
		timeout, err := migrationStatementTimeoutFor(migration.version, migration.body)
		if err != nil {
			return err
		}
		timeouts[index] = timeout
	}

	if err := p.inMigrationTransaction(ctx, "ledger", migrationStatementTimeout, func(tx migrationExecutor) error {
		if _, err := tx.Exec(ctx, createMigrationLedger); err != nil {
			return fmt.Errorf("credential store: creating migration ledger: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	for index, migration := range migrations {
		if err := p.inMigrationTransaction(ctx, migration.version, timeouts[index], func(tx migrationExecutor) error {
			return applyMigration(ctx, tx, migration.version, migration.body)
		}); err != nil {
			return err
		}
	}
	return nil
}

// inMigrationTransaction runs apply in its own transaction under the runner's
// timeouts, retrying lock contention with backoff.
func (p *Postgres) inMigrationTransaction(ctx context.Context, name string, statementTimeout time.Duration, apply func(migrationExecutor) error) error {
	for attempt := 1; ; attempt++ {
		err := p.migrationAttempt(ctx, statementTimeout, apply)
		if err == nil {
			return nil
		}
		if !isLockContention(err) {
			return err
		}
		if attempt == migrationLockAttempts {
			return fmt.Errorf("credential store: migration %s could not take its locks in %d attempts: %w", name, attempt, err)
		}
		timer := time.NewTimer(lockRetryDelay(attempt, rand.Float64))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *Postgres) migrationAttempt(ctx context.Context, statementTimeout time.Duration, apply func(migrationExecutor) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("credential store: beginning migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT pg_catalog.set_config('lock_timeout', $1, true), pg_catalog.set_config('statement_timeout', $2, true)`,
		strconv.FormatInt(migrationLockTimeout.Milliseconds(), 10),
		strconv.FormatInt(statementTimeout.Milliseconds(), 10),
	); err != nil {
		return fmt.Errorf("credential store: setting migration timeouts: %w", err)
	}
	if err := apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("credential store: committing migration: %w", err)
	}
	return nil
}
