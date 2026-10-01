package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestScopedDescriptorNegotiationStrictAndEchoed(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	response := h.postDeploymentQuery(t, []byte(`{"deploymentId":"dep_stub","scopedExecutionContractVersion":"3.6.0"}`), true)
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"scopedExecutionContractVersion":"3.6.0"`) {
		t.Fatal("extension not echoed", string(raw))
	}
	legacy := h.postDeploymentQuery(t, []byte(`{}`), true)
	defer func() { _ = legacy.Body.Close() }()
	raw, _ = io.ReadAll(legacy.Body)
	if strings.Contains(string(raw), "scopedExecution") {
		t.Fatal("legacy response changed")
	}
}
func TestScopedCatalogueRequiresSignedBodyNegotiation(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	for _, tc := range []struct {
		body   string
		sign   bool
		status int
	}{{`{"scopedExecutionContractVersion":"3.6.0"}`, true, 200}, {`{}`, true, 400}, {`{"scopedExecutionContractVersion":"3.6.0"}`, false, 401}} {
		req, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/models/query", bytes.NewBufferString(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if tc.sign {
			h.sign(req, []byte(tc.body))
		}
		response, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("status%d wanted%d: %s", response.StatusCode, tc.status, raw)
		}
		if tc.status == 200 {
			var parsed struct {
				ScopedExecutionContractVersion string `json:"scopedExecutionContractVersion"`
			}
			if json.Unmarshal(raw, &parsed) != nil || parsed.ScopedExecutionContractVersion != "3.6.0" {
				t.Fatal("missing catalogue echo")
			}
		}
	}
}

func TestPrivatePublicationIsExcludedFromEveryLegacyProjection(t *testing.T) {
	scope := map[string]any{"permitId": "private-permit", "idempotencyKey": "idem-fixture", "fixtureSha256": strings.Repeat("a", 64), "expiresAt": "2099-01-01T00:00:00Z", "principal": map[string]any{"accountId": "account", "applicationId": "app", "credentialId": "credential", "environment": "production"}, "policy": map[string]any{"routingPolicyId": "policy", "policyVersion": 1}, "deploymentId": "dep_private", "provider": "stub", "keyId": "exact-key", "modelReference": "stub/private@2026-10-02", "upstreamModelId": "private-upstream", "priceVersionId": "oxy-price", "providerRateCardVersionId": "provider-card", "providerSourceVersion": "source", "maxCostUsd": "0.01"}
	h := newHarnessWithDeployments(t, &stubAdapter{}, []map[string]any{{"deploymentId": "dep_private", "provider": "stub", "modelReference": "stub/private@2026-10-02", "upstreamModelId": "private-upstream", "current": false, "scopedExecution": scope, "observed": map[string]any{"listPrice": map[string]any{"currency": "USD", "input": "0.042", "output": "0"}}}})
	for _, tc := range []struct {
		method, path, body string
		private            bool
	}{{"GET", "/internal/v1/models", "", false}, {"GET", "/internal/v1/health", "", false}, {"POST", "/internal/v1/deployments/query", `{}`, false}, {"POST", "/internal/v1/models/query", `{"scopedExecutionContractVersion":"3.6.0"}`, true}, {"POST", "/internal/v1/deployments/query", `{"deploymentId":"dep_private","scopedExecutionContractVersion":"3.6.0"}`, true}} {
		req, err := http.NewRequest(tc.method, h.server.URL+tc.path, bytes.NewBufferString(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		h.sign(req, []byte(tc.body))
		response, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s: status%d: %s", tc.path, response.StatusCode, raw)
		}
		private := strings.Contains(string(raw), "dep_private") || strings.Contains(string(raw), "private-permit")
		if private != tc.private {
			t.Fatalf("projection scope mismatch %s: %s", tc.path, raw)
		}
		if tc.private && !strings.Contains(string(raw), `"scopedExecutionContractVersion":"3.6.0"`) {
			t.Fatal("private body without explicit echo")
		}
	}
	response := h.postDeploymentQuery(t, []byte(`{"deploymentId":"dep_private"}`), true)
	_ = response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("legacy exact lookup exposed private deployment")
	}
}

func TestScopedCatalogueDifferentRevisionsFailClosed(t *testing.T) {
	scope := map[string]any{"permitId": "private-permit", "idempotencyKey": "idem-fixture", "fixtureSha256": strings.Repeat("a", 64), "expiresAt": "2099-01-01T00:00:00Z", "principal": map[string]any{"accountId": "account", "applicationId": "app", "credentialId": "credential", "environment": "production"}, "policy": map[string]any{"routingPolicyId": "policy", "policyVersion": 1}, "deploymentId": "dep_private", "provider": "stub", "keyId": "exact-key", "modelReference": "stub/private@2026-10-02", "upstreamModelId": "private-upstream", "priceVersionId": "oxy-price", "providerRateCardVersionId": "provider-card", "providerSourceVersion": "source", "maxCostUsd": "0.01"}
	h := newHarnessWithDeployments(t, &stubAdapter{}, []map[string]any{{"deploymentId": "dep_public", "provider": "stub", "modelReference": "stub/private@2026-10-01", "upstreamModelId": "public-upstream", "current": true}, {"deploymentId": "dep_private", "provider": "stub", "modelReference": "stub/private@2026-10-02", "upstreamModelId": "private-upstream", "current": false, "scopedExecution": scope}})
	body := []byte(`{"scopedExecutionContractVersion":"3.6.0"}`)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/models/query", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	h.sign(req, body)
	response, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(raw), "private-permit") {
		t.Fatal("ambiguous full catalogue was projected", string(raw))
	}
}
