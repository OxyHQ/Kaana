package publicapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

// IdentityVerifier establishes live caller identity. It cannot approve spending.
type IdentityVerifier interface {
	Verify(context.Context, contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError)
}

// OxyIdentityVerifier uses the identity signature domain and identity-only path.
type OxyIdentityVerifier struct{ transport *OxyAuthorizer }

type OxyIdentityConfig struct {
	Oxy     OxyControl
	Keys    map[string]ed25519.PublicKey
	Client  *http.Client
	Timeout time.Duration
	Logger  *slog.Logger
	Now     func() time.Time
}

// NewOxyIdentityVerifier cannot be configured to accept economic signatures.
func NewOxyIdentityVerifier(config OxyIdentityConfig) (*OxyIdentityVerifier, error) {
	verifier, err := edgeauth.NewIdentityVerifier(config.Keys, time.Minute)
	if err != nil {
		return nil, err
	}
	transport, err := NewOxyAuthorizer(OxyAuthorizerConfig{Oxy: config.Oxy, Grants: verifier, Client: config.Client, Timeout: config.Timeout, Logger: config.Logger, Now: config.Now})
	if err != nil {
		return nil, err
	}
	transport.url = config.Oxy.ControlURL(contract.OxyKaanaIdentitiesEndpoint)
	return &OxyIdentityVerifier{transport: transport}, nil
}

func (v *OxyIdentityVerifier) Verify(ctx context.Context, request contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError) {
	unavailable := func() (*contract.IdentityAssertion, *contract.PublicError) {
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return unavailable()
	}
	body, headers, failure := v.transport.post(ctx, request.RequestID, request)
	if failure != nil {
		return nil, failure
	}
	if err := v.transport.grants.Verify(headers, body); err != nil {
		return unavailable()
	}
	assertion, err := contract.DecodeIdentityAssertion(body)
	if err != nil {
		return unavailable()
	}
	if assertion.RequestID != request.RequestID || assertion.RequestSHA256 != bodyDigest(raw) || assertion.Audience != request.Audience || !assertion.Current(v.transport.now()) {
		return unavailable()
	}
	if (assertion.UserID == nil) != (request.OnBehalfOfUserID == nil) || (assertion.UserID != nil && *assertion.UserID != *request.OnBehalfOfUserID) {
		return unavailable()
	}
	return assertion, nil
}
