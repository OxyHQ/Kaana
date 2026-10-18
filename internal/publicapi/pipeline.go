package publicapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// authorization is what one request asks Oxy to grant, in content-free terms.
type authorization struct {
	operation       contract.GrantOperation
	target          *contract.GrantTarget
	facts           contract.GrantRequestFacts
	body            []byte
	endpoint        string
	labels          contract.RequestLabels
	clientRequestID *string
	// onBehalfOf is a delegated user the body names (OpenAI's `user`);
	// X-Oxy-User-Id, when present, must agree with it.
	onBehalfOf *contract.UserID
}

// admission is a request Oxy granted and Kaana resolved: everything the
// execution and the settlement need, and nothing of the caller's credential.
type admission struct {
	requestID  contract.RequestID
	receivedAt time.Time
	grant      *contract.Grant
	plan       *plan
	labels     contract.RequestLabels
	clientID   *string
	endpoint   string
}

// bodyDigest is the hash a grant binds: SHA-256 of the exact request bytes.
// Oxy never sees the body; it echoes the hash into the grant, and Kaana
// executes only the bytes it hashed.
func bodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// admit authenticates nothing itself: it reads the credential, applies the
// local rate limit, asks Oxy for a grant and resolves it into routes. Every
// refusal is a public error decided before the first byte.
func (s *Server) admit(ctx context.Context, r *http.Request, requestID contract.RequestID, receivedAt time.Time, ask authorization) (*admission, *contract.PublicError) {
	bearer, ok := bearerFrom(r)
	if !ok {
		return nil, contract.NewPublicError(requestID, "authentication_failed", contract.PublicMessage("authentication_failed"))
	}
	if wait, allowed := s.limiter.allow(bearer.digest); !allowed {
		return nil, contract.NewPublicError(requestID, "rate_limited", contract.PublicMessage("rate_limited")).WithRetryAfter(int(wait.Milliseconds()) + 1)
	}
	user, ok := onBehalfOf(r)
	if !ok {
		return nil, invalid(requestID, HeaderOnBehalfOf, "X-Oxy-User-Id is 1 to 64 characters without whitespace")
	}
	if ask.onBehalfOf != nil {
		if user != nil && *user != *ask.onBehalfOf {
			return nil, invalid(requestID, "user", "user and X-Oxy-User-Id name different users")
		}
		user = ask.onBehalfOf
	}
	key, ok := idempotencyKey(r)
	if !ok {
		return nil, invalid(requestID, HeaderIdempotencyKey, "Idempotency-Key is 1 to 255 characters on one line")
	}
	request := contract.InferenceAuthorizationRequest{
		SchemaVersion:    contract.AuthorizationSchemaVersion,
		Kind:             contract.AuthorizationKindInference,
		Credential:       bearer.value,
		RequestID:        requestID,
		Operation:        ask.operation,
		Target:           ask.target,
		Facts:            ask.facts,
		IdempotencyKey:   key,
		OnBehalfOfUserID: user,
		BodySHA256:       bodyDigest(ask.body),
		Client: contract.AuthorizationClient{
			Endpoint: ask.endpoint, ClientRequestID: ask.clientRequestID,
			ReceivedAt: contract.NewTimestamp(receivedAt), Labels: ask.labels,
		},
	}
	grant, failure := s.authorizer.Authorize(ctx, request)
	if failure != nil {
		return nil, failure
	}
	resolved, failure := resolveGrant(grant, s.inventory.Current(), s.funding, s.now())
	if failure != nil {
		s.releaseUnserved(ctx, grant)
		return nil, failure
	}
	return &admission{
		requestID: requestID, receivedAt: receivedAt, grant: grant, plan: resolved,
		labels: ask.labels, clientID: ask.clientRequestID, endpoint: ask.endpoint,
	}, nil
}

// releaseUnserved records that a grant served nothing and named no route.
// A settlement must name a deployment, and none was chosen, so nothing is
// written: Oxy releases the hold when it expires (expireReservations). The log
// line is the operator's trace of a hold that will expire unused.
func (s *Server) releaseUnserved(_ context.Context, grant *contract.Grant) {
	s.logger.Warn("a granted request resolved to no route and settles by expiry", "requestId", grant.RequestID, "grantId", grant.GrantID)
}

// request is the executor's envelope for a granted request: the grant's
// attribution, its resolved routes and its idempotency key.
func (a *admission) request(modality contract.Modality, format contract.APIFormat, input contract.Input) contract.Request {
	return contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution:   a.grant.Attribution(),
		Target:        a.plan.target,
		Modality:      modality,
		Input:         input,
		Client: contract.ClientRequestMetadata{
			APIFormat: format, Endpoint: a.endpoint, ClientRequestID: a.clientID,
			ReceivedAt: contract.NewTimestamp(a.receivedAt), Labels: a.labels,
		},
		IdempotencyKey:   a.grant.IdempotencyKey,
		RoutingPolicy:    a.grant.RoutingPolicy,
		AuthorizedRoutes: a.plan.routes,
	}
}

// execute runs a granted request and settles it, exactly once, whatever the
// outcome. The executor leaves the provider cost to the settlement, which
// writes both in one transaction.
func (s *Server) execute(ctx context.Context, a *admission, request *contract.Request, sink kaana.Sink) kaana.Result {
	result := s.executor.Execute(kaana.DeferCostRecording(ctx), request, sink)
	if result.Failure != nil {
		s.logFailure(a, result)
	}
	// The settlement is written beside the response, not before it: the
	// customer's last frame does not wait on Kaana's database, and Drain
	// waits for every settlement still being written before the task stops.
	s.settling.Add(1)
	go func() {
		defer s.settling.Done()
		s.settle(context.WithoutCancel(ctx), a, result)
	}()
	return result
}

// logFailure records a failed execution in content-free terms: the code, how
// many upstream attempts ran (each may have been billed) and, for a decisions
// answer Kaana refused, the finite validation reason. Never the failure's
// message, which can carry upstream text.
func (s *Server) logFailure(a *admission, result kaana.Result) {
	attributes := []any{
		"requestId", a.requestID, "grantId", a.grant.GrantID,
		"code", result.Failure.Code, "retryable", result.Failure.Retryable,
		"upstreamAttempts", len(result.UpstreamCost.Attempts),
	}
	if result.Failure.Code == contract.CodeProviderError {
		if reason := provider.DecisionResponseDiagnostic(result.Failure.Message); reason != "" {
			attributes = append(attributes, "decisionResponseValidation", reason)
		}
	}
	s.logger.Warn("inference request failed", attributes...)
}

// Drain waits for every settlement still being written, or for ctx.
func (s *Server) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.settling.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// settle writes the settlement of an executed grant.
func (s *Server) settle(ctx context.Context, a *admission, result kaana.Result) {
	report := result.Report
	if report == nil {
		report = a.unservedReport(result.Failure, s.now())
	}
	_ = s.settler.Settle(ctx, a.grant, *report, a.plan.stepOf(report.DeploymentID), result.UpstreamCost)
}

// unservedReport is the report of a granted request that produced no report
// of its own (refused before any attempt): nothing consumed, naming the first
// route the grant resolved to, as a refused realtime session does.
func (a *admission) unservedReport(failure *contract.Error, now time.Time) *contract.UsageReport {
	first := a.plan.routes[0]
	outcome := contract.OutcomeFailed
	if failure != nil && failure.Code == contract.CodeCancelled {
		outcome = contract.OutcomeCancelled
	}
	started := contract.NewTimestamp(a.receivedAt)
	completed := contract.NewTimestamp(now)
	if now.Before(a.receivedAt) {
		completed = started
	}
	return &contract.UsageReport{
		SchemaVersion: contract.UsageReportSchemaVersion, RequestID: a.requestID, Attribution: a.grant.Attribution(),
		Outcome: outcome, Units: []contract.UsageQuantity{}, UsageSource: contract.UsageEstimated,
		ResolvedModelReference: first.ModelReference, ServingProvider: first.Provider, DeploymentID: first.DeploymentID,
		StartedAt: started, CompletedAt: completed,
	}
}

// errAnswerTooLarge stops a non-streamed answer that outgrew its buffer.
var errAnswerTooLarge = errors.New("publicapi: the answer exceeds what a whole response may buffer")

// errClientGone marks a write the client is no longer reading, which the
// executor treats as a cancellation rather than a provider failure.
func errClientGone(err error) error {
	return fmt.Errorf("%w: %w", kaana.ErrClientGone, err)
}
