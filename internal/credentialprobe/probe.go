// Package credentialprobe asks a provider, with one authenticated and
// non-billable GET, whether a secret is still a credential.
//
// It exists for one decision: may a disabled key's ciphertext be destroyed
// (credentialstore.Destroy). It therefore says "rejected" only on the
// provider's own documented invalid-credential error, never on a status
// alone: a throttle, an outage, an IP restriction or a permission-scoped key
// answer 4xx too, and reading any of them as "dead" would destroy the only
// copy of a working key. Everything it cannot classify is "unknown".
//
// A provider without a reviewed probe here is always "unknown", so its keys
// can be destroyed only as a duplicate of an enabled key.
package credentialprobe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

const (
	// Timeout bounds the whole exchange, body included.
	Timeout = 10 * time.Second
	// MaxResponseBytes bounds what is read; the verdict needs the error object.
	MaxResponseBytes = 64 << 10

	// AnthropicEndpoint lists one model: authenticated, account-scoped and
	// free. An invalid key answers 401 {"type":"error","error":{"type":
	// "authentication_error","message":"invalid x-api-key"}} (measured
	// 2026-11-17 with a made-up key).
	AnthropicEndpoint = "https://api.anthropic.com/v1/models?limit=1"
	// ElevenLabsEndpoint is the authenticated, workspace-scoped model list the
	// publisher already reads; an invalid key answers 401 (measured
	// 2026-10-12, docs/elevenlabs.md) with detail code invalid_api_key.
	ElevenLabsEndpoint = "https://api.elevenlabs.io/v1/models"
)

var (
	ErrInvalidCredential = errors.New("credential probe: the secret is not a valid header value")
	ErrRequestFailed     = errors.New("credential probe: the request failed")
	ErrTimeout           = errors.New("credential probe: the request timed out")
	ErrResponseTooLarge  = errors.New("credential probe: the response exceeds 64 KiB")
)

type probeSpec struct {
	endpoint string
	headers  func([]byte) http.Header
	classify func(status int, body []byte) credentialstore.LivenessVerdict
}

// Prober is a credentialstore.LivenessProbe with a fixed set of reviewed
// providers and fixed origins.
type Prober struct {
	client *http.Client
	specs  map[contract.ProviderSlug]probeSpec
}

var _ credentialstore.LivenessProbe = (*Prober)(nil)

// New builds a prober over an explicit transport (tests pass a fake).
func New(transport http.RoundTripper) (*Prober, error) {
	if transport == nil {
		return nil, errors.New("credential probe: transport is required")
	}
	return &Prober{
		client: provider.RefuseRedirects(&http.Client{Transport: transport, Timeout: Timeout}),
		specs: map[contract.ProviderSlug]probeSpec{
			"anthropic":  {endpoint: AnthropicEndpoint, headers: anthropicHeaders, classify: classifyAnthropic},
			"elevenlabs": {endpoint: ElevenLabsEndpoint, headers: elevenLabsHeaders, classify: classifyElevenLabs},
		},
	}, nil
}

// NewProduction refuses proxies, redirects, connection reuse and HTTP/2 (which
// may retry a GET implicitly), so the secret is sent once, to one origin.
func NewProduction() *Prober {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.Protocols.SetHTTP2(false)
	transport.ForceAttemptHTTP2 = false
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	prober, _ := New(transport)
	return prober
}

// Probe makes at most one request. A provider without a reviewed probe
// returns unknown without any request.
func (p *Prober) Probe(ctx context.Context, slug contract.ProviderSlug, secret []byte) (credentialstore.LivenessVerdict, error) {
	spec, ok := p.specs[slug]
	if !ok {
		return credentialstore.LivenessUnknown, nil
	}
	if provider.ValidateCustomerCredential(secret) != nil {
		return credentialstore.LivenessUnknown, ErrInvalidCredential
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.endpoint, nil)
	if err != nil {
		return credentialstore.LivenessUnknown, ErrRequestFailed
	}
	request.Header = spec.headers(secret)
	response, err := p.client.Do(request)
	if err != nil {
		// Never return err: a transport error may quote the request.
		if errors.Is(err, context.DeadlineExceeded) {
			return credentialstore.LivenessUnknown, ErrTimeout
		}
		return credentialstore.LivenessUnknown, ErrRequestFailed
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return credentialstore.LivenessUnknown, ErrRequestFailed
	}
	if len(body) > MaxResponseBytes {
		return credentialstore.LivenessUnknown, ErrResponseTooLarge
	}
	return spec.classify(response.StatusCode, body), nil
}

func anthropicHeaders(secret []byte) http.Header {
	return http.Header{
		"X-Api-Key":         {string(secret)},
		"Anthropic-Version": {providerconfig.AnthropicAPIVersion},
		"Accept":            {"application/json"},
	}
}

func elevenLabsHeaders(secret []byte) http.Header {
	return http.Header{
		"Xi-Api-Key": {string(secret)},
		"Accept":     {"application/json"},
	}
}

// classifyAnthropic: 200 authenticates; 401 with the documented
// authentication_error type is an invalid key. A 403 permission_error, a 402
// billing_error (a funded key would work) or anything else proves nothing.
func classifyAnthropic(status int, body []byte) credentialstore.LivenessVerdict {
	if status == http.StatusOK {
		return credentialstore.LivenessAccepted
	}
	if status != http.StatusUnauthorized {
		return credentialstore.LivenessUnknown
	}
	var parsed struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if !decodeObject(body, &parsed) || parsed.Type != "error" || parsed.Error.Type != "authentication_error" {
		return credentialstore.LivenessUnknown
	}
	return credentialstore.LivenessRejected
}

// classifyElevenLabs: 200 authenticates; 401 whose detail code (or the legacy
// detail status) is invalid_api_key is an invalid key. A 403 (the production
// keys are restricted to Kaana's NAT addresses, so a probe from anywhere else
// is refused while the key still works) and a 401 for any other reason, such
// as a scoped key missing a permission, prove nothing.
func classifyElevenLabs(status int, body []byte) credentialstore.LivenessVerdict {
	if status == http.StatusOK {
		return credentialstore.LivenessAccepted
	}
	if status != http.StatusUnauthorized {
		return credentialstore.LivenessUnknown
	}
	var parsed struct {
		Detail struct {
			Type   string `json:"type"`
			Code   string `json:"code"`
			Status string `json:"status"`
		} `json:"detail"`
	}
	if !decodeObject(body, &parsed) {
		return credentialstore.LivenessUnknown
	}
	// Measured 2026-11-17 with a made-up key: 401
	// {"detail":{"type":"authentication_error","code":"unauthorized",
	// "status":"invalid_api_key",...}}. The generic code says only
	// "unauthorized"; the invalid-key statement is the status (or a code that
	// says it directly).
	detail := parsed.Detail
	if detail.Type != "" && detail.Type != "authentication_error" {
		return credentialstore.LivenessUnknown
	}
	if detail.Code == "invalid_api_key" || detail.Status == "invalid_api_key" {
		return credentialstore.LivenessRejected
	}
	return credentialstore.LivenessUnknown
}

func decodeObject(body []byte, target any) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Unmarshal(trimmed, target) == nil
}
