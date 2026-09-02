// Package httpapi is Kaana's Oxy-facing surface.
//
// It has exactly five routes and no customer-facing one. Kaana is not on the
// public internet's path to a customer: the Oxy edge authenticates, attributes,
// authorizes and reserves, then forwards a signed envelope here.
//
//	POST /internal/v1/inference     signed envelope in, normalized event stream out
//	GET  /internal/v1/health        signed, the customer-safe provider projection
//	GET  /internal/v1/models        signed, the model catalogue Oxy may publish
//	POST /internal/v1/deployments/query signed, exact operator-safe route identities
//	GET  /livez                     unsigned liveness, carrying no provider detail
//
// # Where inference HTTP status codes stop
//
// On POST /internal/v1/inference a status code answers exactly one question: was
// this a well-formed envelope from the Oxy edge? Once the answer is yes, the
// response is 200 and an event stream, and every outcome after that — including
// a refusal — arrives as the stream's terminal error event. The signed operator
// surfaces use ordinary statuses for malformed or absent lookups because no
// inference stream has begun.
//
// The alternative, choosing a status from the failure, cannot be made to work:
// the decision would have to be taken before the first byte of a stream that
// has not been produced yet, and a request that fails after two hundred tokens
// has already sent 200. One rule that always holds beats one that holds until
// the interesting case.
package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/rotation"
	"github.com/OxyHQ/Kaana/internal/sse"
)

// SSE frame names. Kaana's own transport framing; see internal/sse.
const (
	FrameStreamEvent = "stream_event"
	FrameUsageReport = "usage_report"
)

// HeaderRequestID echoes the envelope's request id so a failure is correlatable
// even when the body could not be read.
const HeaderRequestID = "X-Oxy-Request-Id"

// DefaultMaxEnvelopeBytes bounds an inbound envelope. A prompt with inline
// images is legitimately large; a body without a limit is a memory exhaustion
// primitive.
const DefaultMaxEnvelopeBytes int64 = 16 << 20

// MaxDeploymentDescriptorQueryIDs bounds one signed identity attestation. The
// edge sends every route it may authorize in one request, but an authenticated
// caller must not be able to turn that operator surface into an unbounded
// allocation or full-catalogue substitute.
const MaxDeploymentDescriptorQueryIDs = 64

// Server serves the Oxy-facing surface.
type Server struct {
	executor         *kaana.Executor
	verifier         *edgeauth.Verifier
	registry         *provider.Registry
	inventory        *inventory.Store
	rotation         *rotation.Registry
	logger           *slog.Logger
	maxEnvelopeBytes int64
}

// Config wires a Server. Every field is required; there is no unauthenticated
// mode, not even for local development, because a bypass that exists is a
// bypass that ships.
type Config struct {
	Executor  *kaana.Executor
	Verifier  *edgeauth.Verifier
	Registry  *provider.Registry
	Inventory *inventory.Store
	Rotation  *rotation.Registry
	// Logger is optional; nil uses the default logger.
	Logger           *slog.Logger
	MaxEnvelopeBytes int64
}

// New builds the server.
func New(config Config) (*Server, error) {
	switch {
	case config.Executor == nil:
		return nil, fmt.Errorf("httpapi: no executor")
	case config.Verifier == nil:
		return nil, fmt.Errorf("httpapi: no edge signature verifier")
	case config.Registry == nil:
		return nil, fmt.Errorf("httpapi: no adapter registry")
	case config.Inventory == nil:
		return nil, fmt.Errorf("httpapi: no inventory store")
	case config.Rotation == nil:
		return nil, fmt.Errorf("httpapi: no rotation registry")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	limit := config.MaxEnvelopeBytes
	if limit <= 0 {
		limit = DefaultMaxEnvelopeBytes
	}
	return &Server{
		executor:         config.Executor,
		verifier:         config.Verifier,
		registry:         config.Registry,
		inventory:        config.Inventory,
		rotation:         config.Rotation,
		logger:           logger,
		maxEnvelopeBytes: limit,
	}, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/inference", s.handleInference)
	mux.HandleFunc("GET /internal/v1/health", s.handleHealth)
	mux.HandleFunc("GET /internal/v1/models", s.handleModels)
	mux.HandleFunc("POST /internal/v1/deployments/query", s.handleDeployments)
	mux.HandleFunc("GET /livez", s.handleLive)
	return mux
}

/* -------------------------------------------------------------------------- */
/*  Inference                                                                 */
/* -------------------------------------------------------------------------- */

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	body, failure := s.readSignedBody(w, r)
	if failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}

	if version, err := envelopeVersion(body); err != nil {
		s.writeRejection(w, http.StatusBadRequest,
			contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest, err.Error()))
		return
	} else if version != contract.RequestEnvelopeVersion {
		// Refused whole, before any field of it is read. A partially understood
		// envelope is how a routing or spend constraint gets silently dropped,
		// so an unrecognised version is a hard error rather than an optimistic
		// interpretation.
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
			fmt.Sprintf("this build implements envelope schemaVersion %d; the request declares %d", contract.RequestEnvelopeVersion, version)).
			WithParam("schemaVersion"))
		return
	}

	var request contract.Request
	// Unknown fields are deliberately tolerated: the contract states that
	// adding an optional field is additive and does not bump a shape's version,
	// so a strict decoder would turn every additive Oxy change into an outage.
	if err := json.Unmarshal(body, &request); err != nil {
		s.writeRejection(w, http.StatusBadRequest,
			contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest, "the envelope is not readable as an inference request"))
		return
	}

	requestID := request.Attribution.RequestID
	if requestID == "" {
		s.writeRejection(w, http.StatusBadRequest,
			contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest, "the envelope carries no attribution.requestId").
				WithParam("attribution.requestId"))
		return
	}
	w.Header().Set(HeaderRequestID, string(requestID))

	writer, err := sse.NewWriter(w)
	if err != nil {
		s.logger.Error("the response writer cannot stream", "requestId", requestID, "error", err)
		return
	}

	startedAt := time.Now()
	sink := func(event contract.StreamEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("httpapi: encoding a %s event: %w", event.EventType(), err)
		}
		if err := writer.WriteEvent(FrameStreamEvent, payload); err != nil {
			if errors.Is(err, sse.ErrClientGone) {
				return fmt.Errorf("%w: %w", kaana.ErrClientGone, err)
			}
			return err
		}
		return nil
	}

	// r.Context() is cancelled by net/http when the client disconnects, and it
	// is the same context the adapter hands to its upstream request. That is
	// the entire cancellation path: no signalling of our own, and nothing an
	// adapter can decline to honour.
	result := s.executor.Execute(r.Context(), &request, sink)

	if result.Report != nil {
		payload, err := json.Marshal(result.Report)
		if err != nil {
			s.logger.Error("the usage report does not encode", "requestId", requestID, "error", err)
		} else if err := writer.WriteEvent(FrameUsageReport, payload); err != nil {
			// The work happened and is owed. The edge recovers the record by
			// requestId; losing the frame is not losing the usage.
			s.logger.Warn("the usage report could not be delivered", "requestId", requestID, "error", err)
		}
	}

	s.logResult(requestID, result, time.Since(startedAt))
}

// logResult records what happened. It names ids, a route and an outcome, and
// never a prompt, a completion, a header or a body: request content does not
// enter ordinary logs.
func (s *Server) logResult(requestID contract.RequestID, result kaana.Result, elapsed time.Duration) {
	attributes := []any{"requestId", requestID, "durationMs", elapsed.Milliseconds()}
	if result.Report != nil {
		attributes = append(attributes,
			"provider", result.Report.ServingProvider,
			"model", result.Report.ResolvedModelReference,
			"outcome", result.Report.Outcome,
			"usageSource", result.Report.UsageSource,
			"units", result.Report.Units,
			"routeSwitches", result.Report.RouteSwitches,
		)
		if result.Report.DeploymentID != nil {
			attributes = append(attributes, "deploymentId", *result.Report.DeploymentID)
		}
		if result.Report.TimeToFirstTokenMs != nil {
			attributes = append(attributes, "timeToFirstTokenMs", *result.Report.TimeToFirstTokenMs)
		}
	}
	if len(result.UpstreamCost.Attempts) > 0 {
		// What the providers will invoice for this request, including attempts
		// that failed and produced nothing for the customer. It is here, in an
		// operator log, and in no response body: Kaana measures its own cost
		// and never quotes an amount to anyone.
		attributes = append(attributes, "upstreamCost", result.UpstreamCost)
	}
	if result.Failure != nil {
		attributes = append(attributes, "code", result.Failure.Code, "retryable", result.Failure.Retryable)
		s.logger.Warn("inference request failed", attributes...)
		return
	}
	s.logger.Info("inference request served", attributes...)
}

/* -------------------------------------------------------------------------- */
/*  Health                                                                    */
/* -------------------------------------------------------------------------- */

// deploymentHealth is one route's rotation state, joined to what the inventory
// says it is. It names a deployment id — which Oxy issues and already receives
// on every usage report — a provider slug and a health score, and nothing else:
// no upstream URL, no region-level capacity, no credential state.
type deploymentHealth struct {
	rotation.Health
	Provider contract.ProviderSlug `json:"provider"`
}

type healthResponse struct {
	ContractVersion string             `json:"contractVersion"`
	CheckedAt       contract.Timestamp `json:"checkedAt"`
	Providers       []provider.Health  `json:"providers"`
	// Configuration is what the data plane is serving from. An operator reading
	// a wave of refusals needs to see a snapshot that stopped advancing here,
	// rather than inferring it from the shape of the errors.
	Configuration inventory.SnapshotStatus `json:"configuration"`
	// Deployments is the rotation state of every route in the snapshot. A
	// deployment that is out of rotation is the difference between "the
	// provider is down" and "we stopped asking", and only this surface says
	// which.
	Deployments []deploymentHealth `json:"deployments"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, failure := s.readSignedBody(w, r); failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}

	response := healthResponse{
		ContractVersion: contract.ContractVersion,
		CheckedAt:       contract.NewTimestamp(time.Now()),
		Providers:       make([]provider.Health, 0),
		Configuration:   s.inventory.Status(),
		Deployments:     make([]deploymentHealth, 0),
	}
	for _, adapter := range s.registry.All() {
		response.Providers = append(response.Providers, adapter.Health(r.Context()))
	}

	endpoints := s.inventory.Current().Deployments()
	ids := make([]contract.DeploymentID, 0, len(endpoints))
	for _, endpoint := range endpoints {
		ids = append(ids, endpoint.DeploymentID)
	}
	for index, health := range s.rotation.Project(ids) {
		response.Deployments = append(response.Deployments, deploymentHealth{
			Health:   health,
			Provider: endpoints[index].Provider,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

/* -------------------------------------------------------------------------- */
/*  Exact deployment identities                                                */
/* -------------------------------------------------------------------------- */

type deploymentDescriptorsResponse struct {
	SnapshotID  string                           `json:"snapshotId"`
	Deployments []inventory.DeploymentDescriptor `json:"deployments"`
}

var (
	errDeploymentDescriptorNotFound  = errors.New("deployment descriptor not found")
	errDeploymentDescriptorAmbiguous = errors.New("deployment descriptor identity is ambiguous")
)

// handleDeployments exposes only the fields Oxy signs in an authorized route.
// A signed {} body returns the complete snapshot projection. A signed body with
// one deploymentId, or a bounded deploymentIds array, performs exact opaque-id
// lookup. The batch is atomic: one absent id refuses the whole response. It
// never resolves a name, provider or position.
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	// Set before authentication so neither a success nor a typed refusal can be
	// retained after the signature that authorized it has expired.
	w.Header().Set("Cache-Control", "no-store")
	body, failure := s.readSignedBody(w, r)
	if failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}
	if r.URL.RawQuery != "" {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(
			newLocalRequestID(), contract.CodeInvalidRequest,
			"deployment descriptor parameters belong in the signed JSON body",
		))
		return
	}
	query, err := parseDeploymentDescriptorQuery(body)
	if err != nil {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(
			newLocalRequestID(), contract.CodeInvalidRequest,
			"the deployment descriptor query must be {}, contain one exact deploymentId, or contain a bounded deploymentIds array",
		))
		return
	}

	current := s.inventory.Current()
	descriptors := current.DeploymentDescriptors()
	if err := validateUniqueDeploymentDescriptors(descriptors); err != nil {
		// Inventory loading already refuses duplicate ids. Keep the read surface's
		// own gate so a future loader regression cannot publish an ambiguous list.
		s.logger.Error("the serving snapshot contains an ambiguous deployment identity",
			"snapshotId", current.SnapshotID())
		s.writeRejection(w, http.StatusServiceUnavailable, contract.NewError(
			newLocalRequestID(), contract.CodeServiceUnavailable,
			"the deployment identity is ambiguous in the serving snapshot",
		))
		return
	}
	if query.DeploymentIDs != nil {
		selected, err := selectExactDeploymentDescriptors(descriptors, query.DeploymentIDs)
		switch {
		case errors.Is(err, errDeploymentDescriptorNotFound):
			s.writeRejection(w, http.StatusNotFound, contract.NewError(
				newLocalRequestID(),
				contract.CodeNoRouteAvailable,
				"no deployment has that exact deploymentId in the serving snapshot",
			))
			return
		case errors.Is(err, errDeploymentDescriptorAmbiguous):
			// Avoid logging the signed body: an authenticated caller can still put
			// credential-shaped text in it.
			s.logger.Error("the serving snapshot contains an ambiguous deployment identity",
				"snapshotId", current.SnapshotID())
			s.writeRejection(w, http.StatusServiceUnavailable, contract.NewError(
				newLocalRequestID(),
				contract.CodeServiceUnavailable,
				"the deployment identity is ambiguous in the serving snapshot",
			))
			return
		case err != nil:
			s.writeRejection(w, http.StatusInternalServerError, contract.NewError(
				newLocalRequestID(), contract.CodeInternalError, "the deployment lookup failed"))
			return
		default:
			descriptors = selected
		}
	}

	writeJSON(w, http.StatusOK, deploymentDescriptorsResponse{
		SnapshotID:  current.SnapshotID(),
		Deployments: descriptors,
	})
}

type deploymentDescriptorQuery struct {
	// nil means the explicit {} operator list. A non-nil slice is one exact
	// selector or a bounded batch; the parser refuses an empty batch.
	DeploymentIDs []contract.DeploymentID
}

// parseDeploymentDescriptorQuery accepts exactly three wire shapes:
//
//	{}
//	{"deploymentId":"<opaque exact id>"}
//	{"deploymentIds":["<opaque exact id>", "..."]}
//
// A struct plus DisallowUnknownFields would still accept a duplicate JSON key.
// Token-level parsing makes duplicates, null, trailing JSON and unknown fields
// explicit refusals rather than values encoding/json quietly normalizes.
func parseDeploymentDescriptorQuery(body []byte) (deploymentDescriptorQuery, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil {
		return deploymentDescriptorQuery{}, fmt.Errorf("reading object start: %w", err)
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return deploymentDescriptorQuery{}, errors.New("the query is not a JSON object")
	}

	query := deploymentDescriptorQuery{}
	seenDeploymentID := false
	seenDeploymentIDs := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return deploymentDescriptorQuery{}, fmt.Errorf("reading field name: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok || (key != "deploymentId" && key != "deploymentIds") {
			return deploymentDescriptorQuery{}, errors.New("the query contains an unknown field")
		}

		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return deploymentDescriptorQuery{}, fmt.Errorf("reading %s: %w", key, err)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return deploymentDescriptorQuery{}, fmt.Errorf("%s is null", key)
		}

		switch key {
		case "deploymentId":
			if seenDeploymentID {
				return deploymentDescriptorQuery{}, errors.New("deploymentId appears more than once")
			}
			seenDeploymentID = true
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return deploymentDescriptorQuery{}, errors.New("deploymentId is not a string")
			}
			if err := validateDeploymentDescriptorID(value); err != nil {
				return deploymentDescriptorQuery{}, err
			}
			query.DeploymentIDs = []contract.DeploymentID{contract.DeploymentID(value)}
		case "deploymentIds":
			if seenDeploymentIDs {
				return deploymentDescriptorQuery{}, errors.New("deploymentIds appears more than once")
			}
			seenDeploymentIDs = true
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil {
				return deploymentDescriptorQuery{}, errors.New("deploymentIds is not an array of strings")
			}
			if len(values) == 0 || len(values) > MaxDeploymentDescriptorQueryIDs {
				return deploymentDescriptorQuery{}, fmt.Errorf(
					"deploymentIds is outside 1..%d entries", MaxDeploymentDescriptorQueryIDs)
			}
			seen := make(map[contract.DeploymentID]struct{}, len(values))
			query.DeploymentIDs = make([]contract.DeploymentID, 0, len(values))
			for _, value := range values {
				if err := validateDeploymentDescriptorID(value); err != nil {
					return deploymentDescriptorQuery{}, err
				}
				deploymentID := contract.DeploymentID(value)
				if _, duplicate := seen[deploymentID]; duplicate {
					return deploymentDescriptorQuery{}, errors.New("deploymentIds contains a duplicate exact id")
				}
				seen[deploymentID] = struct{}{}
				query.DeploymentIDs = append(query.DeploymentIDs, deploymentID)
			}
		}
	}
	if seenDeploymentID && seenDeploymentIDs {
		return deploymentDescriptorQuery{}, errors.New("deploymentId and deploymentIds cannot be combined")
	}

	closing, err := decoder.Token()
	if err != nil {
		return deploymentDescriptorQuery{}, fmt.Errorf("reading object end: %w", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return deploymentDescriptorQuery{}, errors.New("the query object does not close")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return deploymentDescriptorQuery{}, fmt.Errorf("reading trailing JSON: %w", err)
		}
		return deploymentDescriptorQuery{}, errors.New("the query has trailing JSON")
	}
	return query, nil
}

func validateDeploymentDescriptorID(value string) error {
	if len(value) == 0 || len(value) > 128 {
		return errors.New("deployment id is outside 1..128 characters")
	}
	return nil
}

func validateUniqueDeploymentDescriptors(descriptors []inventory.DeploymentDescriptor) error {
	seen := make(map[contract.DeploymentID]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if _, duplicate := seen[descriptor.DeploymentID]; duplicate {
			return errDeploymentDescriptorAmbiguous
		}
		seen[descriptor.DeploymentID] = struct{}{}
	}
	return nil
}

// selectExactDeploymentDescriptor is a second identity gate behind inventory
// loading. The loader refuses duplicate deployment ids today; this function
// makes the operator lookup remain fail-closed if that invariant ever changes.
func selectExactDeploymentDescriptor(
	descriptors []inventory.DeploymentDescriptor,
	deploymentID contract.DeploymentID,
) ([]inventory.DeploymentDescriptor, error) {
	matches := make([]inventory.DeploymentDescriptor, 0, 1)
	for _, descriptor := range descriptors {
		if descriptor.DeploymentID == deploymentID {
			matches = append(matches, descriptor)
		}
	}
	switch len(matches) {
	case 0:
		return nil, errDeploymentDescriptorNotFound
	case 1:
		return matches, nil
	default:
		return nil, errDeploymentDescriptorAmbiguous
	}
}

// selectExactDeploymentDescriptors returns only the requested exact ids and
// refuses the entire batch if any is absent. Global descriptor uniqueness is
// validated before this function is called; it checks again through the count
// equality so a later caller cannot accidentally accept a partial projection.
func selectExactDeploymentDescriptors(
	descriptors []inventory.DeploymentDescriptor,
	deploymentIDs []contract.DeploymentID,
) ([]inventory.DeploymentDescriptor, error) {
	requested := make(map[contract.DeploymentID]struct{}, len(deploymentIDs))
	for _, deploymentID := range deploymentIDs {
		if _, duplicate := requested[deploymentID]; duplicate {
			return nil, errDeploymentDescriptorAmbiguous
		}
		requested[deploymentID] = struct{}{}
	}
	selected := make([]inventory.DeploymentDescriptor, 0, len(deploymentIDs))
	for _, descriptor := range descriptors {
		if _, wanted := requested[descriptor.DeploymentID]; wanted {
			selected = append(selected, descriptor)
		}
	}
	if len(selected) != len(requested) {
		return nil, errDeploymentDescriptorNotFound
	}
	return selected, nil
}

// handleModels answers what this snapshot serves, under the names a caller can
// hold onto.
//
// It exists because the alternative is every consumer keeping its own list of
// model names, and a hand-maintained copy of someone else's catalogue drifts in
// exactly one direction: it keeps offering what was removed and never offers
// what was added. Measured against one consumer's table on 2026-08-23, half its
// misses were spelling — `xai/` against `x-ai/`, `mistral/` against `mistralai/`
// — on names Kaana was serving the whole time.
//
// Signed like the health surface rather than public: the set of models Oxy has
// contracted for is commercial information, and this route names the providers
// behind each one.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, failure := s.readSignedBody(w, r); failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}

	current := s.inventory.Current()
	now := time.Now()
	writeJSON(w, http.StatusOK, modelsResponse{
		ContractVersion: contract.ContractVersion,
		CheckedAt:       contract.NewTimestamp(now),
		Configuration:   s.inventory.Status(),
		// Whether an unpinned name resolves AT ALL right now. Past the staleness
		// horizon every entry below is refused, so a consumer that read the list
		// without reading this would build a catalogue of names that all fail.
		ServesUnpinned:       current.ServesUnpinned(now),
		Models:               current.Catalogue(),
		PinnedOnlyReferences: current.PinnedOnlyReferences(),
	})
}

type modelsResponse struct {
	ContractVersion string             `json:"contractVersion"`
	CheckedAt       contract.Timestamp `json:"checkedAt"`
	// Configuration is the same snapshot identity the health surface reports, so
	// a catalogue read and a health read can be compared without guessing
	// whether they saw the same file.
	Configuration  inventory.SnapshotStatus   `json:"configuration"`
	ServesUnpinned bool                       `json:"servesUnpinned"`
	Models         []inventory.CatalogueEntry `json:"models"`
	// References no unpinned name resolves to. Usually empty; present so that a
	// catalogue which omits a routable model says so rather than looking
	// complete.
	PinnedOnlyReferences []contract.ModelReference `json:"pinnedOnlyReferences"`
}

// handleLive is the only unauthenticated route. It carries the contract version
// — which is what the startup handshake needs and is not a secret — and nothing
// about providers, routes or configuration.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":          "ok",
		"contractVersion": contract.ContractVersion,
	})
}

/* -------------------------------------------------------------------------- */
/*  Envelope admission                                                        */
/* -------------------------------------------------------------------------- */

// readSignedBody reads and verifies the request body.
//
// The bytes returned are the exact bytes verified. Re-encoding between
// verification and parsing would authenticate something other than what gets
// executed, which is the classic way a signature check becomes decorative.
func (s *Server) readSignedBody(w http.ResponseWriter, r *http.Request) ([]byte, *contract.Error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxEnvelopeBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, contract.NewError(newLocalRequestID(), contract.CodeRequestTooLarge,
				fmt.Sprintf("the envelope exceeds %d bytes", s.maxEnvelopeBytes))
		}
		return nil, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest, "the envelope could not be read")
	}
	if err := s.verifier.Verify(r.Header, body); err != nil {
		// Logged without the headers that failed: an attacker's near-miss
		// signature is not information worth storing, and the headers are
		// attacker-controlled text going into a log line.
		s.logger.Warn("rejected an unsigned or badly signed envelope", "path", r.URL.Path)
		return nil, contract.NewError(newLocalRequestID(), contract.CodeAuthenticationFailed,
			"the request is not a signed Oxy edge envelope")
	}
	return body, nil
}

// envelopeVersion reads only the version, before anything else is interpreted.
func envelopeVersion(body []byte) (int, error) {
	var probe struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, fmt.Errorf("the envelope is not JSON")
	}
	if probe.SchemaVersion == nil {
		// Never inferred from the presence of a field. An envelope that does
		// not state its version is one whose meaning has to be guessed.
		return 0, fmt.Errorf("the envelope declares no schemaVersion")
	}
	return *probe.SchemaVersion, nil
}

func (s *Server) writeRejection(w http.ResponseWriter, status int, failure *contract.Error) {
	w.Header().Set(HeaderRequestID, string(failure.RequestID))
	writeJSON(w, status, failure)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// newLocalRequestID names a request that was rejected before its envelope could
// be trusted.
//
// The contract requires a requestId on every error, including one that never
// reached the data plane. Echoing the id from an unverified body would let an
// unauthenticated caller choose what appears in Kaana's logs, so a rejection
// gets an id Kaana minted.
func newLocalRequestID() contract.RequestID {
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "req_kaana_local"
	}
	return contract.RequestID("req_kaana_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:])))
}
