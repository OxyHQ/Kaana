// Package httpapi is Relay's Oxy-facing surface.
//
// It has exactly three routes and no customer-facing one. Relay is not on the
// public internet's path to a customer: the Oxy edge authenticates, attributes,
// authorizes and reserves, then forwards a signed envelope here.
//
//	POST /internal/v1/inference   signed envelope in, normalized event stream out
//	GET  /internal/v1/health      signed, the customer-safe provider projection
//	GET  /livez                   unsigned liveness, carrying no provider detail
//
// # Where HTTP status codes stop
//
// A status code answers exactly one question: was this a well-formed envelope
// from the Oxy edge? Once the answer is yes, the response is 200 and an event
// stream, and every outcome after that — including a refusal — arrives as the
// stream's terminal error event.
//
// The alternative, choosing a status from the failure, cannot be made to work:
// the decision would have to be taken before the first byte of a stream that
// has not been produced yet, and a request that fails after two hundred tokens
// has already sent 200. One rule that always holds beats one that holds until
// the interesting case.
package httpapi

import (
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

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/edgeauth"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/relay"
	"github.com/OxyHQ/Relay/internal/sse"
)

// SSE frame names. Relay's own transport framing; see internal/sse.
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

// Server serves the Oxy-facing surface.
type Server struct {
	executor         *relay.Executor
	verifier         *edgeauth.Verifier
	registry         *provider.Registry
	logger           *slog.Logger
	maxEnvelopeBytes int64
}

// Config wires a Server. Every field is required; there is no unauthenticated
// mode, not even for local development, because a bypass that exists is a
// bypass that ships.
type Config struct {
	Executor         *relay.Executor
	Verifier         *edgeauth.Verifier
	Registry         *provider.Registry
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
		logger:           logger,
		maxEnvelopeBytes: limit,
	}, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/inference", s.handleInference)
	mux.HandleFunc("GET /internal/v1/health", s.handleHealth)
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
				return fmt.Errorf("%w: %w", relay.ErrClientGone, err)
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
func (s *Server) logResult(requestID contract.RequestID, result relay.Result, elapsed time.Duration) {
	attributes := []any{"requestId", requestID, "durationMs", elapsed.Milliseconds()}
	if result.Report != nil {
		attributes = append(attributes,
			"provider", result.Report.ServingProvider,
			"model", result.Report.ResolvedModelReference,
			"outcome", result.Report.Outcome,
			"usageSource", result.Report.UsageSource,
		)
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

type healthResponse struct {
	ContractVersion string             `json:"contractVersion"`
	CheckedAt       contract.Timestamp `json:"checkedAt"`
	Providers       []provider.Health  `json:"providers"`
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
	}
	for _, adapter := range s.registry.All() {
		response.Providers = append(response.Providers, adapter.Health(r.Context()))
	}
	writeJSON(w, http.StatusOK, response)
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
// unauthenticated caller choose what appears in Relay's logs, so a rejection
// gets an id Relay minted.
func newLocalRequestID() contract.RequestID {
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "req_relay_local"
	}
	return contract.RequestID("req_relay_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:])))
}
