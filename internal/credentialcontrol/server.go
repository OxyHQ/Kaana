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
	MutationPath                = "/internal/v1/customer-provider-credentials/mutations"
	maxMutationBytes      int64 = 16 << 10
	mutationSchemaVersion       = 1
)

type mutationWriter interface {
	Create(context.Context, credentialstore.CustomerCredentialIdentity, []byte, string) (credentialstore.CustomerCredentialReference, error)
	Rotate(context.Context, credentialstore.CustomerCredentialScope, []byte, string) (credentialstore.CustomerCredentialReference, error)
	Revoke(context.Context, credentialstore.CustomerCredentialScope, string) (credentialstore.CustomerCredentialReference, error)
}

// Server accepts signed Oxy control-plane mutations and returns only Kaana's
// opaque handle and revision. It has no read or plaintext-resolution route.
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

// Handler returns a handler containing exactly one mutation route and an
// unsigned liveness route that exposes no credential state.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+MutationPath, s.handleMutation)
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
	CredentialHandle string `json:"credentialHandle"`
	Revision         int64  `json:"revision"`
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

	var reference credentialstore.CustomerCredentialReference
	switch header.Action {
	case "create":
		var request createMutation
		if decodeStrict(body, &request) != nil || request.Action != "create" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		defer clear(request.Secret)
		reference, err = s.writer.Create(r.Context(), request.identity(), request.Secret, request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialExists) {
			writeReference(w, http.StatusConflict, reference)
			return
		}
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeError(w, http.StatusConflict, "credential_conflict")
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
		writeReference(w, http.StatusCreated, reference)
	case "rotate":
		var request rotateMutation
		if decodeStrict(body, &request) != nil || request.Action != "rotate" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		defer clear(request.Secret)
		reference, err = s.writer.Rotate(r.Context(), request.scope(), request.Secret, request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeError(w, http.StatusConflict, "credential_conflict")
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
		writeReference(w, http.StatusOK, reference)
	case "revoke":
		var request revokeMutation
		if decodeStrict(body, &request) != nil || request.Action != "revoke" || request.SchemaVersion != mutationSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var err error
		reference, err = s.writer.Revoke(r.Context(), request.scope(), request.OperationActor)
		if errors.Is(err, credentialstore.ErrCustomerCredentialConflict) {
			writeError(w, http.StatusConflict, "credential_conflict")
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
		writeReference(w, http.StatusOK, reference)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
	}
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

func (m createMutation) identity() credentialstore.CustomerCredentialIdentity {
	return m.mutationIdentity.identity()
}

func (m rotateMutation) scope() credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: m.mutationIdentity.identity(),
		CredentialHandle:           m.CredentialHandle,
		Revision:                   m.ExpectedRevision,
	}
}

func (m revokeMutation) scope() credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: m.mutationIdentity.identity(),
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

func writeReference(w http.ResponseWriter, status int, reference credentialstore.CustomerCredentialReference) {
	writeJSON(w, status, mutationResponse{
		SchemaVersion:    mutationSchemaVersion,
		CredentialHandle: reference.CredentialHandle,
		Revision:         reference.Revision,
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
