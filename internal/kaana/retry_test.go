package kaana_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// The tests in this file pin how Kaana itself absorbs a transient upstream
// failure, so that its callers never have to: a failure that delivered nothing
// is retried on the same route, then moved to the next authorized route, and
// a failure after delivery is never retried anywhere.

/* -------------------------------------------------------------------------- */
/*  Fixtures                                                                  */
/* -------------------------------------------------------------------------- */

const lunaReference contract.ModelReference = "openai/gpt-6-luna@observed-2026-09-30"

// lunaOnOpenRouter is the production shape that motivated same-route retries:
// a model with exactly ONE authorized route, so failover alone cannot help.
const lunaOnOpenRouter = `{"deploymentId":"dep_or_luna","provider":"openrouter","modelReference":"openai/gpt-6-luna@observed-2026-09-30","upstreamModelId":"openai/gpt-6-luna","regions":[],"current":true}`

func lunaRequest() *contract.Request {
	request := baseRequest()
	reference := lunaReference
	request.Target.ModelReference = &reference
	request.Client.APIFormat, request.Client.Endpoint = contract.APIFormatChatCompletions, "/v1/chat/completions"
	request.AuthorizedRoutes = openRouterRoute("dep_or_luna", reference)
	return request
}

// openRouterRateLimitedOnce answers the first `failures` calls exactly as
// OpenRouter did in production on 2026-09-30 — HTTP 200, its keep-alive
// comment, then an error frame naming an upstream rate limit — and serves a
// normal answer after that. It records the credential every call carried.
func openRouterRateLimitedOnce(t *testing.T, failures int) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mutex       sync.Mutex
		credentials []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		call := len(credentials)
		mutex.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		if call <= failures {
			_, _ = io.WriteString(w, `data: {"error":{"code":429,"message":"openai/gpt-6-luna is temporarily rate-limited upstream. Please retry shortly.","metadata":{"error_type":"rate_limit_exceeded"}},"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]string(nil), credentials...)
	}
}

func rateLimited(slug contract.ProviderSlug, retryAfterMs int) provider.ErrUpstream {
	return provider.ErrUpstream{
		Code: contract.CodeRateLimited, Category: contract.UpstreamRateLimit, RetryAfterMs: retryAfterMs,
		Detail: string(slug) + " rate-limited this request part-way through it", Passthrough: &contract.ProviderErrorPassthrough{Provider: slug},
	}
}

// startedThenFailed is an attempt that got as far as the upstream's 200 — its
// adapter called Start and reported the input it had been billed for — and
// then failed before a single output token.
func startedThenFailed(slug contract.ProviderSlug, failure error) func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
	return func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 11}}
		if err := out.Usage(units, contract.UsageProviderReported); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported}, failure
	}
}

// failsThenServes fails its first `failures` attempts with startedThenFailed
// and serves every attempt after that.
func failsThenServes(slug contract.ProviderSlug, failures int, failure error) *scriptedAdapter {
	var (
		mutex sync.Mutex
		seen  int
	)
	fail := startedThenFailed(slug, failure)
	serve := succeedingAdapter(slug, 5).stream
	return &scriptedAdapter{slug: slug, stream: func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		mutex.Lock()
		seen++
		attempt := seen
		mutex.Unlock()
		if attempt <= failures {
			return fail(ctx, call, out)
		}
		return serve(ctx, call, out)
	}}
}

func assertOneStartFirstAndMonotonic(t *testing.T, events []contract.StreamEvent) {
	t.Helper()
	if len(eventsOfType(events, contract.EventStart)) != 1 {
		t.Fatalf("the customer saw %d start events", len(eventsOfType(events, contract.EventStart)))
	}
	for index, event := range events {
		if event.Sequence() != index {
			t.Errorf("event %d (%s) carries sequence %d; a discarded attempt must leave no gap", index, event.EventType(), event.Sequence())
		}
		if event.EventType() == contract.EventStart {
			break
		}
		if event.EventType() != contract.EventRouteSwitch {
			t.Errorf("a %s event precedes the start event", event.EventType())
		}
	}
}

/* -------------------------------------------------------------------------- */
/*  The production failure, end to end                                        */
/* -------------------------------------------------------------------------- */

// TestAnOpenRouterRateLimitInsideA200IsRetriedOnItsOnlyRoute replays the
// 2026-09-30 failure through the real OpenRouter adapter: a 200 whose stream
// carries a rate-limit error frame before any content, on a model with one
// authorized route. Kaana retries it on the same route with the same key, and
// the customer sees one clean answer — never the failed attempt.
func TestAnOpenRouterRateLimitInsideA200IsRetriedOnItsOnlyRoute(t *testing.T) {
	server, credentials := openRouterRateLimitedOnce(t, 1)
	events, result := harness{
		deployments: lunaOnOpenRouter,
		adapters:    []provider.Adapter{openRouterAdapter(t, server.URL)},
	}.run(t, lunaRequest())

	if result.Failure != nil {
		t.Fatalf("a rate limit that cleared on retry reached the customer: %+v", result.Failure)
	}
	sent := credentials()
	if len(sent) != 2 {
		t.Fatalf("OpenRouter was called %d times; want the rate-limited attempt and its retry", len(sent))
	}
	if sent[0] != sent[1] || sent[0] == "" {
		t.Error("the retry did not reuse the single-key pool's key; a rate limit is not exhaustion")
	}

	assertOneStartFirstAndMonotonic(t, events)
	if len(eventsOfType(events, contract.EventError)) != 0 || len(eventsOfType(events, contract.EventRouteSwitch)) != 0 {
		t.Fatalf("the retry was visible to the customer: %d errors, %d switches",
			len(eventsOfType(events, contract.EventError)), len(eventsOfType(events, contract.EventRouteSwitch)))
	}
	start := events[0].(*contract.StreamStartEvent)
	if start.ServingProvider != "openrouter" || start.ResolvedModelReference != lunaReference {
		t.Errorf("start names %s/%s", start.ServingProvider, start.ResolvedModelReference)
	}
	if last := events[len(events)-1]; last.EventType() != contract.EventDone {
		t.Errorf("the stream ends with %s", last.EventType())
	}

	report := result.Report
	if report == nil || report.Outcome != contract.OutcomeCompleted || report.RouteSwitches != 0 || report.DeploymentID != "dep_or_luna" {
		t.Fatalf("report = %+v", report)
	}
	if report.TimeToFirstTokenMs == nil {
		t.Error("the served attempt's time to first token was not reported")
	}

	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 2 {
		t.Fatalf("the cost record holds %d attempts, want one row per attempt", len(attempts))
	}
	for index, attempt := range attempts {
		if attempt.AttemptIndex != index || attempt.DeploymentID != "dep_or_luna" || attempt.KeyID != "openrouter-test" {
			t.Errorf("attempt %d = index %d deployment %s key %q", index, attempt.AttemptIndex, attempt.DeploymentID, attempt.KeyID)
		}
	}
	if attempts[0].Served || attempts[0].Telemetry.Outcome != providercost.AttemptFailed || attempts[0].Telemetry.FailureCode != contract.CodeRateLimited {
		t.Errorf("the rate-limited attempt is recorded as %+v served=%t", attempts[0].Telemetry, attempts[0].Served)
	}
	if !attempts[1].Served || attempts[1].Telemetry.Outcome != providercost.AttemptSucceeded {
		t.Errorf("the retry is recorded as %+v served=%t", attempts[1].Telemetry, attempts[1].Served)
	}
}

// TestWithoutSameRouteRetryTheProductionFailureReachesTheCustomer is the
// positive control for the test above: the identical upstream with retries
// disabled fails the request, so the success above is the retry's doing. It
// also pins what the customer sees of a failure that delivered nothing: the
// error alone, without a start event for an attempt that never served.
func TestWithoutSameRouteRetryTheProductionFailureReachesTheCustomer(t *testing.T) {
	server, credentials := openRouterRateLimitedOnce(t, 1)
	events, result := harness{
		deployments: lunaOnOpenRouter,
		adapters:    []provider.Adapter{openRouterAdapter(t, server.URL)},
		retry:       &noRetries,
	}.run(t, lunaRequest())

	if len(credentials()) != 1 {
		t.Fatalf("with retries disabled OpenRouter was called %d times", len(credentials()))
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeRateLimited {
		t.Fatalf("the control did not fail with the rate limit: %+v", result.Failure)
	}
	if len(events) != 1 || events[0].EventType() != contract.EventError || events[0].Sequence() != 0 {
		t.Fatalf("a failure that delivered nothing produced %d events, first %v", len(events), events)
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomeFailed {
		t.Fatalf("report = %+v", result.Report)
	}
}

/* -------------------------------------------------------------------------- */
/*  Retry, then failover                                                      */
/* -------------------------------------------------------------------------- */

// TestAPreOutputFailureIsRetriedThenFailsOverWithOneStart covers a failure
// after the upstream's 200 — the adapter called Start and reported usage — on
// a primary that keeps failing. The primary is retried up to the cap, then the
// request moves to the next authorized route, and the customer sees one route
// switch, one start naming the backup, and none of the primary's usage.
func TestAPreOutputFailureIsRetriedThenFailsOverWithOneStart(t *testing.T) {
	primary := &scriptedAdapter{slug: "stub", stream: startedThenFailed("stub", overloaded("stub"))}
	backup := succeedingAdapter("backup", 4)

	events, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{primary, backup},
	}.run(t, authorizedRequest())

	if result.Failure != nil {
		t.Fatalf("the request failed: %+v", result.Failure)
	}
	if want := 1 + fastRetries.MaxRetriesPerRoute; primary.attempts() != want || backup.attempts() != 1 {
		t.Fatalf("attempts primary=%d backup=%d, want %d and 1", primary.attempts(), backup.attempts(), want)
	}
	assertOneStartFirstAndMonotonic(t, events)
	switches := eventsOfType(events, contract.EventRouteSwitch)
	if len(switches) != 1 || events[0].EventType() != contract.EventRouteSwitch {
		t.Fatalf("%d route switches; the stream opens with %s", len(switches), events[0].EventType())
	}
	if to := switches[0].(*contract.StreamRouteSwitchEvent).Detail.ToDeploymentID; to == nil || *to != "dep_b" {
		t.Errorf("the switch names %v", to)
	}
	if start := eventsOfType(events, contract.EventStart)[0].(*contract.StreamStartEvent); start.ServingProvider != "backup" {
		t.Errorf("start names %q, not the route that served", start.ServingProvider)
	}
	for _, event := range eventsOfType(events, contract.EventUsage) {
		if usage := event.(*contract.StreamUsageEvent); usage.DeploymentID != "dep_b" {
			t.Errorf("the customer received usage from %s, an attempt that never served them", usage.DeploymentID)
		}
	}
	if result.Report.RouteSwitches != 1 || result.Report.DeploymentID != "dep_b" {
		t.Errorf("report = %+v", result.Report)
	}

	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 4 {
		t.Fatalf("%d cost rows, want three primary attempts and the backup", len(attempts))
	}
	for index, attempt := range attempts {
		if attempt.AttemptIndex != index {
			t.Errorf("row %d carries attempt index %d", index, attempt.AttemptIndex)
		}
		if served := index == 3; attempt.Served != served {
			t.Errorf("row %d served=%t", index, attempt.Served)
		}
		if index < 3 && (attempt.DeploymentID != "dep_a" || len(attempt.Units) == 0) {
			t.Errorf("failed row %d lost its deployment or its measured units: %+v", index, attempt)
		}
	}
}

// TestASecondAttemptThatServesNamesTheSameRouteAndCountsNoSwitch: a retry that
// succeeds on the primary is not a route switch, in the stream or the receipt.
func TestASecondAttemptThatServesNamesTheSameRouteAndCountsNoSwitch(t *testing.T) {
	primary := failsThenServes("stub", 1, rateLimited("stub", 0))
	backup := succeedingAdapter("backup", 4)

	events, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{primary, backup},
	}.run(t, authorizedRequest())

	if result.Failure != nil || primary.attempts() != 2 || backup.attempts() != 0 {
		t.Fatalf("failure=%+v primary=%d backup=%d", result.Failure, primary.attempts(), backup.attempts())
	}
	assertOneStartFirstAndMonotonic(t, events)
	if len(eventsOfType(events, contract.EventRouteSwitch)) != 0 || result.Report.RouteSwitches != 0 {
		t.Fatal("a same-route retry was reported as a route switch")
	}
	if start := events[0].(*contract.StreamStartEvent); start.ServingProvider != "stub" {
		t.Errorf("start names %q", start.ServingProvider)
	}
}

/* -------------------------------------------------------------------------- */
/*  What is never retried                                                     */
/* -------------------------------------------------------------------------- */

// TestAFailureAfterOutputIsNeverRetried: once a token reached the customer, a
// retry would splice two answers. The partial attempt settles alone.
func TestAFailureAfterOutputIsNeverRetried(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "half an answer"); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 3}}, UsageSource: contract.UsageProviderReported}, rateLimited("stub", 5_000)
	}}
	// A budget that could absorb the provider's Retry-After, so only the
	// delivered output stands between this failure and a retry.
	policy := kaana.RetryPolicy{MaxRetriesPerRoute: 2, Backoff: time.Millisecond, Budget: time.Minute}

	began := time.Now()
	events, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).run(t, baseRequest())

	if adapter.attempts() != 1 {
		t.Fatalf("a failure after delivered output was retried: %d attempts", adapter.attempts())
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("a partial answer that cannot be retried waited %s for a retry before settling", elapsed)
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomePartial || !result.UpstreamCost.Attempts[0].Served {
		t.Fatalf("report=%+v cost=%+v", result.Report, result.UpstreamCost)
	}
	kinds := []contract.StreamEventType{}
	for _, event := range events {
		kinds = append(kinds, event.EventType())
	}
	if len(kinds) < 3 || kinds[0] != contract.EventStart || kinds[1] != contract.EventDelta || kinds[len(kinds)-1] != contract.EventError {
		t.Fatalf("the partial stream is %v", kinds)
	}
}

// TestOnlyTransientDeploymentFailuresAreRetriedOnTheSameRoute pins the
// classification from both directions on a single route: each transient class
// is retried to the cap, and everything else is attempted exactly once.
func TestOnlyTransientDeploymentFailuresAreRetriedOnTheSameRoute(t *testing.T) {
	upstream := func(code contract.ErrorCode, category contract.UpstreamErrorCategory) provider.ErrUpstream {
		return provider.ErrUpstream{Code: code, Category: category, Detail: "scripted", Passthrough: &contract.ProviderErrorPassthrough{Provider: "stub"}}
	}
	persistent := rateLimited("stub", 0)
	persistent.RecursOnThisRoute = true
	retried := 1 + fastRetries.MaxRetriesPerRoute

	for _, testCase := range []struct {
		name     string
		failure  error
		attempts int
	}{
		{"rate limited", rateLimited("stub", 0), retried},
		{"overloaded", overloaded("stub"), retried},
		{"timed out", upstream(contract.CodeProviderTimeout, contract.UpstreamTimeout), retried},
		{"server error", upstream(contract.CodeProviderError, contract.UpstreamServerError), retried},

		{"invalid request", customerFault("stub"), 1},
		{"model not found", upstream(contract.CodeModelNotFound, contract.UpstreamInvalidReq), 1},
		{"permission denied", upstream(contract.CodePermissionDenied, contract.UpstreamInvalidReq), 1},
		{"content filter", upstream(contract.CodeUpstreamContentFiltered, contract.UpstreamContentFilter), 1},
		{"platform credential refused", upstream(contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication), 1},
		{"platform billing refused", upstream(contract.CodeProviderBillingRefused, contract.UpstreamQuota), 1},
		{"every key retired", upstream(contract.CodeDeploymentUnavailable, contract.UpstreamQuota), 1},
		{"provider error nobody classified", upstream(contract.CodeProviderError, contract.UpstreamUnknown), 1},
		{"unclassified error", errors.New("scripted: something nobody classified"), 1},
		{"customer's own BYOK throttle", provider.ErrCustomerUpstream{Failure: rateLimited("stub", 0)}, 1},
		{"a rate limit that recurs on this route", persistent, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			adapter := &scriptedAdapter{stream: startedThenFailed("stub", testCase.failure)}
			events, result := execute(t, adapter, baseRequest())
			if adapter.attempts() != testCase.attempts {
				t.Fatalf("%d attempts, want %d", adapter.attempts(), testCase.attempts)
			}
			if len(result.UpstreamCost.Attempts) != testCase.attempts {
				t.Errorf("%d cost rows for %d attempts", len(result.UpstreamCost.Attempts), testCase.attempts)
			}
			if result.Failure == nil || len(eventsOfType(events, contract.EventStart)) != 0 {
				t.Errorf("failure=%+v starts=%d", result.Failure, len(eventsOfType(events, contract.EventStart)))
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/*  Bounds                                                                    */
/* -------------------------------------------------------------------------- */

func TestSameRouteRetriesAreBoundedByTheAttemptCapAndTheWaitBudget(t *testing.T) {
	t.Run("the attempt cap", func(t *testing.T) {
		policy := kaana.RetryPolicy{MaxRetriesPerRoute: 1, Backoff: time.Millisecond, Budget: time.Second}
		adapter := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 0))}
		_, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).run(t, baseRequest())
		if adapter.attempts() != 2 || result.Failure == nil || result.Failure.Code != contract.CodeRateLimited {
			t.Fatalf("attempts=%d failure=%+v", adapter.attempts(), result.Failure)
		}
	})

	t.Run("a Retry-After is waited for", func(t *testing.T) {
		policy := kaana.RetryPolicy{MaxRetriesPerRoute: 2, Backoff: time.Millisecond, Budget: time.Second}
		adapter := failsThenServes("stub", 1, rateLimited("stub", 60))
		began := time.Now()
		_, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).run(t, baseRequest())
		if result.Failure != nil || adapter.attempts() != 2 {
			t.Fatalf("attempts=%d failure=%+v", adapter.attempts(), result.Failure)
		}
		if waited := time.Since(began); waited < 60*time.Millisecond {
			t.Errorf("the retry came %s after a 60ms Retry-After", waited)
		}
	})

	t.Run("a Retry-After beyond the budget is not waited for", func(t *testing.T) {
		policy := kaana.RetryPolicy{MaxRetriesPerRoute: 2, Backoff: time.Millisecond, Budget: 100 * time.Millisecond}
		adapter := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 10_000))}
		began := time.Now()
		_, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).run(t, baseRequest())
		if adapter.attempts() != 1 || result.Failure == nil {
			t.Fatalf("attempts=%d failure=%+v", adapter.attempts(), result.Failure)
		}
		if elapsed := time.Since(began); elapsed > 5*time.Second {
			t.Errorf("a Retry-After past the budget extended the request to %s", elapsed)
		}
		if result.Failure.RetryAfterMs == nil || *result.Failure.RetryAfterMs != 10_000 {
			t.Errorf("the provider's own hint did not reach the customer: %+v", result.Failure)
		}
	})

	t.Run("the budget spans retries", func(t *testing.T) {
		// Each wait is 40ms; the budget holds one of them, not two.
		policy := kaana.RetryPolicy{MaxRetriesPerRoute: 5, Backoff: time.Millisecond, Budget: 70 * time.Millisecond}
		adapter := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 40))}
		_, _ = (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).run(t, baseRequest())
		if adapter.attempts() != 2 {
			t.Fatalf("attempts=%d, want the first and one retry within the budget", adapter.attempts())
		}
	})
}

// TestCancellationDuringBackoffEndsTheRequestAtOnce: a customer who hangs up
// while Kaana waits to retry is not kept waiting for the Retry-After, and the
// failed attempt settles as a cancellation with nothing written to them.
func TestCancellationDuringBackoffEndsTheRequestAtOnce(t *testing.T) {
	policy := kaana.RetryPolicy{MaxRetriesPerRoute: 2, Backoff: time.Millisecond, Budget: time.Minute}
	adapter := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 30_000))}
	executor := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, retry: &policy}).build(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()

	var events []contract.StreamEvent
	began := time.Now()
	result := executor.Execute(ctx, baseRequest(), func(event contract.StreamEvent) error {
		events = append(events, event)
		return nil
	})

	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("the cancelled request waited %s for its retry", elapsed)
	}
	if adapter.attempts() != 1 {
		t.Fatalf("a cancelled request was retried: %d attempts", adapter.attempts())
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeCancelled {
		t.Fatalf("failure = %+v", result.Failure)
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomeCancelled {
		t.Fatalf("report = %+v", result.Report)
	}
	if len(events) != 0 {
		t.Errorf("%d events were written to a customer who had hung up", len(events))
	}
	if len(result.UpstreamCost.Attempts) != 1 || result.UpstreamCost.Attempts[0].Telemetry.Outcome != providercost.AttemptFailed {
		t.Errorf("the rate-limited attempt is not on the cost record as the failure it was: %+v", result.UpstreamCost.Attempts)
	}
}

/* -------------------------------------------------------------------------- */
/*  Breakers                                                                  */
/* -------------------------------------------------------------------------- */

type manualClock struct {
	mutex sync.Mutex
	at    time.Time
}

func (c *manualClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.at
}

func (c *manualClock) advance(by time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.at = c.at.Add(by)
}

// TestARetryNeverBypassesTheBreaker: a half-open breaker admits one trial.
// When that trial fails, the breaker reopens, and the retry — which goes
// through Admit like any attempt — is refused rather than sent. The control
// is the same failure on a closed breaker, which IS retried.
func TestARetryNeverBypassesTheBreaker(t *testing.T) {
	clock := &manualClock{at: time.Now()}
	breakers := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1, Cooldown: time.Minute}, clock.now)

	control := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 0))}
	controlPolicy := kaana.RetryPolicy{MaxRetriesPerRoute: 2, Backoff: time.Millisecond, Budget: time.Second}
	closed := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 100}, clock.now)
	_, _ = (harness{deployments: oneDeployment, adapters: []provider.Adapter{control}, rotation: closed, retry: &controlPolicy}).run(t, baseRequest())
	if control.attempts() != 3 {
		t.Fatalf("the control on a closed breaker made %d attempts", control.attempts())
	}

	// Open the breaker, then let its cooldown pass so it is half-open.
	permit, admitted := breakers.Admit("dep_test")
	if !admitted {
		t.Fatal("a fresh breaker refused")
	}
	permit.Failed()
	if _, admitted := breakers.Admit("dep_test"); admitted {
		t.Fatal("the breaker did not open, so this test measures nothing")
	}
	clock.advance(2 * time.Minute)

	adapter := &scriptedAdapter{stream: startedThenFailed("stub", rateLimited("stub", 0))}
	events, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, rotation: breakers, retry: &controlPolicy}).run(t, baseRequest())

	if adapter.attempts() != 1 {
		t.Fatalf("a half-open deployment received %d attempts from one request; it admits one trial", adapter.attempts())
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeRateLimited {
		t.Fatalf("the customer is told %+v, not the trial's own failure", result.Failure)
	}
	if len(eventsOfType(events, contract.EventStart)) != 0 {
		t.Error("a start event reached the customer for an attempt that never served")
	}
	if _, admitted := breakers.Admit("dep_test"); admitted {
		t.Error("the failed trial did not reopen the breaker")
	}
}

/* -------------------------------------------------------------------------- */
/*  Deferred start                                                            */
/* -------------------------------------------------------------------------- */

// TestAnAnswerWithNoOutputStillStartsOnce: with the start event held until
// output, an attempt that completes with none still writes start, its held
// usage and empty delta, then done — in order, once.
func TestAnAnswerWithNoOutputStillStartsOnce(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, ""); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
		if err := out.Usage(units, contract.UsageProviderReported); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}

	events, result := execute(t, adapter, baseRequest())

	if result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
		t.Fatalf("failure=%+v report=%+v", result.Failure, result.Report)
	}
	want := []contract.StreamEventType{contract.EventStart, contract.EventDelta, contract.EventUsage, contract.EventDone}
	if len(events) != len(want) {
		t.Fatalf("%d events, want %v", len(events), want)
	}
	for index, event := range events {
		if event.EventType() != want[index] || event.Sequence() != index {
			t.Errorf("event %d is %s seq %d, want %s seq %d", index, event.EventType(), event.Sequence(), want[index], index)
		}
	}
	if !result.UpstreamCost.Attempts[0].Served {
		t.Error("an empty answer that completed is not marked served")
	}
}
