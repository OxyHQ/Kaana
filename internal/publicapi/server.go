// Package publicapi serves Kaana's public API, https://kaana.ai/v1 (Kaana ADR
// 0002, phase 3).
//
// Kaana is an inference provider: a developer calls it with an Oxy Console key
// (`oxy_sk_…`), an Oxy service token minted for `aud: kaana.ai`, or an Oxy app
// session. Kaana never interprets that credential. Every billable request
// follows one path:
//
//  1. Read and bound the body, parse it against the public contract and check
//     it against what the model publishes, before anything is reserved.
//  2. Ask Oxy (POST api.oxy.so/internal/kaana/v1/authorizations) with the
//     credential verbatim and content-free facts. Oxy resolves who pays,
//     applies the Console policy, rate limits and capacity, reserves, and
//     answers with a grant signed under oxy-kaana-grant:v1.
//  3. Verify the grant (signature, execution window, body hash), resolve its
//     ordered steps into routes, and execute with the existing executor and
//     adapters. Without a valid grant no provider is called.
//  4. Settle exactly once: the usage report goes to the durable outbox in the
//     same transaction as the provider costs, and a worker delivers it to Oxy
//     signed with Kaana's own key (POST …/settlements).
//
// Nothing public names a provider, operator, key or deployment: errors are
// projected (contract.PublicErrorFrom) and events are rebuilt without them.
// Accounts, keys, limits and billing live in Oxy Console; every message about
// them links to https://console.oxy.so.
package publicapi

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// Header names the public API speaks (`@kaana.ai/contracts` 4.1.0).
const (
	HeaderRequestID            = "Kaana-Request-Id"
	HeaderContractVersion      = "Kaana-Contract-Version"
	HeaderModel                = "Kaana-Model"
	HeaderUsage                = "Kaana-Usage"
	HeaderRoutingPolicyID      = "Kaana-Routing-Policy-Id"
	HeaderRoutingPolicyVersion = "Kaana-Routing-Policy-Version"
	HeaderOnBehalfOf           = "X-Oxy-User-Id"
	HeaderIdempotencyKey       = "Idempotency-Key"
)

// Body limits, checked before anything is parsed or reserved.
const (
	// MaxJSONBodyBytes bounds every JSON request but the transcription one. A
	// prompt with inline images is legitimately large; a body without a limit
	// is a memory-exhaustion primitive.
	MaxJSONBodyBytes int64 = 16 << 20
	// MaxTranscriptionMultipartBytes is the 20 MiB file plus room for the
	// text fields and the multipart framing.
	MaxTranscriptionMultipartBytes int64 = 20<<20 + 64<<10
	// MaxTranscriptionJSONBytes is the base64 of 20 MiB plus the fields.
	MaxTranscriptionJSONBytes int64 = (20<<20+2)/3*4 + 64<<10
)

// Executor is the part of kaana.Executor the public server drives.
type Executor interface {
	Execute(ctx context.Context, request *contract.Request, sink kaana.Sink) kaana.Result
}

// Config wires a Server.
type Config struct {
	Executor   Executor
	Authorizer Authorizer
	Settler    *Settler
	Inventory  *inventory.Store
	// Funding ranks a route's cheapest admissible key; nil keeps the grant's
	// order within each step.
	Funding FundingRanker
	// Kinds says which slugs this process serves as session adapters and
	// which as request adapters; nil resolves every route of an admitted line.
	Kinds RouteKinds
	// Audio publishes each speech model's voice limits (ADR 0002, decision 9);
	// nil takes them from Catalogue.
	Audio AudioCatalogue
	// Costs are the rate cards; a card's requestInputTokens bounds the
	// decisions input estimate. Nil declares none.
	Costs *providercost.Cards
	// Catalogue is the declared half of GET /v1/models (identity, licence,
	// data policy, power class, prices); nil lists nothing.
	Catalogue *PublicCatalogue
	// Realtime serves GET /v1/realtime; nil answers it unavailable.
	Realtime http.Handler
	// AllowedOrigins may call /v1 from a browser (CORS): Oxy Console's
	// playground and the registered origins of apps that use sessions.
	AllowedOrigins []string
	// RateLimit bounds requests per credential before Oxy is asked; the zero
	// value takes DefaultRateLimit.
	RateLimit RateLimit
	Logger    *slog.Logger
	Now       func() time.Time
}

// Server serves /v1.
type Server struct {
	executor   Executor
	authorizer Authorizer
	settler    *Settler
	inventory  *inventory.Store
	funding    FundingRanker
	kinds      RouteKinds
	audio      AudioCatalogue
	realtime   http.Handler
	catalogue  *PublicCatalogue
	costs      *providercost.Cards
	origins    map[string]struct{}
	limiter    *limiter
	logger     *slog.Logger
	now        func() time.Time
	settling   sync.WaitGroup
}

// New builds the server, refusing a configuration that could serve unpaid or
// unsettled requests.
func New(config Config) (*Server, error) {
	switch {
	case config.Executor == nil:
		return nil, errors.New("publicapi: no executor")
	case config.Authorizer == nil:
		return nil, errors.New("publicapi: no Oxy authorizer: without a grant nothing may be served")
	case config.Settler == nil:
		return nil, errors.New("publicapi: no settler: a served request must settle")
	case config.Inventory == nil:
		return nil, errors.New("publicapi: no inventory")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, origin := range config.AllowedOrigins {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if origin == "" {
			continue
		}
		if !strings.HasPrefix(origin, "https://") && !isLoopbackOrigin(origin) {
			return nil, fmt.Errorf("publicapi: CORS origin %q is not https", origin)
		}
		origins[origin] = struct{}{}
	}
	catalogue := config.Catalogue
	if catalogue == nil {
		catalogue = &PublicCatalogue{models: make(map[contract.ModelID]declaredModel)}
	}
	audio := config.Audio
	if audio == nil {
		audio = catalogue
	}
	rate := config.RateLimit
	if rate == (RateLimit{}) {
		rate = DefaultRateLimit
	}
	return &Server{
		executor: config.Executor, authorizer: config.Authorizer, settler: config.Settler,
		inventory: config.Inventory, funding: config.Funding, kinds: config.Kinds, audio: audio, realtime: config.Realtime, catalogue: catalogue, costs: config.Costs,
		origins: origins, limiter: newLimiter(rate, now), logger: logger, now: now,
	}, nil
}

// Handler returns the /v1 routes, wrapped in CORS and the response headers
// every public response carries.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/models/{publisher}/{model}", s.handleModel)
	mux.HandleFunc("GET /v1/power-levels", s.handlePowerLevels)
	mux.HandleFunc("POST /v1/responses", s.timed(s.handleResponses))
	mux.HandleFunc("POST /v1/chat/completions", s.timed(s.handleChatCompletions))
	mux.HandleFunc("POST /v1/embeddings", s.timed(s.handleEmbeddings))
	mux.HandleFunc("POST /v1/audio/speech", s.timed(s.handleSpeech))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.timed(s.handleTranscriptions))
	mux.HandleFunc("POST /v1/images/generations", s.handleImages)
	mux.HandleFunc("POST /v1/decisions", s.timed(s.handleDecisions))
	mux.HandleFunc("GET /v1/generations/{id}", s.handleGeneration)
	mux.HandleFunc("GET /v1/generations", s.handleGenerationByKey)
	mux.HandleFunc("GET /v1/realtime", s.handleRealtime)
	mux.HandleFunc("/v1/", s.handleUnknown)
	return s.cors(mux)
}

func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	writeError(w, requestID, contract.NewPublicError(requestID, "invalid_request",
		fmt.Sprintf("%s %s is not an endpoint of https://kaana.ai/v1", r.Method, r.URL.Path)))
}

// UseRealtime installs the handler of GET /v1/realtime: the realtime
// manager's public handler, built from this server's gate.
func (s *Server) UseRealtime(handler http.Handler) { s.realtime = handler }

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	if s.realtime == nil {
		requestID := NewRequestID()
		writeError(w, requestID, contract.NewPublicError(requestID, "service_unavailable", contract.PublicMessage("service_unavailable")))
		return
	}
	s.realtime.ServeHTTP(w, r)
}

/* -------------------------------------------------------------------------- */
/*  CORS                                                                      */
/* -------------------------------------------------------------------------- */

var (
	corsAllowedHeaders = strings.Join([]string{"Authorization", "Content-Type", HeaderContractVersion, HeaderIdempotencyKey, HeaderOnBehalfOf}, ", ")
	corsExposedHeaders = strings.Join([]string{
		HeaderRequestID, HeaderContractVersion, HeaderModel, HeaderUsage,
		HeaderRoutingPolicyID, HeaderRoutingPolicyVersion, "Retry-After",
	}, ", ")
)

// cors answers preflights for the configured origins and stamps the headers a
// browser needs to read a response. An origin not configured gets no CORS
// headers at all, so the browser refuses it; the credential is never a cookie,
// so credentials mode is never allowed.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderContractVersion, contract.ContractVersion)
		origin := r.Header.Get("Origin")
		_, allowed := s.origins[origin]
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", corsExposedHeaders)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AllowsOrigin reports whether a browser origin may open a realtime session.
func (s *Server) AllowsOrigin(origin string) bool {
	_, allowed := s.origins[origin]
	return allowed
}

func isLoopbackOrigin(origin string) bool {
	for _, prefix := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]"} {
		if origin == prefix || strings.HasPrefix(origin, prefix+":") {
			return true
		}
	}
	return false
}

/* -------------------------------------------------------------------------- */
/*  Responses                                                                 */
/* -------------------------------------------------------------------------- */

// NewRequestID mints the request id Kaana answers with. Kaana mints it: the
// grant and every settlement name it, so the caller cannot choose it.
func NewRequestID() contract.RequestID {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Sprintf("publicapi: the system entropy source failed: %v", err))
	}
	return contract.RequestID("req_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:])))
}

func writeJSON(w http.ResponseWriter, requestID contract.RequestID, status int, payload any) {
	w.Header().Set(HeaderRequestID, string(requestID))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError answers a failure decided before the first byte: the code's
// status and the public error as the body.
func writeError(w http.ResponseWriter, requestID contract.RequestID, failure *contract.PublicError) {
	if failure.RetryAfterMs != nil {
		w.Header().Set("Retry-After", strconv.Itoa((*failure.RetryAfterMs+999)/1000))
	}
	writeJSON(w, requestID, failure.Code.HTTPStatus(), failure)
}

// invalid is the refusal of a request field, before anything is reserved.
func invalid(requestID contract.RequestID, param, message string) *contract.PublicError {
	failure := contract.NewPublicError(requestID, "invalid_request", message)
	if param != "" {
		failure = failure.WithParam(param)
	}
	return failure
}

// fundingRankUnknown ranks a route whose pool states no funding: after paid,
// as ADR 0001 orders an unstated key.
const fundingRankUnknown = 3

// RouteKinds says what this process serves a slug with: the session kinds of
// its realtime adapter, or a request adapter. known is false for a slug it
// does not serve at all.
type RouteKinds interface {
	ServingKinds(slug contract.ProviderSlug) (sessions []contract.RealtimeSessionKind, requests bool, known bool)
}

// servesOperation keeps, of an admitted line's routes, the ones whose adapter
// can perform the grant's operation. Oxy admits a LINE and allows the
// providers that serve it, and one line can be served both as requests and as
// a session (x-ai/grok-voice-transcribe-2.0: file transcription under `xai`,
// streaming under `xai-stt`); a session must not be routed to the request
// adapter, which refuses the whole open, nor a request to the session adapter.
// A slug this process does not serve is left to the executor, which skips it
// as before.
func (s *Server) servesOperation(operation contract.GrantOperation, kind *contract.RealtimeSessionKind) func(provider.Route) bool {
	if s.kinds == nil {
		return nil
	}
	return func(route provider.Route) bool {
		sessions, requests, known := s.kinds.ServingKinds(route.Provider)
		if !known {
			return true
		}
		if operation == contract.GrantRealtimeSession {
			return len(sessions) > 0 && (kind == nil || slices.Contains(sessions, *kind))
		}
		return requests
	}
}

// FundingRanker ranks a platform route by the cheapest funding tier of its
// key pool within a ceiling: 0 free, 1 credit, 2 paid, 3 unstated. ok is
// false when the route has no admissible key at all.
type FundingRanker interface {
	CheapestTier(route provider.Route, ceiling provider.FundingTier) (rank int, ok bool)
}

// RegistryFunding ranks routes from the adapter registry's key pools.
type RegistryFunding struct{ Registry *provider.Registry }

// CheapestTier implements FundingRanker.
func (f RegistryFunding) CheapestTier(route provider.Route, ceiling provider.FundingTier) (int, bool) {
	if f.Registry == nil {
		return fundingRankUnknown, true
	}
	pool, err := f.Registry.ResolveCredential(route.DeploymentID, route.Provider)
	if err != nil {
		return 0, false
	}
	if pool == nil {
		// An adapter that holds its own credentials states no funding: it
		// cannot serve under a ceiling, and comes last without one.
		return fundingRankUnknown, ceiling == provider.FundingTierNone
	}
	if ceiling != provider.FundingTierNone {
		if _, err := pool.WithinFunding(ceiling); err != nil {
			return 0, false
		}
	}
	for rank, tier := range []provider.FundingTier{provider.FundingTierFree, provider.FundingTierCredit, provider.FundingTierPaid} {
		if _, err := pool.WithinFunding(tier); err == nil {
			return rank, true
		}
		if tier == ceiling {
			break
		}
	}
	return fundingRankUnknown, true
}
