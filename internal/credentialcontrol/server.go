// Package credentialcontrol exposes Kaana's signed, mutation-only BYOK
// custody boundary. It runs separately from inference so its task may encrypt
// but can never decrypt a provider credential.
package credentialcontrol

import (
	"bytes"
	"context"
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
	OutcomePath            = "/internal/v1/customer-provider-credentials/outcomes"
	maxMutationBytes int64 = 16 << 10
)

type mutationWriter interface {
	Create(context.Context, string, credentialstore.CustomerCredentialIdentity, []byte, string) (credentialstore.CustomerCredentialOutcome, error)
	Rotate(context.Context, string, credentialstore.CustomerCredentialScope, []byte, string) (credentialstore.CustomerCredentialOutcome, error)
	Revoke(context.Context, string, credentialstore.CustomerCredentialScope, string) (credentialstore.CustomerCredentialOutcome, error)
	Outcome(context.Context, credentialstore.CustomerCredentialOutcomeQuery) (credentialstore.CustomerCredentialOutcome, error)
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
	SchemaVersion int                                     `json:"schemaVersion"`
	Action        contract.KaanaCredentialOperationAction `json:"action"`
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
	if err := json.Unmarshal(body, &header); err != nil || header.SchemaVersion != contract.CredentialControlSchemaVersion {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var outcome credentialstore.CustomerCredentialOutcome
	switch header.Action {
	case contract.KaanaCredentialCreate:
		var request contract.KaanaCredentialCreateMutation
		decodeErr := decodeStrict(body, &request)
		// UnmarshalJSON may already have decoded the secret before a later
		// unknown or trailing field makes the strict decode fail. Register the
		// clear before inspecting that error so malformed signed input gets the
		// same bounded plaintext lifetime as an accepted mutation.
		defer request.SecretBase64.Clear()
		if decodeErr != nil || request.Action != contract.KaanaCredentialCreate || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		outcome, err = s.writer.Create(r.Context(), string(request.OperationID), customerIdentity(request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment), request.SecretBase64, request.OperationActor)
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
	case contract.KaanaCredentialRotate:
		var request contract.KaanaCredentialRotateMutation
		decodeErr := decodeStrict(body, &request)
		defer request.SecretBase64.Clear()
		if decodeErr != nil || request.Action != contract.KaanaCredentialRotate || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		outcome, err = s.writer.Rotate(r.Context(), string(request.OperationID), customerScope(request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment, request.CredentialHandle, request.ExpectedRevision), request.SecretBase64, request.OperationActor)
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
	case contract.KaanaCredentialRevoke:
		var request contract.KaanaCredentialRevokeMutation
		if decodeStrict(body, &request) != nil || request.Action != contract.KaanaCredentialRevoke || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var err error
		outcome, err = s.writer.Revoke(r.Context(), string(request.OperationID), customerScope(request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment, request.CredentialHandle, request.ExpectedRevision), request.OperationActor)
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
	if err := json.Unmarshal(body, &header); err != nil || header.SchemaVersion != contract.CredentialControlSchemaVersion {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var query credentialstore.CustomerCredentialOutcomeQuery
	switch header.Action {
	case contract.KaanaCredentialCreate:
		var request contract.KaanaCredentialCreateOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != contract.KaanaCredentialCreate || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		query = outcomeQuery(request.OperationID, request.Action, request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment, "", 0)
	case contract.KaanaCredentialRotate:
		var request contract.KaanaCredentialRotateOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != contract.KaanaCredentialRotate || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		query = outcomeQuery(request.OperationID, request.Action, request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment, request.CredentialHandle, request.ExpectedRevision)
	case contract.KaanaCredentialRevoke:
		var request contract.KaanaCredentialRevokeOutcomeRequest
		if decodeStrict(body, &request) != nil || request.Action != contract.KaanaCredentialRevoke || request.SchemaVersion != contract.CredentialControlSchemaVersion {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		query = outcomeQuery(request.OperationID, request.Action, request.Provider, request.OwnerAccountID, request.ConnectionID, request.Environment, request.CredentialHandle, request.ExpectedRevision)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome, err := s.writer.Outcome(r.Context(), query)
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

func outcomeQuery(operationID contract.KaanaCredentialOperationID, action contract.KaanaCredentialOperationAction, provider contract.ProviderSlug, ownerAccountID contract.AccountID, connectionID string, environment contract.Environment, credentialHandle contract.KaanaCredentialHandle, expectedRevision int64) credentialstore.CustomerCredentialOutcomeQuery {
	return credentialstore.CustomerCredentialOutcomeQuery{
		OperationID:                string(operationID),
		Action:                     credentialstore.CustomerCredentialAction(action),
		CustomerCredentialIdentity: customerIdentity(provider, ownerAccountID, connectionID, environment),
		CredentialHandle:           string(credentialHandle),
		ExpectedRevision:           expectedRevision,
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

func customerIdentity(provider contract.ProviderSlug, ownerAccountID contract.AccountID, connectionID string, environment contract.Environment) credentialstore.CustomerCredentialIdentity {
	return credentialstore.CustomerCredentialIdentity{
		Provider: provider, OwnerAccountID: string(ownerAccountID),
		ConnectionID: connectionID, Environment: environment,
	}
}

func customerScope(provider contract.ProviderSlug, ownerAccountID contract.AccountID, connectionID string, environment contract.Environment, credentialHandle contract.KaanaCredentialHandle, revision int64) credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: customerIdentity(provider, ownerAccountID, connectionID, environment),
		CredentialHandle:           string(credentialHandle),
		Revision:                   revision,
	}
}

func (s *Server) writeMutationFailure(w http.ResponseWriter, err error) {
	// Never attach the request body, ciphertext, or underlying database/KMS
	// error to the response or structured log.
	s.logger.Error("customer provider credential mutation failed", "errorType", "credential_mutation")
	writeError(w, http.StatusServiceUnavailable, "service_unavailable")
}

func writeOutcome(w http.ResponseWriter, status int, outcome credentialstore.CustomerCredentialOutcome) {
	action := contract.KaanaCredentialOperationAction(outcome.Operation.Action)
	operationID := contract.KaanaCredentialOperationID(outcome.Operation.OperationID)
	switch outcome.Status {
	case credentialstore.CustomerCredentialOutcomeApplied:
		writeJSON(w, status, contract.KaanaCredentialAppliedOutcome{
			SchemaVersion:    contract.CredentialControlSchemaVersion,
			OperationID:      operationID,
			Action:           action,
			Status:           "applied",
			CredentialHandle: contract.KaanaCredentialHandle(outcome.CredentialHandle),
			Revision:         outcome.Revision,
		})
	case credentialstore.CustomerCredentialOutcomeConflict:
		writeJSON(w, status, contract.KaanaCredentialConflictOutcome{
			SchemaVersion: contract.CredentialControlSchemaVersion,
			OperationID:   operationID,
			Action:        action,
			Status:        "conflict",
		})
	default:
		writeError(w, http.StatusServiceUnavailable, "service_unavailable")
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
