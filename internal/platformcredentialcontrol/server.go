// Package platformcredentialcontrol exposes the signed, encrypt-only import
// boundary for Kaana-owned provider pool credentials.
package platformcredentialcontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

const (
	MutationPath       = "/internal/v1/platform-provider-credentials/mutations"
	maxBodyBytes int64 = 16 << 10
)

var (
	ErrConflict = credentialstore.ErrPlatformCredentialConflict
	ErrInvalid  = credentialstore.ErrPlatformCredentialInvalid
)

type Mutation = credentialstore.PlatformCredentialMutation

type Receipt = credentialstore.PlatformCredentialReceipt

type Writer interface {
	Import(context.Context, Mutation, []byte) (Receipt, error)
}

type Server struct {
	verifier *edgeauth.Verifier
	writer   Writer
	logger   *slog.Logger
}

func New(verifier *edgeauth.Verifier, writer Writer, logger *slog.Logger) (*Server, error) {
	if verifier == nil {
		return nil, errors.New("platform credential control: verifier is required")
	}
	if writer == nil {
		return nil, errors.New("platform credential control: writer is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{verifier: verifier, writer: writer, logger: logger}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+MutationPath, s.handleMutation)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer clear(body)
	if s.verifier.Verify(r.Header, body) != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var mutation Mutation
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&mutation) != nil || decoder.Decode(&struct{}{}) != io.EOF || mutation.SchemaVersion != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(mutation.SecretBase64)
	mutation.SecretBase64 = ""
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer clear(secret)
	receipt, err := s.writer.Import(r.Context(), mutation, secret)
	if errors.Is(err, ErrConflict) {
		writeReceipt(w, http.StatusConflict, receipt)
		return
	}
	if errors.Is(err, ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err != nil {
		s.logger.Error("platform credential mutation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeReceipt(w, http.StatusCreated, receipt)
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{Code: code})
}

func writeReceipt(w http.ResponseWriter, status int, receipt Receipt) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(receipt)
}
