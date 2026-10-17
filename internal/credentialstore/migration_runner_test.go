package credentialstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationRunnerTimeoutsAndRetries(t *testing.T) {
	for body, want := range map[string]time.Duration{
		"SELECT 1;": migrationStatementTimeout,
		"-- oxy:statement-timeout=600s\nSELECT 1;": 600 * time.Second,
	} {
		if got, err := migrationStatementTimeoutFor("9999", body); err != nil || got != want {
			t.Errorf("%q: got %s, %v; want %s", body, got, err, want)
		}
	}
	for _, body := range []string{
		"-- oxy:statement-timeout=10m\n",
		"-- oxy:statement-timeout=0s\n",
		"-- oxy:statement-timeout=3601s\n",
		"-- oxy:statement-timeout=60s\n-- oxy:statement-timeout=90s\n",
	} {
		if _, err := migrationStatementTimeoutFor("9999", body); err == nil {
			t.Errorf("%q was accepted", body)
		}
	}

	for code, want := range map[string]bool{"55P03": true, "40P01": true, "57014": false, "23505": false} {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		if got := isLockContention(err); got != want {
			t.Errorf("isLockContention(%s) = %v", code, got)
		}
	}

	almostOne := func() float64 { return 0.999999 }
	for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 40: 30 * time.Second} {
		if got := lockRetryDelay(attempt, almostOne); got > ceiling || got < ceiling*99/100 {
			t.Errorf("attempt %d waits up to %s, want %s", attempt, got, ceiling)
		}
	}
}
