package publicapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

// Session ceilings Kaana signs a public session for when the caller asks for
// less or nothing. Each bounds what the session can cost, which Oxy's hold
// was sized against; a caller may ask for less, never for more.
var defaultRealtimeLimits = contract.RealtimeSessionLimits{
	MaxDurationMs:       30 * 60 * 1000,
	IdleTimeoutMs:       5 * 60 * 1000,
	MaxInputAudioBytes:  contract.MaxRealtimeSessionAudioBytes,
	MaxOutputAudioBytes: contract.MaxRealtimeSessionAudioBytes,
	MaxResponses:        1000,
}

// RealtimeGate admits public realtime sessions for the realtime manager.
type RealtimeGate struct{ server *Server }

// RealtimeGate returns the gate the realtime manager's public handler uses.
func (s *Server) RealtimeGate() *RealtimeGate { return &RealtimeGate{server: s} }

var _ realtime.PublicGate = (*RealtimeGate)(nil)

// Credential implements realtime.PublicGate.
func (g *RealtimeGate) Credential(r *http.Request) ([32]byte, bool) {
	bearer, ok := bearerFrom(r)
	return bearer.digest, ok
}

// OriginPatterns implements realtime.PublicGate: the hosts of the configured
// browser origins.
func (g *RealtimeGate) OriginPatterns() []string {
	patterns := make([]string, 0, len(g.server.origins))
	for origin := range g.server.origins {
		if parsed, err := url.Parse(origin); err == nil && parsed.Host != "" {
			patterns = append(patterns, parsed.Host)
		}
	}
	return patterns
}

// Admit implements realtime.PublicGate: GET /v1/realtime?model=<reference>
// with a session.open first frame. The session is checked against the
// contract, granted by Oxy for the model, resolved into routes, and handed to
// the manager with the settlement it owes.
func (g *RealtimeGate) Admit(ctx context.Context, r *http.Request, frame []byte) (*realtime.PublicAdmission, *contract.PublicError) {
	s := g.server
	requestID, receivedAt := NewRequestID(), s.now()
	query := r.URL.Query()
	for name := range query {
		if name != "model" {
			return nil, invalid(requestID, name, "GET /v1/realtime takes only ?model=")
		}
	}
	model := contract.ModelReference(query.Get("model"))
	if !model.Valid() {
		return nil, invalid(requestID, "model", "?model= names a model reference (publisher/model[@revision])")
	}
	var open contract.RealtimeSessionOpen
	if failure := decodeRequest(frame, requestID, &open); failure != nil {
		return nil, failure
	}
	if open.Type != "session.open" {
		return nil, invalid(requestID, "type", "the first frame is session.open or session.resume")
	}
	limits, failure := realtimeLimits(requestID, open.Limits)
	if failure != nil {
		return nil, failure
	}
	transport := contract.RealtimeSessionTransport("websocket")
	if open.Transport != nil && *open.Transport != transport {
		return nil, invalid(requestID, "transport", "transport is websocket")
	}
	request := &contract.RealtimeSessionRequest{
		SchemaVersion:  contract.RealtimeSchemaVersion,
		ModelReference: model,
		Kind:           open.Kind,
		Transport:      transport,
		Config:         open.Config,
		Limits:         limits,
		Client: contract.RealtimeClientMetadata{
			Endpoint: "/v1/realtime", ClientSessionID: open.ClientSessionID,
			ReceivedAt: contract.NewTimestamp(receivedAt), Labels: open.Labels,
		},
	}
	// Everything the session asks for is checked before Oxy is asked to hold
	// anything for it; the attribution is the grant's and is filled after.
	provisional := *request
	provisional.Attribution = contract.Attribution{RequestID: requestID, Principal: contract.AuthenticatedPrincipal{Billing: contract.BillingPrincipal{AccountID: "pending"}}}
	if err := provisional.ValidateSession(); err != nil {
		return nil, invalid(requestID, "config", strings.TrimPrefix(err.Error(), "contract: "))
	}
	bearer, _ := bearerFrom(r)
	kind := open.Kind
	target := contract.ModelTarget(model)
	tools := len(open.Config.Tools)
	admitted, failure := s.admit(ctx, r, requestID, receivedAt, authorization{
		operation: contract.GrantRealtimeSession, target: &target, endpoint: "/v1/realtime",
		// The grant binds the model and the open frame: the session's
		// whole request.
		body:   append([]byte("GET /v1/realtime?model="+string(model)+"\n"), frame...),
		facts:  contract.GrantRequestFacts{DeclaredTools: tools, Stream: true, RealtimeSessionKind: &kind},
		labels: open.Labels, clientRequestID: open.ClientSessionID,
	})
	if failure != nil {
		return nil, failure
	}
	if admitted.grant.CustomerProviderCredential != nil {
		s.releaseUnserved(ctx, admitted.grant)
		return nil, invalid(requestID, "", "a customer provider credential cannot hold a realtime session")
	}
	routes := make([]contract.AuthorizedRoute, 0, len(admitted.plan.routes))
	for _, route := range admitted.plan.routes {
		// A session serves the one model it named: other lines a later step
		// admits are not session routes.
		if route.ModelReference.ModelID() == model.ModelID() {
			route.Substitution, route.AuthorizedByPolicy = contract.SubstitutionSameModel, nil
			routes = append(routes, route)
		}
	}
	if len(routes) == 0 {
		s.releaseUnserved(ctx, admitted.grant)
		return nil, contract.NewPublicError(requestID, "no_route_available", contract.PublicMessage("no_route_available"))
	}
	request.Attribution = admitted.grant.Attribution()
	request.RoutingPolicy = admitted.grant.RoutingPolicy
	request.AuthorizedRoutes = routes
	return &realtime.PublicAdmission{
		Request:    request,
		Credential: bearer.digest,
		Settle: func(ctx context.Context, settlement kaana.SessionSettlement, opening *kaana.SessionOpening) {
			report := settlement.Report
			if report == nil {
				report = admitted.unservedReport(nil, s.now())
			}
			_ = s.settler.Settle(ctx, admitted.grant, *report, admitted.plan.stepOf(report.DeploymentID), settlement.UpstreamCost)
		},
	}, nil
}

// realtimeLimits signs the session for the defaults, or for less when the
// caller asks for less.
func realtimeLimits(requestID contract.RequestID, asked *contract.RealtimeOpenLimits) (contract.RealtimeSessionLimits, *contract.PublicError) {
	limits := defaultRealtimeLimits
	if asked == nil {
		return limits, nil
	}
	if asked.IdleTimeoutMs != nil && asked.MaxDurationMs != nil && *asked.IdleTimeoutMs > *asked.MaxDurationMs {
		return limits, invalid(requestID, "limits.idleTimeoutMs", "an idle timeout cannot outlast the session")
	}
	if asked.MaxTextItems != nil {
		return limits, invalid(requestID, "limits.maxTextItems", "limits.maxTextItems is not enforced by this server; omit it")
	}
	apply := func(param string, value *int, ceiling *int) *contract.PublicError {
		if value == nil {
			return nil
		}
		if *value < 1 {
			return invalid(requestID, param, param+" is positive")
		}
		*ceiling = min(*ceiling, *value)
		return nil
	}
	for _, field := range []struct {
		param   string
		value   *int
		ceiling *int
	}{
		{"limits.maxDurationMs", asked.MaxDurationMs, &limits.MaxDurationMs},
		{"limits.idleTimeoutMs", asked.IdleTimeoutMs, &limits.IdleTimeoutMs},
		{"limits.maxInputAudioBytes", asked.MaxInputAudioBytes, &limits.MaxInputAudioBytes},
		{"limits.maxOutputAudioBytes", asked.MaxOutputAudioBytes, &limits.MaxOutputAudioBytes},
		{"limits.maxResponses", asked.MaxResponses, &limits.MaxResponses},
	} {
		if failure := apply(field.param, field.value, field.ceiling); failure != nil {
			return limits, failure
		}
	}
	if limits.MaxDurationMs < 1000 || limits.IdleTimeoutMs < 1000 {
		return limits, invalid(requestID, "limits", "a session lasts and idles at least one second")
	}
	if limits.IdleTimeoutMs > limits.MaxDurationMs {
		limits.IdleTimeoutMs = limits.MaxDurationMs
	}
	return limits, nil
}
