package kaana_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
)

func TestGroqCapacityUsesOnlyTheAuthorizedAlternative(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		backup     bool
		success    bool
	}{
		{"capacity fallback", `{"error":{"type":"tokens","code":"rate_limit_exceeded"}}`, true, true},
		{"capacity without authorization", `{"error":{"type":"tokens","code":"rate_limit_exceeded"}}`, false, false},
		{"real oversized payload", `{"error":{"type":"invalid_request_error","code":"request_too_large"}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(upstream.Close)
			primary, err := openaicompat.New(openaicompat.Config{Provider: "groq", BaseURL: upstream.URL, Declarations: []provider.KeyDeclaration{{KeyID: "groq-test", Secret: "groq-capacity-test-secret"}}})
			if err != nil {
				t.Fatal(err)
			}
			backup := succeedingAdapter("backup", 7)
			req := authorizedRequest()
			req.AuthorizedRoutes[0].Provider = "groq"
			if !tc.backup {
				req.AuthorizedRoutes = req.AuthorizedRoutes[:1]
			}
			deployments := strings.ReplaceAll(twoDeploymentsOfOneRevision, `"provider":"stub"`, `"provider":"groq"`)
			events, result := harness{deployments: deployments, adapters: []provider.Adapter{primary, backup}}.run(t, req)
			if attempts.Load() != 1 {
				t.Fatalf("primary attempts = %d, want exactly one", attempts.Load())
			}
			wantBackup := 0
			if tc.success {
				wantBackup = 1
			}
			if backup.attempts() != wantBackup {
				t.Fatalf("backup attempts = %d, want %d", backup.attempts(), wantBackup)
			}
			if len(eventsOfType(events, contract.EventRouteSwitch)) != wantBackup {
				t.Fatal("route switch did not match the actual authorized attempt")
			}
			if tc.success {
				if result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
					t.Fatalf("fallback failed: %+v", result)
				}
			} else if result.Failure == nil {
				t.Fatal("terminal control succeeded")
			}
		})
	}
}
