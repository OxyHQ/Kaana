package oxyvalidation

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestReporterMintsKaanaServiceTokenAndPostsExactGeneration(t *testing.T) {
	var (
		mu            sync.Mutex
		mintCalls     int
		validationRaw string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/service-token":
			mintCalls++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode mint: %v", err)
			}
			if body["apiKey"] != "oxy_dk_kaana" || body["apiSecret"] != "kaana-service-secret" {
				t.Errorf("mint body = %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"token":"kaana-service-token","expiresIn":3600,"appName":"Kaana"}}`)
		case "/inference/provider-connections/conn_customer_01/validation":
			if r.Header.Get("Authorization") != "Bearer kaana-service-token" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			validationRaw = string(body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"id":"conn_customer_01"}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reporter, err := New(Config{
		BaseURL: server.URL, APIKey: "oxy_dk_kaana", APISecret: "kaana-service-secret", Environment: contract.EnvironmentDevelopment,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verdict := Verdict{
		ConnectionID: "conn_customer_01", CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz",
		CredentialRevision: 7, Environment: contract.EnvironmentDevelopment, State: StateInvalid, FailureCode: FailureUnauthorized,
	}
	reporter.Submit(verdict)
	reporter.Submit(verdict)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reporter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	got := validationRaw
	mu.Unlock()
	if mintCalls != 1 {
		t.Fatalf("mint calls = %d", mintCalls)
	}
	if got != `{"credentialHandle":"kcred_abcdefghijklmnopqrstuvwxyz","credentialRevision":7,"state":"invalid","failureCode":"unauthorized"}` {
		t.Fatalf("validation body = %s", got)
	}
	if strings.Contains(got, "ownerAccountId") || strings.Contains(got, "environment") {
		t.Fatalf("validation body exceeded Oxy's exact selector: %s", got)
	}
}

func TestReporterRefreshesOnceAfterUnauthorizedValidation(t *testing.T) {
	var mu sync.Mutex
	mints, validations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/auth/service-token":
			mints++
			_, _ = io.WriteString(w, `{"data":{"token":"token-`+string(rune('0'+mints))+`","expiresIn":3600}}`)
		case "/inference/provider-connections/conn_retry/validation":
			validations++
			if validations == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reporter, err := New(Config{BaseURL: server.URL, APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(Verdict{ConnectionID: "conn_retry", CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 1, Environment: contract.EnvironmentDevelopment, State: StateValid})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reporter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if mints != 2 || validations != 2 {
		t.Fatalf("mints/validations = %d/%d", mints, validations)
	}
}

func TestReporterPostsFullBootstrapBindingAndDoesNotDedupeANewOperation(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/service-token":
			_, _ = io.WriteString(w, `{"data":{"token":"token","expiresIn":3600}}`)
		case "/inference/provider-connections/conn_exact/validation-bootstrap/outcome":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(body))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reporter, err := New(Config{
		BaseURL: server.URL, APIKey: "key", APISecret: "secret",
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := Verdict{
		OperationID: "op_before_topup", ApplicationID: "app_exact", Provider: "stub",
		OwnerAccountID: "acc_exact", ConnectionID: "conn_exact",
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 7,
		Environment: contract.EnvironmentDevelopment, DeploymentID: "dep_exact",
		State: StateInconclusive, FailureCode: FailureForbidden,
	}
	reporter.Submit(base)
	reporter.Submit(base)
	afterTopup := base
	afterTopup.OperationID = "op_after_topup"
	afterTopup.State = StateValid
	afterTopup.FailureCode = ""
	reporter.Submit(afterTopup)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reporter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("callback bodies = %#v", bodies)
	}
	var first, second contract.KaanaCredentialValidationOutcome
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("second callback: %v", err)
	}
	if first.OperationID != "op_before_topup" || first.ApplicationID != "app_exact" ||
		first.OwnerAccountID != "acc_exact" || first.DeploymentID != "dep_exact" ||
		first.State != "inconclusive" || reportedFailure(first) != "forbidden" {
		t.Fatalf("first exact callback = %+v", first)
	}
	if second.OperationID != "op_after_topup" || second.State != "valid" || reportedFailure(second) != "" {
		t.Fatalf("second exact callback = %+v", second)
	}
}

func reportedFailure(outcome contract.KaanaCredentialValidationOutcome) string {
	if outcome.FailureCode == nil {
		return ""
	}
	return string(*outcome.FailureCode)
}

func TestReporterPinsDeployedSecretsToCanonicalOxyOriginAndRejectsInvalidVerdicts(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://api.oxy.so", APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentProduction}); err == nil {
		t.Fatal("non-loopback HTTP base URL was accepted")
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	if _, err := New(Config{BaseURL: server.URL, APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentProduction}); err == nil {
		t.Fatal("production accepted a loopback destination for its service secret")
	}
	for _, destination := range []string{
		"https://attacker.example", "https://api.oxy.so/redirect", "https://api.oxy.so?next=attacker",
		"https://user@api.oxy.so", " https://api.oxy.so",
	} {
		if _, err := New(Config{BaseURL: destination, APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentProduction}); err == nil {
			t.Errorf("production accepted non-canonical destination %q", destination)
		}
	}
	canonical, err := New(Config{BaseURL: canonicalOxyAPIOrigin, APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentProduction, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("canonical production origin was refused: %v", err)
	}
	closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := canonical.Close(closeContext); err != nil {
		t.Fatalf("closing unused canonical reporter: %v", err)
	}
	reporter, err := New(Config{BaseURL: server.URL, APIKey: "key", APISecret: "secret", Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(Verdict{ConnectionID: "conn", CredentialHandle: "handle", CredentialRevision: 1, Environment: contract.EnvironmentDevelopment, State: StateInvalid})
	reporter.Submit(Verdict{ConnectionID: "conn", CredentialHandle: "handle", CredentialRevision: 1, Environment: contract.EnvironmentStaging, State: StateValid})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reporter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if requests != 0 {
		t.Fatalf("invalid verdict produced %d HTTP requests", requests)
	}
}

func TestDeliveredVerdictDedupeCacheIsBounded(t *testing.T) {
	reporter := &Reporter{delivered: make(map[verdictSelector]verdictState)}
	state := verdictState{state: StateValid}
	for index := int64(1); index <= maxDeliveredSelectors+1; index++ {
		reporter.rememberDelivered(verdictSelector{
			connectionID: "conn", handle: "kcred_abcdefghijklmnopqrstuvwxyz",
			revision: index, environment: contract.EnvironmentProduction,
		}, state)
	}
	if len(reporter.delivered) != maxDeliveredSelectors {
		t.Fatalf("delivered selector cache contains %d entries, expected exact bound %d", len(reporter.delivered), maxDeliveredSelectors)
	}
	latest := verdictSelector{
		connectionID: "conn", handle: "kcred_abcdefghijklmnopqrstuvwxyz",
		revision: maxDeliveredSelectors + 1, environment: contract.EnvironmentProduction,
	}
	if reporter.delivered[latest] != state {
		t.Fatal("the newly delivered exact generation was evicted instead of remembered")
	}
}
