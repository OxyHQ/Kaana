package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

// ProviderTelemetrySchemaVersion is the version of both operator telemetry
// reads, their signed queries and their responses.
const ProviderTelemetrySchemaVersion = 1

// MaxProviderTelemetryQueryBytes bounds a signed telemetry query. It carries a
// cursor and a limit, nothing else.
const MaxProviderTelemetryQueryBytes int64 = 4 << 10

// DefaultAttemptFeedPage is the page size when a query names none.
const DefaultAttemptFeedPage = 200

type attemptFeedQuery struct {
	SchemaVersion int     `json:"schemaVersion"`
	After         *string `json:"after"`
	Limit         *int    `json:"limit"`
}

type attemptFeedResponse struct {
	SchemaVersion int                                `json:"schemaVersion"`
	Attempts      []credentialstore.AttemptFeedEvent `json:"attempts"`
	// Next is the cursor to send as `after` on the following read. It equals the
	// query's `after` when nothing new has settled, and is null only when the
	// feed has never held an attempt.
	Next *string `json:"next"`
	// CaughtUp says the page was not full: the reader has everything that has
	// settled, and should wait before reading again.
	CaughtUp bool               `json:"caughtUp"`
	ReadAt   contract.Timestamp `json:"readAt"`
}

type credentialEconomicsQuery struct {
	SchemaVersion int `json:"schemaVersion"`
}

type credentialEconomicsResponse struct {
	SchemaVersion int                                   `json:"schemaVersion"`
	Credentials   []credentialstore.CredentialEconomics `json:"credentials"`
	ReadAt        contract.Timestamp                    `json:"readAt"`
}

// handleAttemptFeed serves every recorded upstream attempt, oldest first, to
// the separately signed operator path. It is Oxy's evidence for ordering
// deployments; nothing it returns is customer-facing.
func (s *Server) handleAttemptFeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, failure := s.readSignedBodyWith(w, r, s.telemetryVerifier, MaxProviderTelemetryQueryBytes)
	if failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}
	if r.URL.RawQuery != "" {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
			"telemetry parameters belong in the signed JSON body"))
		return
	}
	var query attemptFeedQuery
	if err := decodeStrict(body, &query); err != nil || query.SchemaVersion != ProviderTelemetrySchemaVersion {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
			"the attempt feed query must be {schemaVersion: 1, after?: cursor, limit?: 1-500}"))
		return
	}
	limit := DefaultAttemptFeedPage
	if query.Limit != nil {
		limit = *query.Limit
	}
	if limit < 1 || limit > credentialstore.MaxAttemptFeedPage {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
			"the attempt feed limit must be between 1 and 500"))
		return
	}
	var after *credentialstore.AttemptFeedCursor
	if query.After != nil {
		cursor, err := credentialstore.ParseAttemptFeedCursor(*query.After)
		if err != nil {
			s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
				"the attempt feed cursor is not one this feed issued"))
			return
		}
		after = &cursor
	}
	attempts, next, err := s.telemetry.ReadAttemptFeed(r.Context(), after, limit)
	if err != nil {
		s.logger.Error("the provider attempt feed could not be read", "errorType", "provider_telemetry")
		s.writeRejection(w, http.StatusServiceUnavailable, contract.NewError(newLocalRequestID(), contract.CodeServiceUnavailable,
			"the provider attempt feed is temporarily unavailable"))
		return
	}
	response := attemptFeedResponse{
		SchemaVersion: ProviderTelemetrySchemaVersion, Attempts: attempts,
		CaughtUp: len(attempts) < limit, ReadAt: contract.NewTimestamp(time.Now()),
	}
	if next != nil {
		encoded := next.Encode()
		response.Next = &encoded
	}
	writeJSON(w, http.StatusOK, response)
}

// handleCredentialEconomics serves the label-free economic facts about every
// enabled platform key.
func (s *Server) handleCredentialEconomics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, failure := s.readSignedBodyWith(w, r, s.telemetryVerifier, MaxProviderTelemetryQueryBytes)
	if failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}
	var query credentialEconomicsQuery
	if r.URL.RawQuery != "" || decodeStrict(body, &query) != nil || query.SchemaVersion != ProviderTelemetrySchemaVersion {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(newLocalRequestID(), contract.CodeInvalidRequest,
			"the credential economics query must be {schemaVersion: 1} in the signed JSON body"))
		return
	}
	credentials, err := s.telemetry.ReadCredentialEconomics(r.Context())
	if err != nil {
		s.logger.Error("provider credential economics could not be read", "errorType", "provider_telemetry")
		s.writeRejection(w, http.StatusServiceUnavailable, contract.NewError(newLocalRequestID(), contract.CodeServiceUnavailable,
			"provider credential economics are temporarily unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, credentialEconomicsResponse{
		SchemaVersion: ProviderTelemetrySchemaVersion, Credentials: credentials,
		ReadAt: contract.NewTimestamp(time.Now()),
	})
}

// decodeStrict decodes exactly one JSON object with no unknown fields.
func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing content")
	}
	return nil
}
