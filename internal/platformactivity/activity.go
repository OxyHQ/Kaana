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
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// oxyAPIService is Oxy's service name in a flow, as in Oxy's own collector.
const oxyAPIService = "oxy-api"

type Flow struct {
	Region        string `json:"region"`
	SourceRegion  string `json:"sourceRegion,omitempty"`
	TargetRegion  string `json:"targetRegion,omitempty"`
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

// Collection bounds, matching Oxy's collector (packages/telemetry/src/collector.ts):
// maxPendingFlows distinct flows are held between flushes, and each publish
// carries at most publishChunk, the collector's batch limit.
const (
	maxPendingFlows = 512
	publishChunk    = 256
)

// Publish backoff, matching Oxy's createEcosystemTraffic. A refusal will not
// change in two seconds, and a fixed fast retry loop once produced tens of
// refusals a minute on both sides (tnp-relay). The wait after the Nth
// consecutive failure is half of min(max, base*2^(N-1)) plus up to half again
// at random, so a fleet refused at once does not retry in lockstep. A success
// resets it.
const (
	publishBackoffBase = 2 * time.Second
	publishBackoffMax  = 10 * time.Minute
)

// publishBackoff is the wait before the next attempt after failures
// consecutive failures; random returns a value in [0, 1).
func publishBackoff(failures int, random func() float64) time.Duration {
	ceiling := publishBackoffMax
	if exponent := max(0, failures-1); exponent < 30 {
		ceiling = min(publishBackoffMax, publishBackoffBase<<exponent)
	}
	return time.Duration(float64(ceiling)/2 + random()*float64(ceiling)/2).Truncate(time.Millisecond)
}

type Config struct {
	Region string
	Label  string
	// Service names the process publishing activity, e.g. "kaana" or
	// "kaana-publisher". It identifies which binary an aggregate came from and
	// is never inferred from the credential, which several services share.
	Service     string
	Coordinates [2]float64
	// Ready reports whether this process can serve. A heartbeat says `online`
	// only while it returns true and `unknown` otherwise, so the dashboard
	// never shows an instance as serving merely because the process runs. It
	// is required: no default is honest for every binary.
	Ready   func() bool
	BaseURL string
	Token   func(context.Context) (string, error)
	Client  *http.Client
	Logger  *slog.Logger
}
type Collector struct {
	config   Config
	mu       sync.Mutex
	pending  map[Flow]Aggregate
	instance string
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once

	// now and random are the clock and jitter source, replaced in tests.
	now    func() time.Time
	random func() float64
	// failures and retryAt are the publish backoff. Only the worker in Run (or
	// a test driving publish directly) touches them.
	failures int
	retryAt  time.Time
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d$`)
var popPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func New(config Config) (*Collector, error) {
	if math.IsNaN(config.Coordinates[0]) || math.IsNaN(config.Coordinates[1]) || math.Abs(config.Coordinates[0]) > 180 || math.Abs(config.Coordinates[1]) > 90 {
		return nil, errors.New("platform activity: invalid infrastructure coordinates")
	}
	if !regionPattern.MatchString(config.Region) || config.Label == "" || config.Service == "" || config.Token == nil || config.Ready == nil {
		return nil, errors.New("platform activity: region, location, service name, service token and readiness are required")
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
	c := &Collector{config: config, pending: make(map[Flow]Aggregate), instance: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), stop: make(chan struct{}), done: make(chan struct{}), now: time.Now, random: mrand.Float64}
	return c, nil
}
func (c *Collector) Record(flow Flow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flow.Service = c.config.Service
	flow.Region = c.config.Region
	previous, exists := c.pending[flow]
	if !exists && len(c.pending) >= maxPendingFlows {
		return
	}
	if !exists {
		previous = Aggregate{Flow: flow, WindowStartedAt: c.now().UTC().Format(time.RFC3339Nano)}
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
	emittedAt := c.now().UTC().Format(time.RFC3339Nano)
	for _, event := range c.pending {
		event.EmittedAt = emittedAt
		batch = append(batch, event)
	}
	clear(c.pending)
	c.mu.Unlock()
	// A failed chunk drops itself and the rest: there is no durable retry queue.
	for offset := 0; offset < len(batch); offset += publishChunk {
		if err := c.post(ctx, "/internal/activity", batch[offset:min(offset+publishChunk, len(batch))]); err != nil {
			return err
		}
	}
	return nil
}
func (c *Collector) heartbeat(ctx context.Context, removed bool) error {
	status := "unknown"
	if c.config.Ready() {
		status = "online"
	}
	return c.post(ctx, "/internal/activity/infrastructure", struct {
		InstanceID  string     `json:"instanceId"`
		Service     string     `json:"service"`
		Region      string     `json:"region"`
		Label       string     `json:"label"`
		Coordinates [2]float64 `json:"coordinates"`
		Status      string     `json:"status"`
		Removed     bool       `json:"removed"`
	}{c.instance, c.config.Service, c.config.Region, c.config.Label, c.config.Coordinates, status, removed})
}

// publish runs one flush or heartbeat unless an earlier failure still has the
// collector backing off (force overrides that, for the final flush and the
// removal, which must be attempted). The collector keeps aggregating, bounded,
// while it waits. The first failure of a streak is logged and so is the
// recovery; the failures between are not.
func (c *Collector) publish(action func(context.Context) error, force bool) {
	if !force && c.failures > 0 && c.now().Before(c.retryAt) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if action(ctx) != nil {
		c.failures++
		c.retryAt = c.now().Add(publishBackoff(c.failures, c.random))
		if c.failures == 1 {
			c.config.Logger.Warn("platform activity publisher unavailable; backing off", "maxBackoff", publishBackoffMax.String())
		}
		return
	}
	if c.failures > 0 {
		c.failures = 0
		c.config.Logger.Info("platform activity publisher recovered")
	}
}

// Run starts after the listener opens. One worker orders heartbeats and removal.
func (c *Collector) Run() {
	defer close(c.done)
	beat := func(ctx context.Context) error { return c.heartbeat(ctx, false) }
	remove := func(ctx context.Context) error { return c.heartbeat(ctx, true) }
	c.publish(beat, false)
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-flush.C:
			c.publish(c.Flush, false)
		case <-heartbeat.C:
			c.publish(beat, false)
		case <-c.stop:
			c.publish(c.Flush, true)
			c.publish(remove, true)
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
		path := r.URL.Path
		if Excluded(path) {
			next.ServeHTTP(w, r)
			return
		}
		state := &admission{}
		r = r.WithContext(context.WithValue(r.Context(), admissionKey{}, state))
		response := &responseWriter{ResponseWriter: w}
		defer func() {
			// An unlocated peer stays unlocated: the field is omitted, as in Oxy's collector.
			peer := ""
			scope := "external"
			source := ""
			if state.verified {
				scope = "internal"
				// The verified envelope proves a signature by an Oxy-held key
				// id and carries no service name, and Oxy is the only signer
				// of every route that calls MarkVerified, so the source of an
				// internal call is always oxy-api.
				source = oxyAPIService
				if value := r.Header.Get("X-Oxy-Source-Region"); regionPattern.MatchString(value) {
					peer = value
				}
			} else {
				parts := strings.Split(r.Header.Get("Cf-Ray"), "-")
				if len(parts) == 2 && popPattern.MatchString(parts[1]) {
					peer = "edge-" + strings.ToLower(parts[1])
				}
			}
			flow := Flow{SourceRegion: peer, TargetRegion: c.config.Region, SourceService: source, TargetService: c.config.Service, Scope: scope, Direction: "inbound", ActivityType: activityType(path)}
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
	// peer names the Oxy service a call goes to; empty means an upstream
	// provider.
	peer string
}

// Transport observes upstream provider calls: external, `ai`, and with no
// provider location, which is never inferred from a hostname or address.
func (c *Collector) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{collector: c, base: base}
}

// OxyTransport observes Kaana's calls to Oxy (the grant, settlement delivery):
// internal flows to oxy-api. The destination region is the peer's own
// X-Oxy-Region answer, never a guess, and Kaana's region travels to it in
// X-Oxy-Source-Region. Only the request path decides the type; no path, query
// or body is kept.
func (c *Collector) OxyTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{collector: c, base: base, peer: oxyAPIService}
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	if Excluded(path) {
		return t.base.RoundTrip(r)
	}
	flow := Flow{SourceRegion: t.collector.config.Region, SourceService: t.collector.config.Service, Scope: "external", Direction: "outbound", ActivityType: typeAI}
	if t.peer != "" {
		flow.Scope, flow.TargetService, flow.ActivityType = "internal", t.peer, activityType(path)
		// A RoundTripper must not modify the request it is given.
		r = r.Clone(r.Context())
		r.Header.Set("X-Oxy-Source-Region", t.collector.config.Region)
	}
	response, err := t.base.RoundTrip(r)
	if response != nil && t.peer != "" {
		if region := response.Header.Get("X-Oxy-Region"); regionPattern.MatchString(region) {
			flow.TargetRegion = region
		}
	}
	t.collector.Record(flow)
	if response != nil {
		flow.SourceRegion, flow.TargetRegion = flow.TargetRegion, flow.SourceRegion
		flow.SourceService, flow.TargetService = flow.TargetService, flow.SourceService
		flow.Direction = "inbound"
		t.collector.Record(flow)
	}
	return response, err
}
