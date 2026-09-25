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
	Mint(ctx context.Context, origin *url.URL) (token string, expiresIn time.Duration, err error)
}

// Config provides Kaana's exact Oxy service principal. APISecret is an Oxy
// service credential, not an upstream provider credential.
//
// APIKey/APISecret and WorkloadMinter are the two ways to be Kaana, and at least
// one of them is required; see New for what each combination means.
type Config struct {
	BaseURL   string
	APIKey    string
	APISecret string
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
	apiKey      string
	apiSecret   string
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

	tokenMu        sync.Mutex
	token          string
	tokenExpiresAt time.Time
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
//
// # The three identity states, decided here
//
// This is the only place that answers "who is this process to Oxy?", so the
// three answers are exhaustive and visible together:
//
//   - A key pair AND an attestable workload identity. The pair is used and the
//     attestation is the fallback, per mint; mint says why that way round. A
//     service holding both is the safe resting state, and the migration off the
//     pair is then one service at a time rather than a flag day.
//   - One of the two. Whichever it has, with no fallback.
//   - Neither. Refused here, at startup, naming both — because that is the one
//     state where nothing downstream can work and the earliest honest moment to
//     say so. It is also exactly the state a laptop without the pair is in.
//
// Half a key pair is a misconfiguration rather than a state, and is refused
// whether or not this process can attest: a deployment that meant to present a
// pair and typed one half of it should hear about the typo.
func New(config Config) (*Reporter, error) {
	apiKey, apiSecret := strings.TrimSpace(config.APIKey), strings.TrimSpace(config.APISecret)
	switch {
	case (apiKey == "") != (apiSecret == ""):
		return nil, errors.New("oxy validation: a Kaana service key pair needs both halves")
	case apiKey == "" && config.WorkloadMinter == nil:
		return nil, errors.New("oxy validation: this process has no Oxy identity: it can neither attest a workload identity nor present a Kaana service key pair")
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
		baseURL: baseURL, apiKey: apiKey, apiSecret: apiSecret, environment: config.Environment,
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
	token, err := r.serviceToken(ctx, false)
	if err != nil {
		return err
	}
	status, err := r.sendVerdict(ctx, token, verdict)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		token, err = r.serviceToken(ctx, true)
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

// ServiceToken shares the dedicated Kaana principal with operational publishers.
func (r *Reporter) ServiceToken(ctx context.Context) (string, error) {
	return r.serviceToken(ctx, false)
}

// serviceToken returns the cached token, or mints one.
//
// One cache for both identities, and it is here rather than in either minter: a
// token is a token whichever way it was proved, `tokenMu` is what stops two
// verdicts minting two of them at once, and the one-minute margin is for clock
// drift between this process and Oxy — serving a token that expires in the next
// second is the same outage as serving an expired one.
func (r *Reporter) serviceToken(ctx context.Context, force bool) (string, error) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if !force && r.token != "" && time.Now().Add(time.Minute).Before(r.tokenExpiresAt) {
		return r.token, nil
	}
	token, lifetime, err := r.mint(ctx)
	if err != nil {
		return "", err
	}
	r.token = token
	r.tokenExpiresAt = time.Now().Add(lifetime)
	return r.token, nil
}

// mint proves this process's identity: the key pair first, the attested workload
// identity second.
//
// # Why that way round, when the point of ADR 0026 is to stop holding secrets
//
// Because a token is not authority, and this was measured rather than assumed.
// An attested Kaana token carries the same appId, ownerAccountId, environment,
// tier and `inference:byok:validate` scope the pair's token carries, and Oxy's
// `/internal/activity*` routes accept it. But every Oxy route that re-reads the
// principal through `resolveLiveAgencyServicePrincipal` refuses it: that lookup
// finds an `application_credentials` row by the token's `credentialId`, and an
// attested token's is `wl_…`, which is not a row in that table.
// `GET /capabilities/service-identity` answers 200 for the pair and 401
// `service_principal_no_longer_active` for the attestation — and
// `authorizeKaanaValidation`, the gate on the BYOK validation callback this
// reporter exists to make, goes through that same resolver.
//
// So preferring the attestation would mint happily and then lose every verdict
// to a 404. Until Oxy resolves an attested principal there too — the sibling
// resolver `resolveLiveAgencyWorkloadByHandle` exists and has one consumer — the
// pair is the identity that can do the work. `@oxy.so/core` orders it the same
// way, which is the second reason: two clients of one contract should not
// disagree about which identity a service prefers.
//
// # Why the fallback is per mint and not only per startup
//
// Each identity has failure modes the other does not: a pair can be rotated or
// revoked between two verdicts, and an attestation depends on Oxy's challenge
// store, on STS and on a binding an operator can change. A process holding both
// has something better to do about either than stop reporting verdicts. What it
// must not do is fail quietly, so every fallback is logged: a run whose primary
// identity never works and whose fallback always does looks identical to a
// healthy one in every other signal.
func (r *Reporter) mint(ctx context.Context) (string, time.Duration, error) {
	switch {
	case r.apiKey != "" && r.workload != nil:
		token, lifetime, err := r.mintFromKeyPair(ctx)
		if err == nil {
			return token, lifetime, nil
		}
		r.logger.Error("the Kaana service key pair could not mint a token; falling back to this task's workload identity",
			"errorType", "oxy_service_token", "reason", err.Error())
		return r.workload.Mint(ctx, r.baseURL)
	case r.apiKey != "":
		return r.mintFromKeyPair(ctx)
	default:
		return r.workload.Mint(ctx, r.baseURL)
	}
}

func (r *Reporter) mintFromKeyPair(ctx context.Context) (string, time.Duration, error) {
	body, err := json.Marshal(struct {
		APIKey    string `json:"apiKey"`
		APISecret string `json:"apiSecret"`
	}{APIKey: r.apiKey, APISecret: r.apiSecret})
	if err != nil {
		return "", 0, err
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint("auth/service-token"), bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("oxy validation: minting service token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return "", 0, fmt.Errorf("oxy validation: service token endpoint returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Token     string `json:"token"`
			ExpiresIn int    `json:"expiresIn"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&envelope); err != nil || strings.TrimSpace(envelope.Data.Token) == "" || envelope.Data.ExpiresIn <= 0 {
		return "", 0, errors.New("oxy validation: service token response is invalid")
	}
	return envelope.Data.Token, time.Duration(envelope.Data.ExpiresIn) * time.Second, nil
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
