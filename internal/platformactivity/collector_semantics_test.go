package platformactivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin the collector to the semantics of Oxy's own
// (packages/telemetry/src/collector.ts and packages/core/src/server/traffic.ts).

func TestPublishBackoffMatchesOxy(t *testing.T) {
	low, high := func() float64 { return 0 }, func() float64 { return 0.999999 }
	for _, tc := range []struct {
		failures   int
		ceiling    time.Duration
		lowWait    time.Duration
		highAtMost time.Duration
	}{
		{1, 2 * time.Second, time.Second, 2 * time.Second},
		{2, 4 * time.Second, 2 * time.Second, 4 * time.Second},
		{3, 8 * time.Second, 4 * time.Second, 8 * time.Second},
		{8, 256 * time.Second, 128 * time.Second, 256 * time.Second},
		{9, 512 * time.Second, 256 * time.Second, 512 * time.Second},
		{10, 10 * time.Minute, 5 * time.Minute, 10 * time.Minute},
		{50, 10 * time.Minute, 5 * time.Minute, 10 * time.Minute},
		{1 << 30, 10 * time.Minute, 5 * time.Minute, 10 * time.Minute},
	} {
		if got := publishBackoff(tc.failures, low); got != tc.lowWait {
			t.Errorf("failures %d, no jitter: got %s want %s", tc.failures, got, tc.lowWait)
		}
		got := publishBackoff(tc.failures, high)
		if got <= tc.lowWait || got > tc.highAtMost {
			t.Errorf("failures %d, full jitter: got %s, want in (%s, %s]", tc.failures, got, tc.lowWait, tc.highAtMost)
		}
	}
}

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

// logged returns the number of log lines at a level in a text-handler buffer.
func logged(buffer *bytes.Buffer, level string) int {
	return strings.Count(buffer.String(), "level="+level)
}

func TestPublishBacksOffAndBoundsLogNoise(t *testing.T) {
	c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	clock := &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	c.now = clock.Now
	c.random = func() float64 { return 0 }
	var buffer bytes.Buffer
	c.config.Logger = slog.New(slog.NewTextHandler(&buffer, nil))

	attempts := 0
	fail := true
	action := func(context.Context) error {
		attempts++
		if fail {
			return errors.New("refused")
		}
		return nil
	}

	c.publish(action, false) // failure 1: waits 1s
	if attempts != 1 {
		t.Fatalf("attempts %d", attempts)
	}
	// The old loop retried every 2 s tick; inside the wait nothing is attempted.
	clock.now = clock.now.Add(900 * time.Millisecond)
	c.publish(action, false)
	if attempts != 1 {
		t.Fatalf("retried inside the backoff: %d attempts", attempts)
	}
	clock.now = clock.now.Add(200 * time.Millisecond)
	c.publish(action, false) // failure 2: waits 2s
	clock.now = clock.now.Add(1500 * time.Millisecond)
	c.publish(action, false)
	if attempts != 2 {
		t.Fatalf("retried inside the second wait: %d attempts", attempts)
	}
	clock.now = clock.now.Add(600 * time.Millisecond)
	c.publish(action, false) // failure 3: waits 4s
	if attempts != 3 {
		t.Fatalf("attempts %d", attempts)
	}
	// Forced attempts (the final flush and the removal) ignore the wait.
	c.publish(action, true)
	if attempts != 4 {
		t.Fatalf("forced attempt skipped: %d", attempts)
	}
	for range 20 {
		clock.now = clock.now.Add(11 * time.Minute)
		c.publish(action, false)
	}
	if warns := logged(&buffer, "WARN"); warns != 1 {
		t.Fatalf("%d warnings for one failure streak:\n%s", warns, buffer.String())
	}
	if strings.Contains(buffer.String(), "refused") {
		t.Fatal("the failure text is not logged")
	}

	fail = false
	clock.now = clock.now.Add(11 * time.Minute)
	c.publish(action, false)
	if c.failures != 0 || logged(&buffer, "INFO") != 1 {
		t.Fatalf("recovery: failures %d\n%s", c.failures, buffer.String())
	}
	// A success resets the backoff: the next failure waits the base again and a
	// new streak logs again.
	fail = true
	c.publish(action, false)
	if wait := c.retryAt.Sub(clock.now); wait != time.Second {
		t.Fatalf("backoff did not reset: %s", wait)
	}
	if warns := logged(&buffer, "WARN"); warns != 2 {
		t.Fatalf("%d warnings across two streaks", warns)
	}
}

func TestHeartbeatStatusFollowsReadiness(t *testing.T) {
	var statuses []string
	c := collectorFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		statuses = append(statuses, body["status"].(string))
		w.WriteHeader(202)
	}))
	ready := false
	c.config.Ready = func() bool { return ready }
	for _, want := range []bool{false, true, false} {
		ready = want
		if err := c.heartbeat(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(statuses, ","); got != "unknown,online,unknown" {
		t.Fatalf("statuses %s", got)
	}
}

func TestReadinessIsRequired(t *testing.T) {
	_, err := New(Config{Region: "us-west-2", Label: "AWS Oregon", Service: "kaana", Coordinates: [2]float64{-120, 44}, BaseURL: "https://api.oxy.so", Token: func(context.Context) (string, error) { return "t", nil }})
	if err == nil {
		t.Fatal("New must refuse a config with no readiness")
	}
}

func TestPendingBoundAndChunking(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	c := collectorFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []Aggregate
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sizes = append(sizes, len(batch))
		mu.Unlock()
		w.WriteHeader(202)
	}))
	for i := range 700 {
		c.Record(Flow{SourceRegion: "edge-" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)), Direction: "inbound"})
	}
	// An existing flow still counts once the bound is reached.
	c.Record(Flow{SourceRegion: "edge-aaa", Direction: "inbound"})
	if len(c.pending) != 512 {
		t.Fatalf("held %d flows, want 512", len(c.pending))
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0] != 256 || sizes[1] != 256 {
		t.Fatalf("chunks %v, want [256 256]", sizes)
	}

	// Positive control: a smaller set is one smaller chunk.
	sizes = nil
	for i := range 300 {
		c.Record(Flow{SourceRegion: "edge-" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "a", Direction: "outbound"})
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0]+sizes[1] != 300 || max(sizes[0], sizes[1]) > 256 {
		t.Fatalf("chunks %v", sizes)
	}
}

func TestFailedChunkDropsTheRestWithoutRetry(t *testing.T) {
	calls := 0
	c := collectorFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	for i := range 400 {
		c.Record(Flow{SourceRegion: "edge-a" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Direction: "inbound"})
	}
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("a refused batch must report the failure")
	}
	if calls != 1 || len(c.pending) != 0 {
		t.Fatalf("calls %d pending %d: a failed batch is discarded, not queued", calls, len(c.pending))
	}
}

func TestUnlocatedPeersOmitTheRegion(t *testing.T) {
	c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil) // no Cf-Ray
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), request)
	provider := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	response, err := c.Transport(provider).RoundTrip(httptest.NewRequest("POST", "https://provider.example/v1/chat", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	encoded, err := json.Marshal(c.pendingValues())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.pending) != 2 { // the provider call coalesces with the response Kaana itself wrote
		t.Fatalf("flows %d", len(c.pending))
	}
	if strings.Contains(string(encoded), "unknown") {
		t.Fatalf("an unlocated peer must be omitted, not named: %s", encoded)
	}
	// Positive control: a located peer is present.
	located := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	located.Header.Set("Cf-Ray", "abc-MAD")
	c2 := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c2.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), located)
	encoded, _ = json.Marshal(c2.pendingValues())
	if !strings.Contains(string(encoded), `"sourceRegion":"edge-mad"`) {
		t.Fatalf("located peer lost: %s", encoded)
	}
}

// Every path excluded from observation, as the table must treat it. Each is
// checked inbound and, where it can be an outbound destination, on the Oxy
// transport.
var excludedPaths = []string{
	"/livez", "/healthz", "/health", "/health/deep", "/ready", "/readyz", "/live", "/status",
	"/api/health", "/api/ready", "/api/live",
	"/platform-stats", "/platform-activity", "/platform-infrastructure", "/infra-status",
	"/cdn-cgi/trace", "/internal/v1/health",
	"/internal/activity", "/internal/activity/infrastructure", "/auth/service-token",
}

var observedPaths = []string{
	"/v1/chat/completions", "/internal/v1/inference", "/v1/models", "/v1/realtime",
	"/statuspage", "/healthy-models", "/livez-ish/x", "/internal/v1/settlements",
	"/internal/kaana/v1/authorizations", "/api/v1/chat/completions", "/",
}

func TestExcludedPathsProduceNoFlow(t *testing.T) {
	respond := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	for _, path := range excludedPaths {
		t.Run("excluded "+path, func(t *testing.T) {
			c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
			response, err := c.OxyTransport(respond).RoundTrip(httptest.NewRequest("GET", "https://api.oxy.so"+path, nil))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if len(c.pending) != 0 || !Excluded(path) {
				t.Fatalf("%s produced %+v", path, c.pending)
			}
		})
	}
	for _, path := range observedPaths {
		t.Run("observed "+path, func(t *testing.T) {
			c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
			if len(c.pending) != 2 || Excluded(path) {
				t.Fatalf("%s produced %+v", path, c.pending)
			}
		})
	}
}

func TestActivityTypes(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/chat/completions":              "ai",
		"/v1/messages":                      "ai",
		"/v1/responses":                     "ai",
		"/v1/embeddings":                    "ai",
		"/v1/rerank":                        "ai",
		"/v1/models/openai/gpt":             "ai",
		"/v1/realtime":                      "ai",
		"/internal/v1/inference":            "ai",
		"/internal/v1/realtime":             "ai",
		"/v1/audio/speech":                  "media",
		"/v1/images/generations":            "media",
		"/api/v1/chat/completions":          "ai",
		"/internal/kaana/v1/identities":     "identity",
		"/internal/kaana/v1/authorizations": "platform",
		"/internal/kaana/v1/settlements":    "platform",
		"/internal/v1/settlements":          "platform",
		"/internal/v1/deployments/query":    "platform",
		"/unknown":                          "platform",
		"/":                                 "platform",
	} {
		if got := activityType(path); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}

func TestOxyTransportRecordsInternalFlows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		header     string
		wantRegion string
		wantType   string
		fail       bool
	}{
		{"grant", "/internal/kaana/v1/authorizations", "us-west-2", "us-west-2", "platform", false},
		{"identity", "/internal/kaana/v1/identities", "us-west-2", "us-west-2", "identity", false},
		{"settlement with no region header", "/internal/kaana/v1/settlements", "", "", "platform", false},
		{"settlement with a forged region", "/internal/kaana/v1/settlements", "not a region; 203.0.113.9", "", "platform", false},
		{"connection failure", "/internal/kaana/v1/authorizations", "", "", "platform", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			var forwarded *http.Request
			base := transportFunc(func(r *http.Request) (*http.Response, error) {
				forwarded = r
				if tc.fail {
					return nil, errors.New("connection failed")
				}
				header := http.Header{}
				if tc.header != "" {
					header.Set("X-Oxy-Region", tc.header)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: header}, nil
			})
			original := httptest.NewRequest("POST", "https://api.oxy.so"+tc.path+"?secret=value", strings.NewReader("body"))
			response, err := c.OxyTransport(base).RoundTrip(original)
			if tc.fail != (err != nil) {
				t.Fatalf("err %v", err)
			}
			if response != nil {
				_ = response.Body.Close()
			}
			if forwarded.Header.Get("X-Oxy-Source-Region") != "us-west-2" || original.Header.Get("X-Oxy-Source-Region") != "" {
				t.Fatal("the source region goes on a copy of the request, never the caller's")
			}
			want := 2
			if tc.fail {
				want = 1
			}
			if len(c.pending) != want {
				t.Fatalf("flows %+v", c.pending)
			}
			for flow := range c.pending {
				if flow.Scope != "internal" || flow.ActivityType != tc.wantType {
					t.Fatalf("bad flow %+v", flow)
				}
				switch flow.Direction {
				case "outbound":
					if flow.SourceService != "kaana" || flow.TargetService != "oxy-api" || flow.SourceRegion != "us-west-2" || flow.TargetRegion != tc.wantRegion {
						t.Fatalf("bad outbound %+v", flow)
					}
				case "inbound":
					if flow.SourceService != "oxy-api" || flow.TargetService != "kaana" || flow.TargetRegion != "us-west-2" || flow.SourceRegion != tc.wantRegion {
						t.Fatalf("bad inbound %+v", flow)
					}
				}
			}
		})
	}
}

func TestNoFlowCarriesAPathAddressOrAccount(t *testing.T) {
	c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	secrets := []string{"acct_4242", "sk-live-secret", "192.0.2.77", "private-prompt", "session-9", "completions", "authorizations", "Bearer"}
	inbound := httptest.NewRequest("POST", "/v1/chat/completions/acct_4242?key=sk-live-secret", strings.NewReader("private-prompt"))
	inbound.RemoteAddr = "192.0.2.77:1234"
	inbound.Header.Set("Authorization", "Bearer sk-live-secret")
	inbound.Header.Set("X-Forwarded-For", "192.0.2.77")
	inbound.Header.Set("Cookie", "session-9")
	inbound.Header.Set("Cf-Ray", "abc-MAD")
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), inbound)
	respond := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"X-Oxy-Region": {"us-west-2"}}}, nil
	})
	for _, transport := range []http.RoundTripper{c.Transport(respond), c.OxyTransport(respond)} {
		request := httptest.NewRequest("POST", "https://api.oxy.so/internal/kaana/v1/authorizations/acct_4242?key=sk-live-secret", strings.NewReader("private-prompt"))
		request.Header.Set("Authorization", "Bearer sk-live-secret")
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if len(c.pending) < 6 {
		t.Fatalf("expected inbound, provider and Oxy flows, got %d", len(c.pending))
	}
	encoded, err := json.Marshal(c.pendingValues())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("%q escaped into %s", secret, encoded)
		}
	}
}
