package publicapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// NativeStore persists the authority decision before execution and content-free
// completion evidence before acknowledging a terminal response.
type NativeStore interface {
	BindGeneration(context.Context, string, contract.GenerationID) error
	HasNativeGeneration(context.Context, contract.CredentialID, *contract.GenerationID, *contract.IdempotencyKey) (bool, error)
	Admit(context.Context, nativeadmission.AdmissionRequest) (*nativeadmission.Admission, error)
	StageCompletion(context.Context, string, contract.UsageReport, []providercost.Event) error
	Settle(context.Context, string, contract.UsageReport, []providercost.Event) error
	ReplayCompletions(context.Context, int) (int, error)
	Generation(context.Context, contract.IdentityPrincipal, *contract.GenerationID, *contract.IdempotencyKey, *contract.UserID) (json.RawMessage, error)
}

type NativePromotionSource interface {
	Eligibility(context.Context, time.Time) (func(provider.Route) bool, error)
}

// NativeConfig is explicit, disabled by absence, and contains no default product
// enrollment. Matching callers stay under native authority on every error.
type NativeConfig struct {
	Identity   IdentityVerifier
	Store      NativeStore
	Policies   []nativeadmission.Policy
	Promotions NativePromotionSource
}

func (c *NativeConfig) validate() error {
	if c.Identity == nil || c.Store == nil || len(c.Policies) == 0 {
		return errors.New("publicapi: native identity, durable store and explicit policies are required")
	}
	if _, err := nativeadmission.NewPlanner(c.Policies, nil); err != nil {
		return err
	}
	for _, policy := range c.Policies {
		requires := policy.Promotional != nil || policy.Routes.RequirePromotionalCredit
		if policy.ImageInput != nil {
			requires = requires || policy.ImageInput.Routes.RequirePromotionalCredit
		}
		for _, operation := range []*nativeadmission.OperationPolicy{policy.Speech, policy.Transcription, policy.Realtime} {
			if operation != nil {
				requires = requires || operation.Routes.RequirePromotionalCredit
			}
		}
		if requires && c.Promotions == nil {
			return errors.New("publicapi: promotional policy requires local funding evidence")
		}
	}
	return nil
}

func (c *NativeConfig) selects(identity contract.IdentityPrincipal) bool {
	for _, policy := range c.Policies {
		if policy.Selects(identity) {
			return true
		}
	}
	return false
}

func nativeFailure(id contract.RequestID, err error) *contract.PublicError {
	code := contract.PublicErrorCode("service_unavailable")
	switch {
	case errors.Is(err, nativeadmission.ErrCapacity):
		code = "rate_limited"
	case errors.Is(err, nativeadmission.ErrReplay), errors.Is(err, nativeadmission.ErrLegacy):
		return contract.NewPublicError(id, "idempotency_conflict", "this request already belongs to an admission; read its generation record without repeating inference")
	case errors.Is(err, nativeadmission.ErrConflict):
		code = "idempotency_conflict"
	case errors.Is(err, nativeadmission.ErrInvalidRequest):
		code = "invalid_request"
	case errors.Is(err, nativeadmission.ErrNoRoute), errors.Is(err, nativeadmission.ErrUnpriced):
		code = "no_route_available"
	case errors.Is(err, nativeadmission.ErrNotEnrolled), errors.Is(err, nativeadmission.ErrDisabled):
		code = "policy_violation"
	}
	return contract.NewPublicError(id, code, contract.PublicMessage(code))
}

func (s *Server) admitNative(ctx context.Context, r *http.Request, id contract.RequestID, receivedAt time.Time, ask authorization, credential string, user *contract.UserID, key *contract.IdempotencyKey) (*admission, bool, *contract.PublicError) {
	identity, failure := s.native.Identity.Verify(ctx, contract.IdentityRequest{SchemaVersion: 1, Kind: "identity", RequestID: id, Audience: "kaana.ai", Credential: credential, OnBehalfOfUserID: user})
	if failure != nil {
		return nil, true, failure
	}
	if identity == nil {
		return nil, true, nativeFailure(id, errors.New("empty identity assertion"))
	}
	if !s.native.selects(identity.Principal) {
		return nil, false, nil
	}
	if ask.nativeUnsupported {
		return nil, true, contract.NewPublicError(id, "unsupported_modality", contract.PublicMessage("unsupported_modality"))
	}
	current := s.inventory.Current()
	models, err := s.catalogue.nativeModels(current, s.now())
	if err != nil {
		return nil, true, nativeFailure(id, err)
	}
	planner, err := nativeadmission.NewPlanner(s.native.Policies, models)
	if err != nil {
		return nil, true, nativeFailure(id, err)
	}
	requested := nativeadmission.Request{Operation: ask.operation, Target: ask.target, Facts: ask.facts, SessionMaxDuration: ask.sessionMaxDuration, SessionMaxResponses: ask.sessionMaxResponses}
	if ask.nativeFormat != nil {
		requested.StructuredOutput = ask.nativeFormat.Type == contract.ResponseFormatJSONSchema
		requested.JSONMode = ask.nativeFormat.Type == contract.ResponseFormatJSONObject
	}
	if ask.nativeReasoning != nil {
		requested.Reasoning = true
		requested.RequestedEffort = ask.nativeReasoning.Effort
	}
	planned, err := planner.Plan(identity.Principal, requested)
	if err != nil {
		return nil, true, nativeFailure(id, err)
	}
	promotional := func(provider.Route) bool { return false }
	for _, scope := range planned.StepRoutes {
		if scope.RequirePromotionalCredit {
			eligibility, readErr := s.native.Promotions.Eligibility(ctx, s.now())
			if readErr == nil && eligibility != nil {
				promotional = eligibility
			}
			break
		}
	}
	needs := ask.needs
	needs.prices = s.catalogue
	if planned.OutputTokens > 0 {
		needs.maxOutputTokens = &planned.OutputTokens
		needs.inputTokens = planned.InputTokens
	}
	var affinity *[32]byte
	if ask.cacheKey != "" {
		hash := principalAffinityKey(*identity.Principal.OwnerAccountID, identity.Principal.CredentialID, ask.cacheKey)
		affinity = &hash
		if deployment, ok := s.affinity.get(hash, s.now()); ok {
			needs.affine = deployment
		}
	}
	authorized := routeAuthorization{requestID: id, steps: planned.Steps, permits: func(step int, route provider.Route) bool {
		return step < len(planned.StepRoutes) && planned.StepRoutes[step].Allows(route.DeploymentID, route.ModelReference, route.Provider, promotional(route)) && planned.AllowsParameters(route.ModelReference.ModelID(), route.AcceptedParameters)
	}}
	resolved, failure := resolveAuthorizedFor(authorized, current, s.funding, s.latency, s.servesOperation(ask.operation, ask.facts.RealtimeSessionKind), needs, s.now())
	if failure != nil {
		return nil, true, failure
	}
	accepted, err := s.native.Store.Admit(ctx, nativeadmission.AdmissionRequest{RequestID: id, Principal: identity.Principal, UserID: identity.UserID, Operation: ask.operation, BodyDigest: bodyDigest(ask.body), IdempotencyKey: key, Plan: planned})
	if err != nil {
		return nil, true, nativeFailure(id, err)
	}
	return &admission{requestID: id, receivedAt: receivedAt, native: accepted, plan: resolved, labels: ask.labels, clientID: ask.clientRequestID, endpoint: ask.endpoint, affinity: affinity, idempotencyKey: key}, true, nil
}

func (a *admission) attribution() contract.Attribution {
	if a.native != nil {
		return a.native.Attribution()
	}
	return a.grant.Attribution()
}
func (a *admission) routingPolicy() contract.RoutingPolicyReference {
	if a.native != nil {
		return a.native.Plan.RoutingPolicy
	}
	return a.grant.RoutingPolicy
}
func (a *admission) steps() []contract.GrantStep {
	if a.native != nil {
		return a.native.Plan.Steps
	}
	return a.grant.Steps
}
func (a *admission) key() *contract.IdempotencyKey {
	if a.native != nil {
		return a.idempotencyKey
	}
	return a.grant.IdempotencyKey
}
func (a *admission) executionID() string {
	if a.native != nil {
		return a.native.ID
	}
	return string(a.grant.GrantID)
}
func (a *admission) customerCredential() *contract.CustomerProviderCredential {
	if a.native != nil {
		return nil
	}
	return a.grant.CustomerProviderCredential
}

func nativeUnsupportedMessages(messages []contract.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Type == contract.ContentPartAudio || part.Type == contract.ContentPartFile {
				return true
			}
		}
	}
	return false
}
