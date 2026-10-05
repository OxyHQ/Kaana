package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestDecisionDiagnosticLogsOnlyFiniteReason(t *testing.T) {
	for _, tt := range []struct{ name, detail, want string }{
		{"positive", "invalid systemone response: score_distribution", "score_distribution"},
		{"legacy", "invalid systemone response", ""},
		{"secret", "PRIVATE_PROVIDER_SECRET_CANARY", ""},
		{"prefixed_secret", "invalid systemone response: PRIVATE_PROVIDER_SECRET_CANARY", ""},
		{"suffix", "invalid systemone response: score_distribution PRIVATE_PROVIDER_SECRET_CANARY", ""},
		{"newline", "invalid systemone response: score_distribution\nPRIVATE_PROVIDER_SECRET_CANARY", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			failure := provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: tt.detail}
			s.logResult("synthetic-request", kaana.Result{Failure: failure.ContractError("synthetic-request")}, time.Millisecond)
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			got, present := entry["decisionResponseValidation"]
			if tt.want != "" && got != tt.want {
				t.Fatalf("missing finite diagnostic: %v", entry)
			}
			if tt.want == "" && present {
				t.Fatalf("unexpected diagnostic: %v", entry)
			}
			if strings.Contains(logs.String(), "PRIVATE_") || strings.Contains(logs.String(), tt.detail) {
				t.Fatal("raw error detail was logged")
			}
			if entry["code"] != "provider_error" || entry["msg"] != "inference request failed" {
				t.Fatal("failure classification changed")
			}
		})
	}
}
