// Package xaistttest is a fake of xAI's streaming speech-to-text over a real
// WebSocket, for the adapter's tests and the session engine's: the handshake
// at `wss://api.x.ai/v1/stt` with a bearer key and query-parameter
// configuration, `transcript.created`, raw binary audio frames and JSON control
// messages in, and whatever a test Script sends out, spelled as xAI's guide
// spells it (https://docs.x.ai/developers/model-capabilities/audio/speech-to-text).
// Every frame the adapter sends is recorded, so a test can assert what reached
// xAI and what never did.
package xaistttest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Key and SecondKey are the synthetic credentials the fake accepts. Neither is
// a credential.
const (
	Key       = "xai-stt-synthetic-credential-not-valid"
	SecondKey = "xai-stt-second-synthetic-credential-not-valid"
)

// Event is one JSON event as it crossed the wire.
type Event map[string]any

// Type is the event's type field.
func (e Event) Type() string {
	value, _ := e["type"].(string)
	return value
}

// Frame is one frame the adapter sent: audio bytes, or a control event.
type Frame struct {
	Audio   []byte
	Control Event
}

// Upstream is the fake. Zero-value hooks give a session that opens and then
// runs Script.
type Upstream struct {
	t      testing.TB
	server *httptest.Server

	// Refuse, when set, may answer the handshake itself: it writes a status
	// and a body and returns true, and the upgrade never happens.
	Refuse func(w http.ResponseWriter, r *http.Request) bool
	// Ready replaces the transcript.created the fake opens with; a test that
	// refuses in-band sends an `error` instead.
	Ready Event
	// Script runs once the session is ready.
	Script func(conn *Conn)

	mu      sync.Mutex
	frames  []Frame
	queries []url.Values
	closed  chan struct{}
}

// New starts the fake. The caller sets its hooks before the first dial.
func New(t testing.TB) *Upstream {
	t.Helper()
	upstream := &Upstream{t: t, closed: make(chan struct{}, 16)}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// Client points the adapter's real HTTP client at the fake while asserting the
// handshake left for xAI's own speech-to-text endpoint with a synthetic key.
func (u *Upstream) Client() *http.Client {
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.x.ai" || r.URL.Path != "/v1/stt" {
			u.t.Errorf("the handshake left for %s, not xAI's speech-to-text endpoint", r.URL.Redacted())
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

// Frames is every frame the fake read, in order.
func (u *Upstream) Frames() []Frame {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Frame(nil), u.frames...)
}

// Queries is each handshake's query parameters, in order.
func (u *Upstream) Queries() []url.Values {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]url.Values(nil), u.queries...)
}

// WaitClosed waits until one session's connection has ended.
func (u *Upstream) WaitClosed(t testing.TB) {
	t.Helper()
	select {
	case <-u.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake upstream's session did not end")
	}
}

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.queries = append(u.queries, r.URL.Query())
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
	ctx := r.Context()
	conn := &Conn{ctx: ctx, ws: ws, upstream: u}
	ready := u.Ready
	if ready == nil {
		ready = Event{"type": "transcript.created"}
	}
	conn.send(ctx, ready)
	if ready.Type() == "transcript.created" && u.Script != nil {
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

// Read returns the next frame the adapter sent.
func (c *Conn) Read() (Frame, error) { return c.read(c.ctx) }

func (c *Conn) read(parent context.Context) (Frame, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	kind, data, err := c.ws.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	frame := Frame{}
	if kind == websocket.MessageBinary {
		frame.Audio = data
	} else {
		var event Event
		if err := json.Unmarshal(data, &event); err != nil {
			c.upstream.t.Errorf("the adapter sent xAI invalid JSON: %v", err)
			return Frame{}, err
		}
		frame.Control = event
	}
	c.upstream.mu.Lock()
	c.upstream.frames = append(c.upstream.frames, frame)
	c.upstream.mu.Unlock()
	return frame, nil
}

// ExpectAudio reads the next frame and requires it to be audio.
func (c *Conn) ExpectAudio() []byte {
	frame, err := c.Read()
	if err != nil || frame.Audio == nil {
		c.upstream.t.Errorf("expected an audio frame from the adapter: %v %v", err, frame.Control)
	}
	return frame.Audio
}

// ExpectControl reads the next frame and requires it to be the control event.
func (c *Conn) ExpectControl(eventType string) Event {
	frame, err := c.Read()
	if err != nil || frame.Control.Type() != eventType {
		c.upstream.t.Errorf("expected %s from the adapter, read %v (%v)", eventType, frame.Control, err)
	}
	return frame.Control
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

// Close ends the session from xAI's side.
func (c *Conn) Close(code websocket.StatusCode) { _ = c.ws.Close(code, "") }

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

// Partial is xAI's transcript.partial: interim (`final` false), a locked
// chunk (`final` true, `speechFinal` false) or the utterance's stitched final.
func Partial(text string, final, speechFinal bool) Event {
	return Event{"type": "transcript.partial", "text": text, "words": []any{}, "is_final": final, "speech_final": speechFinal, "start": 0.0, "duration": 1.0}
}

// Error is xAI's in-band error, which documents only a message.
func Error(message string) Event { return Event{"type": "error", "message": message} }
