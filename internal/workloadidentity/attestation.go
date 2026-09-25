// Package workloadidentity mints a short-lived Oxy service token by proving
// what this process IS, rather than by presenting a key pair (Oxy ADR 0026).
//
// # The exchange
//
// Two round trips, and the second one carries the proof:
//
//  1. POST /auth/service-token/workload/challenge returns a single-use nonce
//     Oxy holds for sixty seconds.
//  2. This process signs an STS GetCallerIdentity request with the credentials
//     its ECS task role already has, with that nonce inside the SIGNED headers,
//     and posts the signature — never the request — to
//     POST /auth/service-token/workload.
//
// Oxy replays the signature to STS, believes AWS's answer rather than this
// caller's, reduces the per-task `assumed-role/<role>/<session>` ARN to the
// role, looks the role up in its own binding table, and mints exactly the token
// an api key and secret would have produced. So Kaana authenticates with a
// credential AWS rotates for it, and there is no secret for anybody to create,
// copy, paste into a parameter store or leak.
//
// # What this package deliberately does not do
//
// It never sends the signed request to AWS: a GetCallerIdentity this process
// executed itself would only tell it what it already knows, and the signature
// is the artefact, not the answer. It never logs the AWS credentials, the
// signature or the token. It holds no cache: the one caller with a token to
// cache owns the cache (internal/oxyvalidation), because two answers to "is
// this token still good?" is one too many.
//
// # This is not yet the identity Kaana prefers
//
// A token minted here authenticates and carries the same scopes the key pair's
// does, but Oxy's BYOK validation callback re-reads the caller through a
// resolver that can only find an `application_credentials` row, and an attested
// token's `credentialId` is not one. So `internal/oxyvalidation` still tries the
// pair first and treats this as the fallback; its `mint` holds the measurement,
// and `docs/operating.md` holds what has to change in Oxy first. Nothing here
// depends on that ordering — this package answers "prove it", not "use it".
//
// # Why internal/awssig and not an SDK signer
//
// internal/awssig already implements SigV4 and is pinned to AWS's published
// test vector, so a second signer would be a second reading of a fixed
// algorithm. Every header it signs is replayed to STS by Oxy verbatim, which is
// what makes the signature verify there — including `x-amz-content-sha256`,
// which is the truthful hash of the exact body Oxy sends. The AWS SDK is still
// what resolves the task role's credentials; that part is not an algorithm and
// writing it by hand would mean reimplementing the container credentials
// endpoint, its refresh and its authorization token.
package workloadidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/OxyHQ/Kaana/internal/awssig"
)

const (
	// attestationProvider is Oxy's closed vocabulary for where a workload runs.
	// AWS is the only place Kaana runs; Oxy refuses a provider it has no
	// verifier for rather than trusting one.
	attestationProvider = "aws-iam"

	// nonceHeader must be spelled exactly as Oxy's ATTESTATION_NONCE_HEADER.
	// Oxy refuses an attestation whose SignedHeaders does not name it, because a
	// nonce the signature does not cover can be swapped by whoever captured the
	// attestation — which is the entire replay the challenge exists to stop.
	nonceHeader = "x-oxy-attestation-nonce"

	// The global STS endpoint, the region its signatures are scoped to, and the
	// only body Oxy will replay — byte for byte, or the replay is refused.
	stsHost    = "sts.amazonaws.com"
	stsRegion  = "us-east-1"
	stsService = "sts"
	stsBody    = "Action=GetCallerIdentity&Version=2011-06-15"

	challengePath = "auth/service-token/workload/challenge"
	exchangePath  = "auth/service-token/workload"

	maxResponseBytes = 64 << 10
)

// ErrNoWorkloadIdentity is what Open reports when this process has no identity
// to prove. It is a state, not a failure: a laptop and a CI runner are supposed
// to reach it, and the caller answers it by presenting a key pair instead.
var ErrNoWorkloadIdentity = errors.New("workload identity: this process has no attestable workload identity")

// Attestable reports whether this process has an identity it can prove.
//
// ECS sets one of these two variables on every task and nothing else does, so
// their presence is the honest test for "there is a task role here to attest
// to". It deliberately does NOT ask whether the AWS SDK can find credentials:
// on a developer's machine it can, and they would be a personal IAM user that
// is bound to no Oxy application — an attestation Oxy refuses, presented
// instead of the key pair that would have worked.
func Attestable(lookup func(string) (string, bool)) bool {
	for _, name := range []string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		if value, present := lookup(name); present && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// Config is what the attestation needs that it cannot decide for itself.
type Config struct {
	// Credentials resolves the task role's current credentials. Open fills it
	// from the ambient AWS configuration, which reads the container credentials
	// endpoint ECS exposes and refreshes them on its own schedule; a test passes
	// an aws.CredentialsProviderFunc.
	Credentials aws.CredentialsProvider
	Client      *http.Client
	Logger      *slog.Logger
	// Now is the instant the signature is scoped to. Oxy refuses a signature
	// more than five minutes from its own clock.
	Now func() time.Time
}

// Client performs the attestation exchange. It is safe for concurrent use and
// holds nothing between calls.
type Client struct {
	credentials aws.CredentialsProvider
	client      *http.Client
	logger      *slog.Logger
	now         func() time.Time
}

// Open builds the client from the ambient AWS configuration, or reports
// ErrNoWorkloadIdentity when this process has nothing to attest.
//
// The attestability check is repeated here rather than left to the caller: a
// caller that forgets it would otherwise get a client that fails on its first
// mint, in production, with an error about a credentials endpoint that was never
// going to be there.
func Open(ctx context.Context, config Config) (*Client, error) {
	if !Attestable(os.LookupEnv) {
		return nil, ErrNoWorkloadIdentity
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("workload identity: loading AWS configuration: %w", err)
	}
	config.Credentials = awsConfig.Credentials
	return New(config)
}

// New builds the client from an explicit credential source.
func New(config Config) (*Client, error) {
	if config.Credentials == nil {
		return nil, errors.New("workload identity: an AWS credential source is required")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	// An attestation must not be followed anywhere. A redirect off the origin
	// that issued the nonce would hand the signature to whoever answered.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Client{credentials: config.Credentials, client: &copyClient, logger: logger, now: now}, nil
}

// Mint exchanges this process's identity for an Oxy service token, returning it
// and the lifetime Oxy reported for it.
//
// The origin is the caller's, not this package's: an attestation is only safe
// to hand to the origin that issued the nonce it signs. An attacker origin that
// relayed a genuine Oxy challenge and collected the signature could mint a
// Kaana token with it, so "which origin" is a security decision, and it is made
// in the one place that already owns it (internal/oxyvalidation). The URL is
// copied rather than used, so a caller's value cannot be edited from here.
//
// Failures are returned, never softened into an empty token: a process that
// cannot prove what it is must not go on to make an unauthenticated request and
// learn about it as a 401 somewhere else.
func (c *Client) Mint(ctx context.Context, origin *url.URL) (string, time.Duration, error) {
	if origin == nil {
		return "", 0, errors.New("workload identity: an Oxy API origin is required")
	}
	nonce, err := c.challenge(ctx, origin)
	if err != nil {
		return "", 0, err
	}
	headers, err := c.attest(ctx, nonce)
	if err != nil {
		return "", 0, err
	}
	return c.exchange(ctx, origin, nonce, headers)
}

// challenge asks Oxy for the nonce the attestation will sign.
func (c *Client) challenge(ctx context.Context, origin *url.URL) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(origin, challengePath), nil)
	if err != nil {
		return "", fmt.Errorf("workload identity: building the challenge request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("workload identity: requesting a challenge: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return "", fmt.Errorf("workload identity: Oxy refused to issue a challenge: HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Nonce string `json:"nonce"`
		} `json:"data"`
		Nonce string `json:"nonce"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&envelope); err != nil {
		return "", errors.New("workload identity: Oxy's challenge response is invalid")
	}
	// Oxy wraps successful bodies in `data`; the unwrapped form is accepted for
	// the same reason its own client accepts it — the older route shape answers
	// flat, and a client that only understood one of the two would fail on a
	// difference that is not about identity at all.
	nonce := envelope.Data.Nonce
	if nonce == "" {
		nonce = envelope.Nonce
	}
	if strings.TrimSpace(nonce) == "" {
		return "", errors.New("workload identity: Oxy issued a challenge with no nonce")
	}
	return nonce, nil
}

// attest signs GetCallerIdentity, binding the nonce into the signature, and
// returns the headers that ARE the attestation.
//
// Nothing else travels: the method, the path and the body are Oxy's own, so the
// most a tampered payload could do is fail to verify against a request it did
// not sign. `host` is added by hand because Go keeps it off the header map,
// which is also why awssig has to sign it from the URL.
func (c *Client) attest(ctx context.Context, nonce string) (map[string]string, error) {
	credentials, err := c.credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("workload identity: resolving this task's AWS credentials: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+stsHost+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("workload identity: building the attestation: %w", err)
	}
	request.Header.Set(nonceHeader, nonce)
	if err := awssig.Sign(
		request,
		awssig.HashPayload([]byte(stsBody)),
		awssig.Credentials{
			AccessKeyID:     credentials.AccessKeyID,
			SecretAccessKey: credentials.SecretAccessKey,
			SessionToken:    credentials.SessionToken,
		},
		stsRegion, stsService, c.now(),
	); err != nil {
		return nil, fmt.Errorf("workload identity: signing the attestation: %w", err)
	}
	headers := map[string]string{"host": stsHost}
	for name, values := range request.Header {
		// Joined, not first-wins: a multi-valued header is signed in its joined
		// form, so sending anything else would send something the signature does
		// not cover.
		headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	return headers, nil
}

// exchange presents the attestation and returns the token Oxy mints from it.
func (c *Client) exchange(ctx context.Context, origin *url.URL, nonce string, headers map[string]string) (string, time.Duration, error) {
	body, err := json.Marshal(attestationRequest{
		Provider:    attestationProvider,
		Nonce:       nonce,
		Attestation: signedRequest{Headers: headers},
	})
	if err != nil {
		return "", 0, fmt.Errorf("workload identity: encoding the attestation: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(origin, exchangePath), bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("workload identity: building the attestation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("workload identity: presenting the attestation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The status, and not the body: Oxy's refusal carries a bounded reason
		// that is safe to read but tells an operator nothing a status does not,
		// and a body echoed into a log is how request metadata gets there.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return "", 0, fmt.Errorf("workload identity: Oxy refused the attestation: HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data grantedToken `json:"data"`
		// The unwrapped form, accepted for the same reason the nonce's is.
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
		AppName   string `json:"appName"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&envelope); err != nil {
		return "", 0, errors.New("workload identity: Oxy's attestation response is invalid")
	}
	granted := envelope.Data
	if granted.Token == "" {
		granted = grantedToken{Token: envelope.Token, ExpiresIn: envelope.ExpiresIn, AppName: envelope.AppName}
	}
	if strings.TrimSpace(granted.Token) == "" || granted.ExpiresIn <= 0 {
		return "", 0, errors.New("workload identity: Oxy answered the attestation without a usable token")
	}
	c.logger.Info("oxy service token minted from this task's workload identity",
		"appName", granted.AppName, "expiresInSeconds", granted.ExpiresIn)
	return granted.Token, time.Duration(granted.ExpiresIn) * time.Second, nil
}

// attestationRequest is Oxy's exact request shape for the exchange.
type attestationRequest struct {
	Provider    string        `json:"provider"`
	Nonce       string        `json:"nonce"`
	Attestation signedRequest `json:"attestation"`
}

// signedRequest is the whole attestation: signed headers, and nothing else.
type signedRequest struct {
	Headers map[string]string `json:"headers"`
}

type grantedToken struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expiresIn"`
	AppName   string `json:"appName"`
}

func endpoint(origin *url.URL, relative string) string {
	copyURL := *origin
	copyURL.Path = path.Join(strings.TrimSuffix(copyURL.Path, "/"), relative)
	return copyURL.String()
}
