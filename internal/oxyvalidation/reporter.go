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

// Config provides Kaana's exact Oxy service principal. APISecret is an Oxy
// service credential, not an upstream provider credential.
type Config struct {
	BaseURL     string
	APIKey      string
	APISecret   string
	Environment contract.Environment
	Client      *http.Client
	Logger      *slog.Logger
	QueueSize   int
	Timeout     time.Duration
}

// Reporter queues verdicts off the inference request path and sends them with
// a cached, short-lived Oxy service token.
type Reporter struct {
	baseURL     *url.URL
	apiKey      string
	apiSecret   string
	environment contract.Environment
	client      *http.Client
	logger      *slog.Logger
	timeout     time.Duration

	mu        sync.Mutex
	closed    bool
	queue     chan Verdict
	done      chan struct{}
	pending   map[Verdict]struct{}
	delivered map[verdictSelector]verdictState

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
// credential boundary used by deployed environments.
func New(config Config) (*Reporter, error) {
	if strings.TrimSpace(config.APIKey) == "" || strings.TrimSpace(config.APISecret) == "" {
		return nil, errors.New("oxy validation: Kaana service API key and secret are required")
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
		baseURL: baseURL, apiKey: config.APIKey, apiSecret: config.APISecret, environment: config.Environment,
		client: &copyClient, logger: logger, timeout: timeout,
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

func (r *Reporter) serviceToken(ctx context.Context, force bool) (string, error) {
	if !force && r.token != "" && time.Now().Add(time.Minute).Before(r.tokenExpiresAt) {
		return r.token, nil
	}
	body, err := json.Marshal(struct {
		APIKey    string `json:"apiKey"`
		APISecret string `json:"apiSecret"`
	}{APIKey: r.apiKey, APISecret: r.apiSecret})
	if err != nil {
		return "", err
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint("auth/service-token"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("oxy validation: minting service token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return "", fmt.Errorf("oxy validation: service token endpoint returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Token     string `json:"token"`
			ExpiresIn int    `json:"expiresIn"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&envelope); err != nil || strings.TrimSpace(envelope.Data.Token) == "" || envelope.Data.ExpiresIn <= 0 {
		return "", errors.New("oxy validation: service token response is invalid")
	}
	r.token = envelope.Data.Token
	r.tokenExpiresAt = time.Now().Add(time.Duration(envelope.Data.ExpiresIn) * time.Second)
	return r.token, nil
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
