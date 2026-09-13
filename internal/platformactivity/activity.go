// Package platformactivity publishes bounded, anonymous operational aggregates.
// It never reads client addresses, request bodies, identities or provider keys.
package platformactivity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Flow struct {
	Region        string `json:"region"`
	SourceRegion  string `json:"sourceRegion"`
	TargetRegion  string `json:"targetRegion"`
	SourceService string `json:"sourceService,omitempty"`
	TargetService string `json:"targetService,omitempty"`
	Service       string `json:"service"`
	Scope         string `json:"scope"`
	Direction     string `json:"direction"`
	ActivityType  string `json:"activityType"`
}
type Aggregate struct {
	Flow
	Requests        int    `json:"requests"`
	WindowStartedAt string `json:"windowStartedAt"`
	EmittedAt       string `json:"emittedAt"`
}
type Config struct {
	Region      string
	Label       string
	Coordinates [2]float64
	BaseURL     string
	Token       func(context.Context) (string, error)
	Client      *http.Client
	Logger      *slog.Logger
}
type Collector struct {
	config   Config
	mu       sync.Mutex
	pending  map[Flow]Aggregate
	instance string
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d$`)
var popPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func New(config Config) (*Collector, error) {
	if math.IsNaN(config.Coordinates[0]) || math.IsNaN(config.Coordinates[1]) || math.Abs(config.Coordinates[0]) > 180 || math.Abs(config.Coordinates[1]) > 90 {
		return nil, errors.New("platform activity: invalid infrastructure coordinates")
	}
	if !regionPattern.MatchString(config.Region) || config.Label == "" || config.Token == nil {
		return nil, errors.New("platform activity: region, location and service token are required")
	}
	origin, err := url.Parse(config.BaseURL)
	if err != nil || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.String() != "https://api.oxy.so" && (origin.Scheme != "http" || (origin.Hostname() != "127.0.0.1" && origin.Hostname() != "localhost"))) {
		return nil, errors.New("platform activity: invalid collector origin")
	}
	if config.Client == nil {
		config.Client = &http.Client{}
	}
	client := *config.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	config.Client = &client
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("platform activity: instance identity: %w", err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	c := &Collector{config: config, pending: make(map[Flow]Aggregate), instance: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), stop: make(chan struct{}), done: make(chan struct{})}
	return c, nil
}
func (c *Collector) Record(flow Flow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flow.Service = "kaana"
	flow.Region = c.config.Region
	previous, exists := c.pending[flow]
	if !exists && len(c.pending) >= 256 {
		return
	}
	if !exists {
		previous = Aggregate{Flow: flow, WindowStartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	if previous.Requests < 1_000_000 {
		previous.Requests++
	}
	c.pending[flow] = previous
}
func (c *Collector) post(ctx context.Context, path string, payload any) error {
	token, err := c.config.Token(ctx)
	if err != nil {
		return errors.New("platform activity: service authentication failed")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("platform activity: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("platform activity: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.config.Client.Do(req)
	if err != nil {
		return errors.New("platform activity: collector unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("platform activity: collector returned HTTP %d", response.StatusCode)
	}
	return nil
}
func (c *Collector) Flush(ctx context.Context) error {
	c.mu.Lock()
	batch := make([]Aggregate, 0, len(c.pending))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, event := range c.pending {
		event.EmittedAt = now
		batch = append(batch, event)
	}
	clear(c.pending)
	c.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	return c.post(ctx, "/internal/activity", batch)
}
func (c *Collector) heartbeat(ctx context.Context, removed bool) error {
	return c.post(ctx, "/internal/activity/infrastructure", struct {
		InstanceID  string     `json:"instanceId"`
		Service     string     `json:"service"`
		Region      string     `json:"region"`
		Label       string     `json:"label"`
		Coordinates [2]float64 `json:"coordinates"`
		Status      string     `json:"status"`
		Removed     bool       `json:"removed"`
	}{c.instance, "kaana", c.config.Region, c.config.Label, c.config.Coordinates, "online", removed})
}

// Run starts after the listener opens. One worker orders heartbeats and removal.
func (c *Collector) Run() {
	defer close(c.done)
	perform := func(action func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if action(ctx) != nil {
			c.config.Logger.Warn("platform activity publisher unavailable")
		}
	}
	perform(func(ctx context.Context) error { return c.heartbeat(ctx, false) })
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-flush.C:
			perform(c.Flush)
		case <-heartbeat.C:
			perform(func(ctx context.Context) error { return c.heartbeat(ctx, false) })
		case <-c.stop:
			perform(c.Flush)
			perform(func(ctx context.Context) error { return c.heartbeat(ctx, true) })
			return
		}
	}
}
func (c *Collector) Close(ctx context.Context) error {
	c.once.Do(func() { close(c.stop) })
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type admissionKey struct{}
type admission struct{ verified bool }

// MarkVerified is called only after the existing Ed25519 verifier succeeds.
func MarkVerified(ctx context.Context) {
	if state, ok := ctx.Value(admissionKey{}).(*admission); ok {
		state.verified = true
	}
}
func (c *Collector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oxy-Region", c.config.Region)
		if r.URL.Path == "/livez" || r.URL.Path == "/internal/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		state := &admission{}
		r = r.WithContext(context.WithValue(r.Context(), admissionKey{}, state))
		response := &responseWriter{ResponseWriter: w}
		defer func() {
			peer := "unknown"
			scope := "external"
			source := ""
			if state.verified {
				scope = "internal"
				source = "oxy-api"
				if value := r.Header.Get("X-Oxy-Source-Region"); regionPattern.MatchString(value) {
					peer = value
				}
			} else {
				parts := strings.Split(r.Header.Get("Cf-Ray"), "-")
				if len(parts) == 2 && popPattern.MatchString(parts[1]) {
					peer = "edge-" + strings.ToLower(parts[1])
				}
			}
			flow := Flow{SourceRegion: peer, TargetRegion: c.config.Region, SourceService: source, TargetService: "kaana", Scope: scope, Direction: "inbound", ActivityType: "ai"}
			c.Record(flow)
			if response.wrote {
				flow.SourceRegion, flow.TargetRegion = flow.TargetRegion, flow.SourceRegion
				flow.SourceService, flow.TargetService = flow.TargetService, flow.SourceService
				flow.Direction = "outbound"
				c.Record(flow)
			}
		}()
		next.ServeHTTP(response, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *responseWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.wrote = true
	}
	return n, err
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
	w.wrote = true
}

type transport struct {
	collector *Collector
	base      http.RoundTripper
}

func (c *Collector) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{c, base}
}
func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	flow := Flow{SourceRegion: t.collector.config.Region, TargetRegion: "unknown", SourceService: "kaana", Scope: "external", Direction: "outbound", ActivityType: "ai"}
	response, err := t.base.RoundTrip(r)
	// A provider location is not inferred from its hostname, IP, or a user header.
	t.collector.Record(flow)
	if response != nil {
		flow.SourceRegion, flow.TargetRegion = flow.TargetRegion, flow.SourceRegion
		flow.SourceService, flow.TargetService = flow.TargetService, flow.SourceService
		flow.Direction = "inbound"
		t.collector.Record(flow)
	}
	return response, err
}
