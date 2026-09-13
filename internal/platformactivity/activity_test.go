package platformactivity

import (
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

func collectorFor(t *testing.T, handler http.Handler) *Collector {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(Config{Region: "us-west-2", Label: "AWS Oregon", Coordinates: [2]float64{-120, 44}, BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test-token", nil }, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestVerifiedInboundAndActualResponse(t *testing.T) {
	var received []Aggregate
	c := collectorFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing service token")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(202)
	}))
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MarkVerified(r.Context())
		w.WriteHeader(200)
		_, _ = w.Write([]byte("stream"))
		_ = http.NewResponseController(w).Flush()
	})).ServeHTTP(httptest.NewRecorder(), requestWithRegion())
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 {
		t.Fatalf("got %d flows", len(received))
	}
	for _, event := range received {
		if event.Scope != "internal" || event.ActivityType != "ai" || event.Requests != 1 {
			t.Fatalf("bad flow %+v", event)
		}
		if event.Direction == "inbound" && event.SourceRegion != "eu-west-1" {
			t.Fatal(event)
		}
		if event.Direction == "outbound" && event.TargetRegion != "eu-west-1" {
			t.Fatal(event)
		}
	}
}
func requestWithRegion() *http.Request {
	r := httptest.NewRequest("POST", "/internal/v1/inference", strings.NewReader("private-prompt"))
	r.Header.Set("X-Oxy-Source-Region", "eu-west-1")
	r.Header.Set("Cf-Ray", "abcdef-MAD")
	r.RemoteAddr = "192.0.2.123:5432"
	return r
}
func TestUnverifiedRegionCannotForgeInternalAndNoInventedResponse(t *testing.T) {
	c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), requestWithRegion())
	if len(c.pending) != 1 {
		t.Fatalf("invented response: %+v", c.pending)
	}
	for flow := range c.pending {
		if flow.Scope != "external" || flow.SourceRegion != "edge-mad" || flow.SourceService != "" {
			t.Fatal(flow)
		}
	}
	bytes, err := json.Marshal(c.pendingValues())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"192.0.2.123", "private-prompt", "abcdef", "eu-west-1"} {
		if strings.Contains(string(bytes), private) {
			t.Fatalf("private or untrusted data escaped: %s", private)
		}
	}
}
func (c *Collector) pendingValues() []Aggregate {
	result := []Aggregate{}
	for _, value := range c.pending {
		result = append(result, value)
	}
	return result
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestProviderResponseControlAndFailure(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "response", false: "failure"}[success], func(t *testing.T) {
			c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			base := transportFunc(func(r *http.Request) (*http.Response, error) {
				if success {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("result")), Header: http.Header{}}, nil
				}
				return nil, errors.New("connection failed")
			})
			response, err := c.Transport(base).RoundTrip(httptest.NewRequest("POST", "https://provider.example/v1/chat?private=value", strings.NewReader("private")))
			if success {
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if len(c.pending) != 2 {
					t.Fatal(c.pending)
				}
			} else if len(c.pending) != 1 {
				t.Fatal("failure invented response")
			}
			for flow := range c.pending {
				if flow.Scope != "external" || flow.ActivityType != "ai" {
					t.Fatal(flow)
				}
			}
		})
	}
}
func TestHeartbeatRemovalIsLastAndSameInstance(t *testing.T) {
	var mu sync.Mutex
	var records []map[string]any
	ready := make(chan struct{}, 1)
	c := collectorFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			t.Error(err)
		}
		mu.Lock()
		records = append(records, value)
		mu.Unlock()
		select {
		case ready <- struct{}{}:
		default:
		}
		w.WriteHeader(202)
	}))
	go c.Run()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("no registration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 2 || records[0]["removed"] != false || records[1]["removed"] != true || records[0]["instanceId"] != records[1]["instanceId"] {
		t.Fatalf("bad lifecycle %+v", records)
	}
}
func TestHealthExcludedAndBatchBounded(t *testing.T) {
	c := collectorFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/livez", nil))
	if len(c.pending) != 0 {
		t.Fatal("health counted")
	}
	for i := 0; i < 300; i++ {
		c.Record(Flow{SourceRegion: string(rune(i)), Direction: "inbound"})
	}
	if len(c.pending) != 256 {
		t.Fatal(len(c.pending))
	}
}
