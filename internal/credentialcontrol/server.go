// Package credentialcontrol exposes Kaana's signed, mutation-only BYOK
// custody boundary. It runs separately from inference so its task may encrypt
// but can never decrypt a provider credential.
package credentialcontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

const (
	// MutationPath is the one signed action-discriminated surface. Keeping all
	// actions on one path prevents an authenticated body from changing meaning
	// when replayed to a sibling route.
	MutationPath = "/internal/v1/customer-provider-credentials/mutations"
	// OutcomePath is a signed metadata-only reconciliation route. Its body must
	// repeat the exact non-secret operation identity; operation id alone reveals
	// nothing.
	OutcomePath                 = "/internal/v1/customer-provider-credentials/outcomes"
	maxMutationBytes      int64 = 16 << 10
	mutationSchemaVersion       = 1
)

type mutationWriter interface {
	Create(context.Context, string, credentialstore.CustomerCredentialIdentity, []byte, string) (credentialstore.CustomerCredentialOutcome, error)
	Rotate(context.Context, string, credentialstore.CustomerCredentialScope, []byte, string) (credentialstore.CustomerCredentialOutcome, error)
	Revoke(context.Context, string, credentialstore.CustomerCredentialScope, string) (credentialstore.CustomerCredentialOutcome, error)
	Outcome(context.Context, credentialstore.CustomerCredentialOperation) (credentialstore.CustomerCredentialOutcome, error)
}

// Server accepts signed Oxy control-plane mutations and exact outcome queries.
// It has no credential metadata, ciphertext, or plaintext-resolution route.
type Server struct {
	verifier *edgeauth.Verifier
	writer   mutationWriter
	logger   *slog.Logger
}

// New builds the credential-control server from explicit authorities.
func New(verifier *edgeauth.Verifier, writer mutationWriter, logger *slog.Logger) (*Server, error) {
	if verifier == nil {
		return nil, errors.New("credential control: edge signature verifier is required")
	}
	if writer == nil {
		return nil, errors.New("credential control: customer credential writer is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{verifier: verifier, writer: writer, logger: logger}, nil
}

// Handler returns one mutation route, one exact outcome route and an unsigned
// liveness route that exposes no credential state.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+MutationPath, s.handleMutation)
	mux.HandleFunc("POST "+OutcomePath, s.handleOutcome)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

type mutationHeader struct {
	SchemaVersion int    `json:"schemaVersion"`
	Action        string `json:"action"`
}

type mutationIdentity struct {
	OperationID    string                `json:"operationId"`
	Provider       contract.ProviderSlug `json:"provider"`
	OwnerAccountID string                `json:"ownerAccountId"`
	ConnectionID   string                `json:"connectionId"`
	Environment    contract.Environment  `json:"environment"`
	OperationActor string                `json:"operationActor"`
}

type createMutation struct {
	SchemaVersion int    `json:"schemaVersion"`
	Action        string `json:"action"`
	mutationIdentity
	Secret secretValue `json:"secretBase64"`
}

type rotateMutation struct {
	SchemaVersion int    `json:"schemaVersion"`
	Action        string `json:"action"`
	mutationIdentity
	CredentialHandle string      `json:"credentialHandle"`
	ExpectedRevision int64       `json:"expectedRevision"`
	Secret           secretValue `json:"secretBase64"`
}

type revokeMutation struct {
	SchemaVersion int    `json:"schemaVersion"`
	Action        string `json:"action"`
	mutationIdentity
	CredentialHandle string `json:"credentialHandle"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

type mutationResponse struct {
	SchemaVersion    int    `json:"schemaVersion"`
	OperationID      string `json:"operationId"`
	Action           string `json:"action"`
	Status           string `json:"status"`
	CredentialHandle string `json:"credentialHandle,omitempty"`
	Revision         int64  `json:"revision,omitempty"`
}

type errorResponse struct {
	Code string `json:"code"`
}

func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMutationBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer clear(body)
	if err := s.verifier.Verify(r.Header, body); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var header mutationHeader
	if err := json.Unmarshal(body, &header); err != nil || header.SchemaVersion != mutationSchemaVersion {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var outcome credentialstore.CustomerCredentialOutcome
	switch header.Action {
	case "create":
		var request createMutation
		if decodeStrict(body, &request) != nil || request.Action != "create" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		defer clear(request.Secret)
		outcome, err = s.writer.Create(r.Context(), request.OperationID, request.identity(), request.Secret, request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeOutcome(w, http.StatusConflict, outcome)
			return
		}
		if errors.Is(err, credentialstore.ErrCustomerCredentialInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err != nil {
			s.writeMutationFailure(w, err)
			return
		}
		writeOutcome(w, http.StatusCreated, outcome)
	case "rotate":
		var request rotateMutation
		if decodeStrict(body, &request) != nil || request.Action != "rotate" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		defer clear(request.Secret)
		outcome, err = s.writer.Rotate(r.Context(), request.OperationID, request.scope(), request.Secret, request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeOutcome(w, http.StatusConflict, outcome)
			return
		}
		if errors.Is(err, credentialstore.ErrCustomerCredentialInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err != nil {
			s.writeMutationFailure(w, err)
			return
		}
		writeOutcome(w, http.StatusOK, outcome)
	case "revoke":
		var request revokeMutation
		if decodeStrict(body, &request) != nil || request.Action != "revoke" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var err error
		outcome, err = s.writer.Revoke(r.Context(), request.OperationID, request.scope(), request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeOutcome(w, http.StatusConflict, outcome)
			return
		}
		if errors.Is(err, credentialstore.ErrCustomerCredentialInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err != nil {
			s.writeMutationFailure(w, err)
			return
		}
		writeOutcome(w, http.StatusOK, outcome)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
	}
}

type baseOutcomeRequest struct {
	SchemaVersion  int                   `json:"schemaVersion"`
	Action         string                `json:"action"`
	OperationID    string                `json:"operationId"`
	Provider       contract.ProviderSlug `json:"provider"`
	OwnerAccountID string                `json:"ownerAccountId"`
	ConnectionID   string                `json:"connectionId"`
	Environment    contract.Environment  `json:"environment"`
}

type createOutcomeRequest struct {
	baseOutcomeRequest
	SecretSHA256 string `json:"secretSha256"`
}

type rotateOutcomeRequest struct {
	createOutcomeRequest
	CredentialHandle string `json:"credentialHandle"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

type revokeOutcomeRequest struct {
	baseOutcomeRequest
	CredentialHandle string `json:"credentialHandle"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

func (s *Server) handleOutcome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMutationBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer clear(body)
	if err := s.verifier.Verify(r.Header, body); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var header mutationHeader
	if err := json.Unmarshal(body, &header); err != nil || header.SchemaVersion != mutationSchemaVersion {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var operation credentialstore.CustomerCredentialOperation
	switch header.Action {
	case "create":
		var request createOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != "create" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		operation = request.operation()
	case "rotate":
		var request rotateOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != "rotate" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		operation = request.operation()
	case "revoke":
		var request revokeOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != "revoke" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		operation = request.operation()
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome, err := s.writer.Outcome(r.Context(), operation)
	if errors.Is(err, credentialstore.ErrCustomerCredentialOutcomeUnavailable) {
		writeError(w, http.StatusNotFound, "outcome_not_found")
		return
	}
	if errors.Is(err, credentialstore.ErrCustomerCredentialInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err != nil {
		s.writeMutationFailure(w, err)
		return
	}
	writeOutcome(w, http.StatusOK, outcome)
}

func (r createOutcomeRequest) operation() credentialstore.CustomerCredentialOperation {
	operation := r.baseOutcomeRequest.operation()
	operation.SecretSHA256 = r.SecretSHA256
	return operation
}

func (r baseOutcomeRequest) operation() credentialstore.CustomerCredentialOperation {
	return credentialstore.CustomerCredentialOperation{
		OperationID: r.OperationID,
		Action:      credentialstore.CustomerCredentialAction(r.Action),
		CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
			Provider: r.Provider, OwnerAccountID: r.OwnerAccountID,
			ConnectionID: r.ConnectionID, Environment: r.Environment,
		},
	}
}

func (r rotateOutcomeRequest) operation() credentialstore.CustomerCredentialOperation {
	operation := r.createOutcomeRequest.operation()
	operation.CredentialHandle = r.CredentialHandle
	operation.ExpectedRevision = r.ExpectedRevision
	return operation
}

func (r revokeOutcomeRequest) operation() credentialstore.CustomerCredentialOperation {
	operation := r.baseOutcomeRequest.operation()
	operation.CredentialHandle = r.CredentialHandle
	operation.ExpectedRevision = r.ExpectedRevision
	return operation
}

func decodeStrict(body []byte, destination any) error {
	if err := rejectDuplicateTopLevelFields(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("credential control: trailing JSON")
	}
	return nil
}

func rejectDuplicateTopLevelFields(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("credential control: mutation must be a JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("credential control: mutation field name is invalid")
		}
		if _, duplicate := seen[field]; duplicate {
			return errors.New("credential control: mutation contains a duplicate field")
		}
		seen[field] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		clear(value)
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("credential control: mutation object is not closed")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("credential control: mutation has trailing JSON")
	}
	return nil
}

type secretValue []byte

func (s *secretValue) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' || len(data) > 8194 {
		return errors.New("credential control: invalid secret encoding")
	}
	encoded := data[1 : len(data)-1]
	if len(encoded) == 0 || bytes.ContainsAny(encoded, "\\\r\n") {
		return errors.New("credential control: invalid secret encoding")
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	written, err := base64.StdEncoding.Strict().Decode(decoded, encoded)
	if err != nil {
		clear(decoded)
		return errors.New("credential control: invalid secret encoding")
	}
	*s = decoded[:written]
	return nil
}

func (m mutationIdentity) identity() credentialstore.CustomerCredentialIdentity {
	return credentialstore.CustomerCredentialIdentity{
		Provider: m.Provider, OwnerAccountID: m.OwnerAccountID,
		ConnectionID: m.ConnectionID, Environment: m.Environment,
	}
}

func (m rotateMutation) scope() credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: m.identity(),
		CredentialHandle:           m.CredentialHandle,
		Revision:                   m.ExpectedRevision,
	}
}

func (m revokeMutation) scope() credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: m.identity(),
		CredentialHandle:           m.CredentialHandle,
		Revision:                   m.ExpectedRevision,
	}
}

func (s *Server) writeMutationFailure(w http.ResponseWriter, err error) {
	// Never attach the request body, ciphertext, or underlying database/KMS
	// error to the response or structured log.
	s.logger.Error("customer provider credential mutation failed", "errorType", "credential_mutation")
	writeError(w, http.StatusServiceUnavailable, "service_unavailable")
}

func writeOutcome(w http.ResponseWriter, status int, outcome credentialstore.CustomerCredentialOutcome) {
	writeJSON(w, status, mutationResponse{
		SchemaVersion:    mutationSchemaVersion,
		OperationID:      outcome.Operation.OperationID,
		Action:           string(outcome.Operation.Action),
		Status:           string(outcome.Status),
		CredentialHandle: outcome.CredentialHandle,
		Revision:         outcome.Revision,
	})
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
