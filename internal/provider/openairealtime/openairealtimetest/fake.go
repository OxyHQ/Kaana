// Package openairealtimetest is a fake OpenAI-Realtime-shaped upstream over a
// real WebSocket, for the adapter's tests and for the session engine's
// endpoint tests. New is OpenAI's GA Realtime API; NewXAI is xAI's Voice Agent
// API at its own origin, with its own credentials. The handshake and the
// configure-then-confirm open are the two providers' shared wire; everything a
// test Script sends is spelled as that provider's reference spells it
// (https://developers.openai.com/api/reference/resources/realtime/server-events,
// https://docs.x.ai/voice-realtime.ws.json). Every client event the fake
// receives is recorded, so a test can assert what reached the provider and, as
// importantly, what never did.
package openairealtimetest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Key and SecondKey are the synthetic credentials the fake accepts. Neither
// is a credential.
const (
	Key       = "sk-openai-realtime-synthetic-credential-not-valid"
	SecondKey = "sk-openai-realtime-second-synthetic-credential-not-valid"
	// XAIKey and XAISecondKey are what NewXAI accepts.
	XAIKey       = "xai-realtime-synthetic-credential-not-valid"
	XAISecondKey = "xai-realtime-second-synthetic-credential-not-valid"
)

// Event is one JSON event as it crossed the wire.
type Event map[string]any

// Type is the event's type field.
func (e Event) Type() string {
	value, _ := e["type"].(string)
	return value
}

// Upstream is the fake. Zero-value hooks give a session that opens and then
// runs Script.
type Upstream struct {
	t      testing.TB
	server *httptest.Server
	// host and keys are the provider's origin and the credentials it accepts.
	host string
	keys []string

	// Refuse, when set, may answer the handshake itself: it writes a status
	// and an OpenAI error body and returns true, and the upgrade never
	// happens. Returning false lets the handshake proceed.
	Refuse func(w http.ResponseWriter, r *http.Request) bool
	// Configure answers the configuring session.update. Nil confirms it with
	// session.updated; a non-nil event (an `error`) refuses it.
	Configure func(update Event) Event
	// Greeting, when set, replaces the session.created and
	// conversation.created the fake opens with, and Confirmation the
	// session.updated it confirms the configuration with: a test replaying a
	// captured session sends the provider's own events, verbatim.
	Greeting     []Event
	Confirmation Event
	// Script runs once the session is configured.
	Script func(conn *Conn)

	mu       sync.Mutex
	received []Event
	dials    int
	models   []string
	closed   chan struct{}
}

// New starts the fake. The caller sets its hooks before the first dial.
func New(t testing.TB) *Upstream { return start(t, "api.openai.com", Key, SecondKey) }

// NewXAI starts a fake xAI Voice Agent upstream (wss://api.x.ai/v1/realtime).
func NewXAI(t testing.TB) *Upstream { return start(t, "api.x.ai", XAIKey, XAISecondKey) }

func start(t testing.TB, host string, keys ...string) *Upstream {
	t.Helper()
	upstream := &Upstream{t: t, host: host, keys: keys, closed: make(chan struct{}, 16)}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// Client points the adapter's real HTTP client at the fake while asserting the
// handshake left for the provider's own WebSocket endpoint with a synthetic
// key.
func (u *Upstream) Client() *http.Client {
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != u.host || r.URL.Path != "/v1/realtime" {
			u.t.Errorf("the handshake left for %s, not %s's Realtime endpoint", r.URL.Redacted(), u.host)
		}
		authorized := false
		for _, key := range u.keys {
			authorized = authorized || r.Header.Get("Authorization") == "Bearer "+key
		}
		if !authorized {
			u.t.Error("the handshake did not carry a platform credential as a bearer token")
		}
		if r.Header.Get("OpenAI-Beta") != "" {
			u.t.Error("the GA handshake carried the beta header")
		}
		copied := r.Clone(r.Context())
		copied.URL.Scheme, copied.URL.Host = "http", strings.TrimPrefix(u.server.URL, "http://")
		return http.DefaultTransport.RoundTrip(copied)
	})}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Received is every client event the fake read, in order.
func (u *Upstream) Received() []Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Event(nil), u.received...)
}

// ReceivedTypes is the type of every client event the fake read, in order.
func (u *Upstream) ReceivedTypes() []string {
	types := make([]string, 0)
	for _, event := range u.Received() {
		types = append(types, event.Type())
	}
	return types
}

// Dials is how many handshakes reached the fake, refused ones included.
func (u *Upstream) Dials() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dials
}

// Models is the ?model= of every handshake.
func (u *Upstream) Models() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// WaitClosed blocks until one session's connection has ended on the fake's
// side, or fails the test.
func (u *Upstream) WaitClosed(t testing.TB) {
	t.Helper()
	select {
	case <-u.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream connection was never closed")
	}
}

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.dials++
	u.models = append(u.models, r.URL.Query().Get("model"))
	u.mu.Unlock()
	if u.Refuse != nil && u.Refuse(w, r) {
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		u.t.Errorf("fake upstream accept: %v", err)
		return
	}
	defer func() { u.closed <- struct{}{} }()
	ws.SetReadLimit(32 << 20)
	ctx := r.Context()
	conn := &Conn{ctx: ctx, ws: ws, upstream: u}
	greeting := u.Greeting
	if greeting == nil {
		greeting = []Event{
			{"type": "session.created", "event_id": "event_created", "session": Event{"type": "realtime", "object": "realtime.session", "id": "sess_fake"}},
			{"type": "conversation.created", "event_id": "event_conversation", "conversation": Event{"id": "conv_fake", "object": "realtime.conversation"}},
		}
	}
	for _, event := range greeting {
		conn.send(ctx, event)
	}
	update, err := conn.read(ctx)
	if err != nil {
		return
	}
	if update.Type() != "session.update" {
		u.t.Errorf("the first client event was %q, not session.update", update.Type())
		return
	}
	if u.Configure != nil {
		if refusal := u.Configure(update); refusal != nil {
			conn.send(ctx, refusal)
			conn.waitForClose(ctx)
			return
		}
	}
	confirmation := u.Confirmation
	if confirmation == nil {
		confirmation = Event{"type": "session.updated", "event_id": "event_updated", "session": update["session"]}
	}
	conn.send(ctx, confirmation)
	if u.Script != nil {
		u.Script(conn)
	}
	conn.waitForClose(ctx)
}

// Conn is the fake's side of one session.
type Conn struct {
	ctx      context.Context
	ws       *websocket.Conn
	upstream *Upstream
}

// Read returns the next client event.
func (c *Conn) Read() (Event, error) { return c.read(c.ctx) }

func (c *Conn) read(parent context.Context) (Event, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	kind, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if kind != websocket.MessageText {
		c.upstream.t.Error("the adapter sent OpenAI a binary frame")
		return nil, errors.New("binary frame")
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		c.upstream.t.Errorf("the adapter sent OpenAI invalid JSON: %v", err)
		return nil, err
	}
	c.upstream.mu.Lock()
	c.upstream.received = append(c.upstream.received, event)
	c.upstream.mu.Unlock()
	return event, nil
}

// Expect reads the next client event and requires its type.
func (c *Conn) Expect(eventType string) Event {
	event, err := c.Read()
	if err != nil {
		c.upstream.t.Errorf("expected %s from the adapter: %v", eventType, err)
		return Event{}
	}
	if event.Type() != eventType {
		c.upstream.t.Errorf("expected %s from the adapter, read %s", eventType, event.Type())
	}
	return event
}

// Send writes one server event.
func (c *Conn) Send(event Event) { c.send(c.ctx, event) }

func (c *Conn) send(parent context.Context, event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		c.upstream.t.Errorf("fake upstream encoding: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, data)
}

// SendRaw writes one text frame exactly as given, for a server event that is
// not (or not quite) what the provider documents.
func (c *Conn) SendRaw(frame []byte) {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, frame)
}

// Close ends the session from OpenAI's side.
func (c *Conn) Close(code websocket.StatusCode) {
	_ = c.ws.Close(code, "")
}

// Drop ends the connection without a close handshake.
func (c *Conn) Drop() { _ = c.ws.CloseNow() }

// WaitForClose reads, recording, until the adapter closes the connection.
func (c *Conn) WaitForClose() { c.waitForClose(c.ctx) }

func (c *Conn) waitForClose(ctx context.Context) {
	for {
		if _, err := c.read(ctx); err != nil {
			_ = c.ws.CloseNow()
			return
		}
	}
}

// Usage is a response.done usage block in OpenAI's nesting.
func Usage(text, audio, cachedText, cachedAudio, outputText, outputAudio int) Event {
	return Event{
		"total_tokens":  text + audio + outputText + outputAudio,
		"input_tokens":  text + audio,
		"output_tokens": outputText + outputAudio,
		"input_token_details": Event{
			"text_tokens": text, "audio_tokens": audio, "image_tokens": 0,
			"cached_tokens":         cachedText + cachedAudio,
			"cached_tokens_details": Event{"text_tokens": cachedText, "audio_tokens": cachedAudio, "image_tokens": 0},
		},
		"output_token_details": Event{"text_tokens": outputText, "audio_tokens": outputAudio},
	}
}
