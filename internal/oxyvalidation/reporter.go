// Package oxyvalidation reports provider-credential verdicts from Kaana's
// runtime back to Oxy. It owns only the narrow service-principal exchange and
// the closed validation vocabulary; it never receives provider secret bytes.
package oxyvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const (
	defaultQueueSize      = 256
	defaultTimeout        = 5 * time.Second
	maxResponseBytes      = 64 << 10
	maxDeliveredSelectors = 4096
	canonicalOxyAPIOrigin = "https://api.oxy.so"
)

// State is Oxy's closed credential-validation state vocabulary.
type State string

const (
	StateValid        State = "valid"
	StateInvalid      State = "invalid"
	StateInconclusive State = "inconclusive"
)

// FailureCode is Oxy's closed, non-secret validation reason vocabulary.
type FailureCode string

const (
	FailureUnauthorized FailureCode = "unauthorized"
	FailureForbidden    FailureCode = "forbidden"
	FailureNotFound     FailureCode = "not_found"
	FailureRateLimited  FailureCode = "rate_limited"
	FailureNetwork      FailureCode = "network"
	FailureUnknown      FailureCode = "unknown"
)

// Verdict selects one exact Oxy connection generation. It intentionally has
// no provider error message or secret-derived field.
type Verdict struct {
	OperationID        contract.KaanaCredentialOperationID
	ApplicationID      contract.ApplicationID
	Provider           contract.ProviderSlug
	OwnerAccountID     contract.AccountID
	ConnectionID       string
	CredentialHandle   string
	CredentialRevision int64
	Environment        contract.Environment
	DeploymentID       contract.DeploymentID
	State              State
	FailureCode        FailureCode
}

// Submitter is the executor's non-blocking validation-report boundary.
type Submitter interface {
	Submit(Verdict)
}

// Minter mints a short-lived Oxy service token for the identity this process
// can prove, rather than for a secret it holds (Oxy ADR 0026;
// internal/workloadidentity is the implementation).
//
// It is declared here, next to the one thing that uses it, and it takes the Oxy
// origin as an argument rather than owning one. Both are deliberate: the origin
// rule below is a security decision (an attestation handed to an origin that did
// not issue its nonce can be relayed into a Kaana token) and this package must
// stay the only place that decides it.
type Minter interface {
	Mint(ctx context.Context, origin *url.URL, audience contract.OxyAudience) (token string, expiresIn time.Duration, err error)
}

// Config provides Kaana's attested Oxy workload identity.
type Config struct {
	BaseURL string
	// WorkloadMinter is this process's attested identity, or nil when it has
	// none to attest. Never a nil pointer inside a non-nil interface: the caller
	// assigns it only on success, because "absent" has to be testable as nil.
	WorkloadMinter Minter
	Environment    contract.Environment
	Client         *http.Client
	Logger         *slog.Logger
	QueueSize      int
	Timeout        time.Duration
}

// Reporter queues verdicts off the inference request path and sends them with
// a cached, short-lived Oxy service token.
type Reporter struct {
	baseURL     *url.URL
	environment contract.Environment
	workload    Minter
	client      *http.Client
	logger      *slog.Logger
	timeout     time.Duration

	mu        sync.Mutex
	closed    bool
	queue     chan Verdict
	done      chan struct{}
	pending   map[Verdict]struct{}
	delivered map[verdictSelector]verdictState

	tokenMu sync.Mutex
	tokens  map[contract.OxyAudience]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

type verdictSelector struct {
	operationID   contract.KaanaCredentialOperationID
	applicationID contract.ApplicationID
	connectionID  string
	handle        string
	revision      int64
	environment   contract.Environment
	deploymentID  contract.DeploymentID
}

type verdictState struct {
	state       State
	failureCode FailureCode
}

// New builds and starts a bounded reporter. A production or staging service
// credential is sent only to Oxy's exact canonical origin. Development may use
// an explicit loopback origin so tests and a local Oxy stack do not weaken the
// credential boundary used by deployed environments. An attestation is bound by
// that same rule, and for a sharper reason: an origin that relayed a genuine Oxy
// challenge and collected the signature could mint a Kaana token with it.
func New(config Config) (*Reporter, error) {
	if config.WorkloadMinter == nil {
		return nil, errors.New("oxy validation: an attested workload identity is required")
	}
	if !validEnvironment(config.Environment) {
		return nil, errors.New("oxy validation: Kaana service principal environment is required")
	}
	baseURL, err := parseBaseURL(config.BaseURL, config.Environment)
	if err != nil {
		return nil, err
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	queueSize := config.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reporter := &Reporter{
		baseURL: baseURL, environment: config.Environment,
		workload: config.WorkloadMinter,
		client:   &copyClient, logger: logger, timeout: timeout,
		queue: make(chan Verdict, queueSize), done: make(chan struct{}),
		pending: make(map[Verdict]struct{}), delivered: make(map[verdictSelector]verdictState),
	}
	go reporter.run()
	return reporter, nil
}

// Submit queues a verdict without adding Oxy latency to the inference stream.
// A full queue drops the redundant hint rather than blocking paid work; the
// failure is observable without logging tenant identifiers or secret material.
func (r *Reporter) Submit(verdict Verdict) {
	if !validVerdict(verdict) {
		r.logger.Error("customer credential validation verdict was invalid", "errorType", "validation_contract")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if verdict.Environment != r.environment {
		r.logger.Error("customer credential validation environment does not match the Kaana service principal", "errorType", "validation_principal")
		return
	}
	selector := selectorFor(verdict)
	state := stateFor(verdict)
	if _, queued := r.pending[verdict]; queued || r.delivered[selector] == state {
		return
	}
	select {
	case r.queue <- verdict:
		r.pending[verdict] = struct{}{}
	default:
		r.logger.Error("customer credential validation queue is full", "errorType", "validation_queue_full")
	}
}

// Close stops accepting new verdicts and waits for the bounded queue to drain.
func (r *Reporter) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Reporter) run() {
	defer close(r.done)
	for verdict := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		err := r.report(ctx, verdict)
		cancel()
		r.mu.Lock()
		delete(r.pending, verdict)
		if err == nil {
			r.rememberDelivered(selectorFor(verdict), stateFor(verdict))
		}
		r.mu.Unlock()
		if err != nil {
			r.logger.Error("customer credential validation report failed", "errorType", "validation_callback")
		}
	}
}

// rememberDelivered bounds the success dedupe cache independently from the
// request rate. A process can see arbitrarily many customer generations over
// its lifetime; retaining each selector forever would turn a correctness
// optimization into an unbounded multi-tenant memory sink. Eviction is safe
// because Oxy records the same exact-generation verdict idempotently.
func (r *Reporter) rememberDelivered(selector verdictSelector, state verdictState) {
	if _, present := r.delivered[selector]; !present && len(r.delivered) >= maxDeliveredSelectors {
		for candidate := range r.delivered {
			delete(r.delivered, candidate)
			break
		}
	}
	r.delivered[selector] = state
}

func selectorFor(verdict Verdict) verdictSelector {
	return verdictSelector{
		operationID: verdict.OperationID, applicationID: verdict.ApplicationID,
		connectionID: verdict.ConnectionID, handle: verdict.CredentialHandle,
		revision: verdict.CredentialRevision, environment: verdict.Environment,
		deploymentID: verdict.DeploymentID,
	}
}

func stateFor(verdict Verdict) verdictState {
	return verdictState{state: verdict.State, failureCode: verdict.FailureCode}
}

func (r *Reporter) report(ctx context.Context, verdict Verdict) error {
	token, err := r.serviceToken(ctx, contract.OxyAPIAudience, false)
	if err != nil {
		return err
	}
	status, err := r.sendVerdict(ctx, token, verdict)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		token, err = r.serviceToken(ctx, contract.OxyAPIAudience, true)
		if err != nil {
			return err
		}
		status, err = r.sendVerdict(ctx, token, verdict)
		if err != nil {
			return err
		}
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("oxy validation: callback returned HTTP %d", status)
	}
	return nil
}

// ServiceToken shares the dedicated Kaana principal with operational publishers:
// a token for Oxy's own API (oxy-api).
func (r *Reporter) ServiceToken(ctx context.Context) (string, error) {
	return r.serviceToken(ctx, contract.OxyAPIAudience, false)
}

// ControlToken is the same principal's token for Oxy's /internal/kaana/v1/*,
// the grant and settlement API behind https://kaana.ai/v1. Oxy refuses an
// oxy-api token there, so the public API never borrows ServiceToken.
func (r *Reporter) ControlToken(ctx context.Context) (string, error) {
	return r.serviceToken(ctx, contract.OxyKaanaControlAudience, false)
}

// serviceToken returns the cached token for an audience, or mints one.
//
// One cache for both identities, and it is here rather than in either minter: a
// token is a token whichever way it was proved, `tokenMu` is what stops two
// verdicts minting two of them at once, and the one-minute margin is for clock
// drift between this process and Oxy — serving a token that expires in the next
// second is the same outage as serving an expired one. Keyed by audience,
// because a token for one of Oxy's resource servers is refused by the other.
func (r *Reporter) serviceToken(ctx context.Context, audience contract.OxyAudience, force bool) (string, error) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if cached, ok := r.tokens[audience]; ok && !force && time.Now().Add(time.Minute).Before(cached.expiresAt) {
		return cached.token, nil
	}
	token, lifetime, err := r.mint(ctx, audience)
	if err != nil {
		return "", err
	}
	if r.tokens == nil {
		r.tokens = make(map[contract.OxyAudience]cachedToken)
	}
	r.tokens[audience] = cachedToken{token: token, expiresAt: time.Now().Add(lifetime)}
	return token, nil
}

// mint uses only the process's registered workload identity. Oxy validates its
// live binding on both exchange and callback; failures never fall back to keys.
func (r *Reporter) mint(ctx context.Context, audience contract.OxyAudience) (string, time.Duration, error) {
	return r.workload.Mint(ctx, r.baseURL, audience)
}

func (r *Reporter) sendVerdict(ctx context.Context, token string, verdict Verdict) (int, error) {
	var payload any = struct {
		CredentialHandle   string      `json:"credentialHandle"`
		CredentialRevision int64       `json:"credentialRevision"`
		State              State       `json:"state"`
		FailureCode        FailureCode `json:"failureCode,omitempty"`
	}{CredentialHandle: verdict.CredentialHandle, CredentialRevision: verdict.CredentialRevision,
		State: verdict.State, FailureCode: verdict.FailureCode}
	endpoint := r.endpoint("inference/provider-connections/" + url.PathEscape(verdict.ConnectionID) + "/validation")
	if verdict.OperationID != "" {
		var failureCode *contract.KaanaCredentialValidationFailureCode
		if verdict.FailureCode != "" {
			value := contract.KaanaCredentialValidationFailureCode(verdict.FailureCode)
			failureCode = &value
		}
		payload = contract.KaanaCredentialValidationOutcome{
			SchemaVersion: 1, OperationID: verdict.OperationID,
			ApplicationID: verdict.ApplicationID, Provider: verdict.Provider,
			OwnerAccountID: verdict.OwnerAccountID, ConnectionID: verdict.ConnectionID,
			Environment:        verdict.Environment,
			CredentialHandle:   contract.KaanaCredentialHandle(verdict.CredentialHandle),
			CredentialRevision: verdict.CredentialRevision, DeploymentID: verdict.DeploymentID,
			State:       contract.KaanaCredentialValidationOutcomeState(verdict.State),
			FailureCode: failureCode,
		}
		endpoint = r.endpoint("inference/provider-connections/" + url.PathEscape(verdict.ConnectionID) + "/validation-bootstrap/outcome")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("oxy validation: posting verdict: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	return response.StatusCode, nil
}

func (r *Reporter) endpoint(relative string) string {
	copyURL := *r.baseURL
	copyURL.Path = path.Join(strings.TrimSuffix(copyURL.Path, "/"), relative)
	return copyURL.String()
}

func parseBaseURL(raw string, environment contract.Environment) (*url.URL, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return nil, errors.New("oxy validation: Oxy API base URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("oxy validation: Oxy API base URL must be a plain origin")
	}
	if raw == canonicalOxyAPIOrigin {
		return parsed, nil
	}
	if environment != contract.EnvironmentDevelopment {
		return nil, errors.New("oxy validation: deployed service credentials require the canonical Oxy API origin")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return nil, errors.New("oxy validation: development Oxy API base URL must be an explicit loopback origin")
	}
	return parsed, nil
}

func validVerdict(verdict Verdict) bool {
	if !validOpaqueID(verdict.ConnectionID, 128) || !validCredentialHandle(verdict.CredentialHandle) ||
		verdict.CredentialRevision <= 0 || verdict.CredentialRevision > 1<<53-1 || !validEnvironment(verdict.Environment) {
		return false
	}
	bootstrap := verdict.OperationID != ""
	if bootstrap {
		if !validOpaqueID(string(verdict.OperationID), 128) || !validOpaqueID(string(verdict.ApplicationID), 64) ||
			!verdict.Provider.Valid() || !validOpaqueID(string(verdict.OwnerAccountID), 64) ||
			len(verdict.DeploymentID) < 1 || len(verdict.DeploymentID) > 128 {
			return false
		}
	} else if verdict.ApplicationID != "" || verdict.Provider != "" || verdict.OwnerAccountID != "" || verdict.DeploymentID != "" {
		return false
	}
	switch verdict.State {
	case StateValid:
		return verdict.FailureCode == ""
	case StateInvalid:
		return verdict.FailureCode == FailureUnauthorized
	case StateInconclusive:
		return bootstrap && (verdict.FailureCode == FailureForbidden || verdict.FailureCode == FailureNotFound || verdict.FailureCode == FailureRateLimited ||
			verdict.FailureCode == FailureNetwork || verdict.FailureCode == FailureUnknown)
	default:
		return false
	}
}

func validOpaqueID(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validCredentialHandle(handle string) bool {
	const prefix = "kcred_"
	if !strings.HasPrefix(handle, prefix) || len(handle) != len(prefix)+26 {
		return false
	}
	for _, character := range handle[len(prefix):] {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func validEnvironment(environment contract.Environment) bool {
	switch environment {
	case contract.EnvironmentDevelopment, contract.EnvironmentStaging, contract.EnvironmentProduction:
		return true
	default:
		return false
	}
}
