// Package openailivetest is a fake OpenAI GPT-Live upstream over a real
// WebSocket, for the adapter's tests and for the session engine's live
// contract suite. Everything it sends is spelled as GPT-Live's reference
// spells it (https://developers.openai.com/api/reference/resources/live/primary-websocket,
// read 2026-11-14): the handshake at wss://api.openai.com/v1/live/sessions,
// session.start answered by session.started, the appends acknowledged by
// their *.appended, and session.close answered by session.closed with the
// session's billed seconds. Every client event it receives is recorded, so a
// test can assert what reached the provider and, as importantly, what never
// did.
package openailivetest

import (
	"context"
	"encoding/base64"
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

// Key and SecondKey are the synthetic credentials the fake accepts. Neither is
// a credential.
const (
	Key       = "sk-openai-live-synthetic-credential-not-valid"
	SecondKey = "sk-openai-live-second-synthetic-credential-not-valid"
)

// Event is one JSON event as it crossed the wire.
type Event map[string]any

// Type is the event's type field.
func (e Event) Type() string {
	value, _ := e["type"].(string)
	return value
}

// Upstream is the fake. Zero-value hooks give a session that starts and then
// waits for the test to drive it through Accept.
type Upstream struct {
	t      testing.TB
	server *httptest.Server

	// Refuse, when set, may answer the handshake itself: it writes a status
	// and an OpenAI error body and returns true, and the upgrade never
	// happens.
	Refuse func(w http.ResponseWriter, r *http.Request) bool
	// Start answers the session.start. Nil starts the session with
	// session.started; a non-nil event (an `error`) refuses it.
	Start func(start Event) Event

	mu       sync.Mutex
	received []Event
	dials    int
	sessions chan *Conn
	closed   chan struct{}
}

// New starts the fake. The caller sets its hooks before the first dial.
func New(t testing.TB) *Upstream {
	t.Helper()
	upstream := &Upstream{t: t, sessions: make(chan *Conn, 16), closed: make(chan struct{}, 16)}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// Client points the adapter's real HTTP client at the fake while asserting the
// handshake left for GPT-Live's own endpoint, with no query, carrying a
// synthetic key as a bearer token.
func (u *Upstream) Client() *http.Client {
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/live/sessions" || r.URL.RawQuery != "" {
			u.t.Errorf("the handshake left for %s, not GPT-Live's endpoint without a query", r.URL.Redacted())
		}
		if authorization := r.Header.Get("Authorization"); authorization != "Bearer "+Key && authorization != "Bearer "+SecondKey {
			u.t.Error("the handshake did not carry a platform credential as a bearer token")
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

// Accept returns the next session the fake started, or fails the test.
func (u *Upstream) Accept(t testing.TB) *Conn {
	t.Helper()
	select {
	case conn := <-u.sessions:
		return conn
	case <-time.After(10 * time.Second):
		t.Fatal("no GPT-Live session started")
		return nil
	}
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
	conn := &Conn{ctx: ctx, ws: ws, upstream: u, events: make(chan Event, 4096), billed: "0"}
	start, err := conn.read(ctx)
	if err != nil {
		return
	}
	if start.Type() != "session.start" {
		u.t.Errorf("the first client event was %q, not session.start", start.Type())
		return
	}
	if u.Start != nil {
		if refusal := u.Start(start); refusal != nil {
			conn.send(ctx, refusal)
			conn.drain(ctx)
			return
		}
	}
	session, _ := start["session"].(map[string]any)
	conn.send(ctx, Event{"type": "session.started", "event_id": "event_started", "client_event_id": start["event_id"],
		"session": Event{"id": "live_fake", "expires_at": time.Now().Add(time.Hour).Unix(), "status": "active", "model": session["model"]}})
	u.sessions <- conn
	conn.serve(ctx)
}

// Conn is the fake's side of one started session.
type Conn struct {
	ctx      context.Context
	ws       *websocket.Conn
	upstream *Upstream
	// events are the client events received after session.start, for Expect.
	events chan Event

	mu sync.Mutex
	// billed is the usage.seconds session.closed reports, as a JSON number.
	billed string
	// withhold makes a session.close end the connection without
	// session.closed: a transport that failed before OpenAI's final report.
	withhold bool
}

// serve reads, recording, until the connection ends: it acknowledges every
// append as GPT-Live does, and answers session.close with session.closed.
func (c *Conn) serve(ctx context.Context) {
	clock := int64(0)
	for {
		event, err := c.read(ctx)
		if err != nil {
			_ = c.ws.CloseNow()
			close(c.events)
			return
		}
		select {
		case c.events <- event:
		default:
			// Received keeps every event; Expect is for the few a test waits on.
		}
		switch event.Type() {
		case "session.commentary.append", "session.thinking.append", "session.instructions.append":
			clock += 10
			c.send(ctx, Event{"type": strings.TrimSuffix(event.Type(), "append") + "appended", "event_id": "event_appended",
				"client_event_id": event["event_id"], "start_ms": clock, "end_ms": clock})
		case "session.close":
			c.mu.Lock()
			withhold, billed := c.withhold, c.billed
			c.mu.Unlock()
			if withhold {
				_ = c.ws.CloseNow()
				continue
			}
			c.closed(ctx, "close_requested", billed)
		}
	}
}

// Bill sets the seconds session.closed reports, a JSON number as OpenAI sends
// it (`usage.seconds` is a number, cumulative).
func (c *Conn) Bill(seconds string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.billed = seconds
}

// WithholdClosed makes the next session.close end the connection without
// OpenAI's session.closed.
func (c *Conn) WithholdClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.withhold = true
}

// Expect returns the next client event after session.start and requires its
// type.
func (c *Conn) Expect(eventType string) Event {
	select {
	case event, open := <-c.events:
		if !open {
			c.upstream.t.Errorf("expected %s from the adapter; the connection ended", eventType)
			return Event{}
		}
		if event.Type() != eventType {
			c.upstream.t.Errorf("expected %s from the adapter, read %s", eventType, event.Type())
		}
		return event
	case <-time.After(10 * time.Second):
		c.upstream.t.Errorf("expected %s from the adapter; none came", eventType)
		return Event{}
	}
}

// Hear is the person's speech: one session.input_transcript.delta.
func (c *Conn) Hear(text string, startMs, endMs int64) {
	c.send(c.ctx, Event{"type": "session.input_transcript.delta", "event_id": "event_heard", "delta": text, "start_ms": startMs, "end_ms": endMs})
}

// Say is the voice's speech: one session.output_transcript.delta.
func (c *Conn) Say(text string, startMs, endMs int64) {
	c.send(c.ctx, Event{"type": "session.output_transcript.delta", "event_id": "event_said", "delta": text, "start_ms": startMs, "end_ms": endMs})
}

// Speak is the voice's audio: one session.output_audio.delta, which carries no
// timing on the primary WebSocket.
func (c *Conn) Speak(audio []byte) {
	c.send(c.ctx, Event{"type": "session.output_audio.delta", "event_id": "event_audio", "delta": base64.StdEncoding.EncodeToString(audio)})
}

// Delegate hands a turn to the client: session.delegation.created, which
// carries no words.
func (c *Conn) Delegate(id string, offsetMs int64) {
	c.send(c.ctx, Event{"type": "session.delegation.created", "event_id": "event_delegation", "offset_ms": offsetMs,
		"delegation": Event{"id": id, "type": "delegation", "target": "client"}})
}

// Usage is a cumulative session.usage.updated.
func (c *Conn) Usage(seconds string) {
	c.SendRaw([]byte(`{"type":"session.usage.updated","event_id":"event_usage","usage":{"seconds":` + seconds + `},"context_window":{"usage_ratio":0.01}}`))
}

// Closed is OpenAI ending the session: session.closed with its reason and
// final billed seconds, then a normal close.
func (c *Conn) Closed(reason, seconds string) { c.closed(c.ctx, reason, seconds) }

func (c *Conn) closed(ctx context.Context, reason, seconds string) {
	c.sendRaw(ctx, []byte(`{"type":"session.closed","event_id":"event_closed","reason":"`+reason+
		`","session":{"id":"live_fake","expires_at":0,"status":"active","model":"gpt-live-1"},"usage":{"seconds":`+seconds+`}}`))
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

// Error is an in-band error event, naming the client event it refuses.
func (c *Conn) Error(kind, code, clientEventID string) {
	failure := Event{"type": kind, "code": code, "message": "the fake refused an event"}
	if clientEventID != "" {
		failure["client_event_id"] = clientEventID
	}
	c.send(c.ctx, Event{"type": "error", "event_id": "event_error", "error": failure})
}

// Send writes one server event.
func (c *Conn) Send(event Event) { c.send(c.ctx, event) }

// SendRaw writes one text frame exactly as given.
func (c *Conn) SendRaw(frame []byte) { c.sendRaw(c.ctx, frame) }

func (c *Conn) sendRaw(parent context.Context, frame []byte) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, frame)
}

// Drop ends the connection without a close handshake or session.closed.
func (c *Conn) Drop() { _ = c.ws.CloseNow() }

func (c *Conn) send(ctx context.Context, event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		c.upstream.t.Errorf("fake upstream encoding: %v", err)
		return
	}
	c.sendRaw(ctx, data)
}

func (c *Conn) read(parent context.Context) (Event, error) {
	kind, data, err := c.ws.Read(parent)
	if err != nil {
		return nil, err
	}
	if kind != websocket.MessageText {
		c.upstream.t.Error("the adapter sent GPT-Live a binary frame")
		return nil, errors.New("binary frame")
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		c.upstream.t.Errorf("the adapter sent GPT-Live invalid JSON: %v", err)
		return nil, err
	}
	c.upstream.mu.Lock()
	c.upstream.received = append(c.upstream.received, event)
	c.upstream.mu.Unlock()
	return event, nil
}

func (c *Conn) drain(ctx context.Context) {
	for {
		if _, err := c.read(ctx); err != nil {
			_ = c.ws.CloseNow()
			return
		}
	}
}
