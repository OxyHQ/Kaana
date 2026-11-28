package pgerror_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/pgerror"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestReadOnlyMatchesTheReadOnlyRefusalThroughWrapping(t *testing.T) {
	refusal := &pgconn.PgError{Severity: "ERROR", Code: "25006", Message: "cannot execute INSERT in a read-only transaction"}
	for name, err := range map[string]error{
		"bare":         refusal,
		"wrapped":      fmt.Errorf("credential store: enqueuing settlement: %w", refusal),
		"wrappedTwice": fmt.Errorf("billing: database operation: %w", fmt.Errorf("tx: %w", refusal)),
		"joined":       errors.Join(context.DeadlineExceeded, refusal),
	} {
		if !pgerror.ReadOnly(err) {
			t.Errorf("%s: a read-only refusal was not recognised: %v", name, err)
		}
	}
}

func TestReadOnlyMatchesNothingElse(t *testing.T) {
	for name, err := range map[string]error{
		"nil":               nil,
		"plain":             errors.New("25006"),
		"uniqueViolation":   &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
		"serialization":     fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}),
		"adminShutdown":     &pgconn.PgError{Code: "57P01"},
		"readOnlyInMessage": &pgconn.PgError{Code: "XX000", Message: "cannot execute INSERT in a read-only transaction"},
		"deadline":          context.DeadlineExceeded,
	} {
		if pgerror.ReadOnly(err) {
			t.Errorf("%s: matched as a read-only refusal: %v", name, err)
		}
	}
}

func TestWaitOutReadOnlyRetriesOnlyTheReadOnlyRefusal(t *testing.T) {
	refusal := fmt.Errorf("credential store: registering rate card version v1: %w", &pgconn.PgError{Code: "25006"})
	calls := 0
	err := pgerror.WaitOutReadOnly(context.Background(), nil, "test", time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return refusal
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("a freeze that lifted was not waited out: calls=%d err=%v", calls, err)
	}

	other := &pgconn.PgError{Code: "23505"}
	calls = 0
	err = pgerror.WaitOutReadOnly(context.Background(), nil, "test", time.Millisecond, func(context.Context) error {
		calls++
		return other
	})
	if !errors.Is(err, other) || calls != 1 {
		t.Fatalf("a failure other than read-only was retried: calls=%d err=%v", calls, err)
	}
}

func TestWaitOutReadOnlyGivesUpWithTheRefusalWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	calls := 0
	err := pgerror.WaitOutReadOnly(ctx, nil, "test", 5*time.Millisecond, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "25006"}
	})
	if !pgerror.ReadOnly(err) || calls < 2 {
		t.Fatalf("calls=%d err=%v: want several attempts and the read-only refusal", calls, err)
	}
}

func TestRetryAfterSecondsIsFive(t *testing.T) {
	if got := pgerror.RetryAfterSeconds(); got != "5" {
		t.Fatalf("Retry-After=%q", got)
	}
}
