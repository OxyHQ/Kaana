package kaana_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
)

// The tests in this file pin the durable credential-attempt record across a
// request's same-route retries. On 2026-09-30/10-01 five production requests
// failed with `provider: recording credential attempt: ... provider credential
// attempt identity conflict`: OpenRouter answered 200 and then a rate-limit
// frame, Kaana retried the SAME deployment, and the retry's fresh walk numbered
// its credential attempt 0 again. The record of the first attempt already held
// (request, deployment, 0) with another observation time, PostgreSQL refused
// the second, and that bookkeeping refusal replaced the retry's good answer.

// attemptIdentity is the durable record's primary key.
type attemptIdentity struct {
	request    contract.RequestID
	deployment contract.DeploymentID
	index      int
}

// attemptLedger applies kaana_record_provider_credential_attempt's identity
// rule: an identical replay is accepted, the same identity with any other fact
// is refused.
type attemptLedger struct {
	mu      sync.Mutex
	rows    map[attemptIdentity]provider.CredentialAttempt
	ordered []provider.CredentialAttempt
	// refuse makes every write fail, as an unreachable store would.
	refuse bool
}

func (l *attemptLedger) ClaimCredentialRecovery(context.Context, contract.ProviderSlug, string, time.Time, time.Time) (provider.CredentialRecoveryDecision, error) {
	return provider.CredentialRecoveryUsable, nil
}

func (l *attemptLedger) RecordCredentialAttempt(_ context.Context, attempt provider.CredentialAttempt) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuse {
		return errors.New("ERROR: credential store unavailable (SQLSTATE 08006)")
	}
	identity := attemptIdentity{attempt.RequestID, attempt.DeploymentID, attempt.Index}
	if existing, ok := l.rows[identity]; ok {
		if existing.Provider != attempt.Provider || existing.KeyID != attempt.KeyID || existing.Outcome != attempt.Outcome ||
			existing.Evidence != attempt.Evidence || !existing.OccurredAt.Equal(attempt.OccurredAt) {
			return errors.New("ERROR: provider credential attempt identity conflict (SQLSTATE P0001)")
		}
		return nil
	}
	if l.rows == nil {
		l.rows = make(map[attemptIdentity]provider.CredentialAttempt)
	}
	l.rows[identity] = attempt
	l.ordered = append(l.ordered, attempt)
	return nil
}

func (l *attemptLedger) recorded() []provider.CredentialAttempt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]provider.CredentialAttempt(nil), l.ordered...)
}

// recordingOpenRouterAdapter is openRouterAdapter with its single key's
// attempts persisted to ledger.
func recordingOpenRouterAdapter(t *testing.T, target string, ledger *attemptLedger) provider.Adapter {
	t.Helper()
	adapter, err := openaicompat.New(openaicompat.Config{
		Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
		Declarations: []provider.KeyDeclaration{{KeyID: "openrouter-test", Secret: "openrouter-attempt-ledger-test-secret", Runtime: ledger}},
		HTTPClient:   &http.Client{Transport: redirectTransport{target: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// TestASameRouteRetryRecordsItsOwnCredentialAttempt replays the production
// failure: 200 + rate-limit frame, then a clean answer on the retry. The
// customer gets the answer, and each upstream exchange is its own durable
// attempt — two identities, never one identity claimed twice.
func TestASameRouteRetryRecordsItsOwnCredentialAttempt(t *testing.T) {
	server, credentials := openRouterRateLimitedOnce(t, 1)
	ledger := &attemptLedger{}
	request := lunaRequest()
	_, result := harness{
		deployments: lunaOnOpenRouter,
		adapters:    []provider.Adapter{recordingOpenRouterAdapter(t, server.URL, ledger)},
	}.run(t, request)

	if result.Failure != nil {
		t.Fatalf("the retry's answer was replaced by %+v", result.Failure)
	}
	if len(credentials()) != 2 {
		t.Fatalf("OpenRouter was called %d times; want the rate-limited attempt and its retry", len(credentials()))
	}
	attempts := ledger.recorded()
	if len(attempts) != 2 {
		t.Fatalf("the ledger holds %d credential attempts, want one per upstream exchange: %+v", len(attempts), attempts)
	}
	for index, attempt := range attempts {
		if attempt.Index != index || attempt.RequestID != request.Attribution.RequestID || attempt.DeploymentID != "dep_or_luna" ||
			attempt.KeyID != "openrouter-test" || attempt.Outcome != "accepted" || attempt.Evidence != "response_status" {
			t.Errorf("credential attempt %d = %+v", index, attempt)
		}
	}
}

// TestAFailedCredentialAttemptRecordNeverAbortsTheAnswer pins that the durable
// record is bookkeeping: with every write refused, the provider's answer still
// reaches the customer, and the lost record is logged with the request's
// identity for operators.
func TestAFailedCredentialAttemptRecordNeverAbortsTheAnswer(t *testing.T) {
	logs := captureDefaultLogger(t)
	server, credentials := openRouterRateLimitedOnce(t, 0)
	ledger := &attemptLedger{refuse: true}
	request := lunaRequest()
	events, result := harness{
		deployments: lunaOnOpenRouter,
		adapters:    []provider.Adapter{recordingOpenRouterAdapter(t, server.URL, ledger)},
	}.run(t, request)

	if result.Failure != nil {
		t.Fatalf("a refused bookkeeping write reached the customer as %+v", result.Failure)
	}
	if len(credentials()) != 1 {
		t.Fatalf("OpenRouter was called %d times; a lost record must not spend a second call", len(credentials()))
	}
	if len(eventsOfType(events, contract.EventError)) != 0 || events[len(events)-1].EventType() != contract.EventDone {
		t.Fatalf("the customer's stream did not complete cleanly: %v", events)
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
		t.Fatalf("report = %+v", result.Report)
	}

	lost := logs.matching("credential attempt not recorded")
	if len(lost) != 1 {
		t.Fatalf("logged %d lost credential attempts, want 1", len(lost))
	}
	if lost[0]["requestId"] != string(request.Attribution.RequestID) || lost[0]["deploymentId"] != "dep_or_luna" ||
		lost[0]["keyId"] != "openrouter-test" || lost[0]["outcome"] != "accepted" || lost[0]["error"] == "" {
		t.Errorf("the lost record was logged as %v", lost[0])
	}
}

// capturedLogs collects records written through slog's default logger.
type capturedLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturedLogs) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturedLogs) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record.Clone())
	return nil
}
func (c *capturedLogs) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturedLogs) WithGroup(string) slog.Handler      { return c }

func (c *capturedLogs) matching(message string) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []map[string]string
	for _, record := range c.records {
		if record.Message != message {
			continue
		}
		attrs := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value.String()
			return true
		})
		matched = append(matched, attrs)
	}
	return matched
}

func captureDefaultLogger(t *testing.T) *capturedLogs {
	t.Helper()
	logs := &capturedLogs{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}
