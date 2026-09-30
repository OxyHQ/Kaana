package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

type stubTelemetry struct {
	mu        sync.Mutex
	lastAfter *credentialstore.AttemptFeedCursor
	lastLimit int
}

func (s *stubTelemetry) ReadAttemptFeed(_ context.Context, after *credentialstore.AttemptFeedCursor, limit int) ([]credentialstore.AttemptFeedEvent, *credentialstore.AttemptFeedCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAfter, s.lastLimit = after, limit
	cursor := credentialstore.AttemptFeedCursor{CreatedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), RequestID: "req_feed", AttemptIndex: 1}
	return []credentialstore.AttemptFeedEvent{{
		Position: cursor.Encode(), RequestID: "req_feed", AttemptIndex: 1, Provider: "groq", KeyID: "key-1",
		KeyClass: "free", DeploymentID: "dep_a", ModelReference: "openai/model@2026-09-01",
		Cost: &providercost.OperatorAmount{Currency: "USD", AmountPicos: "125000"}, CostSource: "rate_card",
	}}, &cursor, nil
}

func (s *stubTelemetry) ReadCredentialEconomics(context.Context) ([]credentialstore.CredentialEconomics, error) {
	return []credentialstore.CredentialEconomics{{Provider: "cohere", KeyID: "key-1", KeyClass: "free",
		Description: &credentialstore.CredentialEconomicsDescribed{Revision: 1, CapacityCategory: "trial",
			Environment: "production", CommercialUse: "permitted", FundingAccountID: "kfa_0123456789abcdef0123456789abcdef"},
		CapacityEvidence: json.RawMessage(`[]`)}}, nil
}

func (h *harness) postTelemetry(t *testing.T, path string, body string, sign func(string, int64, []byte) []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	milliseconds := time.Now().UnixMilli()
	signature := ed25519.Sign(h.private, sign(h.keyID, milliseconds, []byte(body)))
	request.Header.Set(edgeauth.HeaderKeyID, h.keyID)
	request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(signature))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}

func TestTheAttemptFeedNeedsItsOwnSignedPurpose(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	for _, path := range []string{"/internal/v1/provider-telemetry/attempts", "/internal/v1/provider-telemetry/credentials"} {
		// An inference signature is a different purpose and reads nothing.
		status, _ := h.postTelemetry(t, path, `{"schemaVersion":1}`, edgeauth.SigningInput)
		if status != http.StatusUnauthorized {
			t.Errorf("%s with an inference signature = %d, want 401", path, status)
		}
		// The positive control: the telemetry purpose is accepted.
		status, _ = h.postTelemetry(t, path, `{"schemaVersion":1}`, edgeauth.ProviderTelemetrySigningInput)
		if status != http.StatusOK {
			t.Errorf("%s with a telemetry signature = %d, want 200", path, status)
		}
	}
}

func TestTheAttemptFeedResumesFromTheCursorItIssued(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	status, raw := h.postTelemetry(t, "/internal/v1/provider-telemetry/attempts", `{"schemaVersion":1,"limit":1}`, edgeauth.ProviderTelemetrySigningInput)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var page struct {
		Attempts []map[string]any `json:"attempts"`
		Next     *string          `json:"next"`
		CaughtUp bool             `json:"caughtUp"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 1 || page.Next == nil || page.CaughtUp {
		t.Fatalf("page = %s", raw)
	}
	if page.Attempts[0]["cost"].(map[string]any)["amountPicos"] != "125000" {
		t.Fatalf("the upstream amount is not an exact integer string: %s", raw)
	}

	status, raw = h.postTelemetry(t, "/internal/v1/provider-telemetry/attempts",
		`{"schemaVersion":1,"after":"`+*page.Next+`"}`, edgeauth.ProviderTelemetrySigningInput)
	if status != http.StatusOK {
		t.Fatalf("resuming = %d: %s", status, raw)
	}
	h.telemetry.mu.Lock()
	after, limit := h.telemetry.lastAfter, h.telemetry.lastLimit
	h.telemetry.mu.Unlock()
	if after == nil || after.RequestID != "req_feed" || after.AttemptIndex != 1 || limit != 200 {
		t.Fatalf("resumed after %+v with limit %d", after, limit)
	}

	for name, body := range map[string]string{
		"forged cursor":   `{"schemaVersion":1,"after":"kaf_bm90LWEtY3Vyc29y"}`,
		"limit too large": `{"schemaVersion":1,"limit":501}`,
		"unknown field":   `{"schemaVersion":1,"since":"yesterday"}`,
		"wrong version":   `{"schemaVersion":2}`,
	} {
		if status, raw := h.postTelemetry(t, "/internal/v1/provider-telemetry/attempts", body, edgeauth.ProviderTelemetrySigningInput); status != http.StatusBadRequest {
			t.Errorf("%s = %d: %s", name, status, raw)
		}
	}
}

func TestCredentialEconomicsCarryNoLabel(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	status, raw := h.postTelemetry(t, "/internal/v1/provider-telemetry/credentials", `{"schemaVersion":1}`, edgeauth.ProviderTelemetrySigningInput)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), `"capacityCategory":"trial"`) || !strings.Contains(string(raw), `"commercialUse":"permitted"`) {
		t.Fatalf("economics lost their facts: %s", raw)
	}
	for _, forbidden := range []string{"label", "Label", "evidence\":\"", "@"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("credential economics contain %q: %s", forbidden, raw)
		}
	}
}
