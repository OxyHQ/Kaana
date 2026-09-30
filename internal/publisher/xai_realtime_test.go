package publisher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

const xaiVoiceModel = "grok-voice-think-fast-2.0"

// sessionCreated is the event xAI sent the production key on 2026-09-30, the
// session id and event id replaced: session.created first, unprompted.
func sessionCreated(model string) string {
	return `{"type":"session.created","event_id":"evt_1","session":{"id":"sess_1","object":"realtime.session","instructions":"","voice":"xai_ara","modalities":["audio"],"turn_detection":{"type":null},"tools":[],"model":"` + model + `"}}`
}

const (
	conversationCreated = `{"type":"conversation.created","event_id":"evt_2","conversation":{"id":"conv_1","object":"realtime.conversation"},"previous_item_id":null}`
	xaiPing             = `{"type":"ping","event_id":"evt_3","timestamp":1790796880889,"previous_item_id":null}`
)

// fakeXAIVoice serves xAI's real wire for this profile: the OpenAI-shaped
// GET /v1/models (text models only, as production answers) and the Voice
// Agent WebSocket at /v1/realtime, which answers with `open` — a script of
// events written to the socket before the server waits for the client to
// close. It records every session it upgraded and anything the client wrote.
type fakeXAIVoice struct {
	server *httptest.Server
	listed string
	open   func(ctx context.Context, conn *websocket.Conn)
	// refuse, when non-zero, answers the upgrade with this status.
	refuse int

	mu       sync.Mutex
	sessions []string // the ?model= of every upgraded session
	written  int      // data frames the client sent
}

func newFakeXAIVoice(t *testing.T, events ...string) *fakeXAIVoice {
	t.Helper()
	fake := &fakeXAIVoice{listed: `{"data":[{"id":"grok-4.6"},{"id":"grok-imagine-image"}]}`}
	fake.open = func(ctx context.Context, conn *websocket.Conn) {
		for _, event := range events {
			if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
				return
			}
		}
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("%s was asked without the discovery credential", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fake.listed))
		case "/v1/realtime":
			if fake.refuse != 0 {
				http.Error(w, `{"code":"Team is not authorized","error":"test-key is not allowed"}`, fake.refuse)
				return
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accepting the session: %v", err)
				return
			}
			fake.mu.Lock()
			fake.sessions = append(fake.sessions, r.URL.Query().Get("model"))
			fake.mu.Unlock()
			fake.open(r.Context(), conn)
			// Wait for the client's close; count anything it wrote first.
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
				fake.mu.Lock()
				fake.written++
				fake.mu.Unlock()
			}
		default:
			t.Errorf("unexpected discovery endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeXAIVoice) provider(attributed ...string) Provider {
	return Provider{
		Slug: "xai-realtime", BaseURL: f.server.URL + "/v1", APIKey: "test-key",
		Discovery: providerconfig.DiscoveryXAIRealtimeSessions, Protocol: providerconfig.ProtocolXAIRealtime,
		AttributedModels: attributed,
	}
}

func (f *fakeXAIVoice) opened() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sessions...)
}

func (f *fakeXAIVoice) clientWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

func discoveredIDs(models []DiscoveredModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.UpstreamModelID)
	}
	return ids
}

func hasID(models []DiscoveredModel, id string) bool {
	for _, model := range models {
		if model.UpstreamModelID == id {
			return true
		}
	}
	return false
}

// TestXAIVoiceIsDiscoveredOnlyWhenSessionCreatedNamesIt is the positive
// control and its answered negatives: a session.created naming exactly the
// asked-for model discovers it; an in-band error, or xAI naming another model
// (its silent substitution of a bogus id, measured in production), leaves it
// absent without failing discovery.
func TestXAIVoiceIsDiscoveredOnlyWhenSessionCreatedNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   bool
	}{
		{"session.created names the model", []string{sessionCreated(xaiVoiceModel), conversationCreated, xaiPing}, true},
		{"session.created after other events", []string{conversationCreated, xaiPing, sessionCreated(xaiVoiceModel)}, true},
		{"an error event", []string{`{"type":"error","event_id":"evt_e","error":{"type":"invalid_request_error","code":"invalid_request_error","message":"model not available"}}`}, false},
		{"xAI substitutes another model", []string{sessionCreated("grok-voice-think-fast-3.0"), conversationCreated}, false},
		{"xAI substitutes its alias", []string{sessionCreated("grok-voice-latest")}, false},
		{"a case-folded name", []string{sessionCreated(strings.ToUpper(xaiVoiceModel))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeXAIVoice(t, tc.events...)
			models, err := Discover(context.Background(), fake.server.Client(), fake.provider(xaiVoiceModel))
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got := hasID(models, xaiVoiceModel); got != tc.want {
				t.Fatalf("voice model discovered = %t, want %t (models %v)", got, tc.want, discoveredIDs(models))
			}
			// The list is still read and passed through; the snapshot builder,
			// not discovery, drops its text models under xai-realtime.
			if !hasID(models, "grok-4.6") {
				t.Fatalf("the account list was not read: %v", discoveredIDs(models))
			}
			if opened := fake.opened(); !reflect.DeepEqual(opened, []string{xaiVoiceModel}) {
				t.Fatalf("sessions opened for %v, want exactly one for the attributed model", opened)
			}
			// Read-only: the probe must never send xAI a billable event.
			deadline := time.Now().Add(200 * time.Millisecond)
			for fake.clientWrites() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if writes := fake.clientWrites(); writes != 0 {
				t.Fatalf("the probe wrote %d frames to the session", writes)
			}
		})
	}
}

// TestAnUnansweredXAIVoiceProbeFailsThatProvidersDiscovery: no answer is not
// "not served", it is "could not ask" — the speech profile's precedent — so
// discovery fails for the cycle and names neither the model nor the key.
func TestAnUnansweredXAIVoiceProbeFailsThatProvidersDiscovery(t *testing.T) {
	previous := xaiRealtimeProbeTimeout
	xaiRealtimeProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { xaiRealtimeProbeTimeout = previous })

	for _, tc := range []struct {
		name   string
		mutate func(*fakeXAIVoice)
	}{
		{"closed before session.created", func(f *fakeXAIVoice) {
			f.open = func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Close(websocket.StatusInternalError, "test-key rejected")
			}
		}},
		{"silent until the timeout", func(f *fakeXAIVoice) {
			f.open = func(context.Context, *websocket.Conn) { time.Sleep(3 * time.Second) }
		}},
		{"handshake refused", func(f *fakeXAIVoice) { f.refuse = http.StatusForbidden }},
		{"a binary frame", func(f *fakeXAIVoice) {
			f.open = func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageBinary, []byte{0, 1})
			}
		}},
		{"not JSON", func(f *fakeXAIVoice) {
			f.open = func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte("test-key"))
			}
		}},
		{"session.created without a session", func(f *fakeXAIVoice) {
			f.open = func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
			}
		}},
		{"too many events before session.created", func(f *fakeXAIVoice) {
			f.open = func(ctx context.Context, conn *websocket.Conn) {
				for range maxXAIRealtimeProbeEvents {
					_ = conn.Write(ctx, websocket.MessageText, []byte(xaiPing))
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(sessionCreated(xaiVoiceModel)))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeXAIVoice(t)
			tc.mutate(fake)
			started := time.Now()
			models, err := Discover(context.Background(), fake.server.Client(), fake.provider(xaiVoiceModel))
			if err == nil {
				t.Fatalf("an unanswered probe was accepted as discovery: %v", discoveredIDs(models))
			}
			if models != nil {
				t.Fatalf("a failed discovery returned models: %v", discoveredIDs(models))
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Fatalf("the credential reached the error: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("the probe took %s; it is bounded by its own timeout", elapsed)
			}
		})
	}
}

// TestXAIVoiceDiscoveryAsksOnlyAboutReviewedVoiceModels: the profile never
// invents an id. It asks about attributed voice models only, not the text
// models attributed alongside them, not a model the account list already
// names, and nothing at all when nothing is attributed.
func TestXAIVoiceDiscoveryAsksOnlyAboutReviewedVoiceModels(t *testing.T) {
	t.Run("nothing attributed", func(t *testing.T) {
		fake := newFakeXAIVoice(t, sessionCreated(xaiVoiceModel))
		models, err := Discover(context.Background(), fake.server.Client(), fake.provider())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if opened := fake.opened(); len(opened) != 0 || hasID(models, xaiVoiceModel) {
			t.Fatalf("sessions %v, models %v: a model nobody attributed was asked about", opened, discoveredIDs(models))
		}
	})
	t.Run("a text model attributed", func(t *testing.T) {
		// grok-4.7 is attributed but absent from the account list, so only the
		// family filter keeps a session from being opened for a text model.
		fake := newFakeXAIVoice(t, sessionCreated("grok-4.7"))
		if _, err := Discover(context.Background(), fake.server.Client(), fake.provider("grok-4.7", xaiVoiceModel)); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if opened := fake.opened(); !reflect.DeepEqual(opened, []string{xaiVoiceModel}) {
			t.Fatalf("sessions opened for %v, want only the voice model", opened)
		}
	})
	t.Run("already listed", func(t *testing.T) {
		fake := newFakeXAIVoice(t, sessionCreated(xaiVoiceModel))
		fake.listed = `{"data":[{"id":"grok-4.6"},{"id":"` + xaiVoiceModel + `"}]}`
		models, err := Discover(context.Background(), fake.server.Client(), fake.provider(xaiVoiceModel))
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if opened := fake.opened(); len(opened) != 0 || !hasID(models, xaiVoiceModel) {
			t.Fatalf("sessions %v, models %v: a listed model is discovered without a session", opened, discoveredIDs(models))
		}
	})
	t.Run("another slug", func(t *testing.T) {
		fake := newFakeXAIVoice(t, sessionCreated(xaiVoiceModel))
		target := fake.provider(xaiVoiceModel)
		target.Slug = "xai"
		if _, err := Discover(context.Background(), fake.server.Client(), target); err == nil {
			t.Fatal("the voice session profile ran under a slug other than xai-realtime")
		}
		if opened := fake.opened(); len(opened) != 0 {
			t.Fatalf("sessions opened under the wrong slug: %v", opened)
		}
	})
}

// TestTheXAIVoiceSessionIsTheServingAdaptersEndpoint: on the locked root the
// probe dials exactly the endpoint the adapter serves on.
func TestTheXAIVoiceSessionIsTheServingAdaptersEndpoint(t *testing.T) {
	endpoint, err := xaiRealtimeSessionEndpoint(Provider{Slug: "xai-realtime", BaseURL: providerconfig.XAIRealtimeBaseURL}, xaiVoiceModel)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(providerconfig.XAIRealtimeSessionURL, "wss://", "https://", 1) + "?model=" + xaiVoiceModel
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
}

// TestThePublisherPublishesAnObservedXAIVoiceModel is the end-to-end positive
// control: the publisher hands the profile the attribution table's reviewed
// ids, the session evidence becomes a deployment, and xAI's text models stay
// out of the voice slug.
func TestThePublisherPublishesAnObservedXAIVoiceModel(t *testing.T) {
	fake := newFakeXAIVoice(t, sessionCreated(xaiVoiceModel), conversationCreated)
	table, err := ParseAttribution([]byte(`{"attribution":{"xai-realtime":{"grok-voice-think-fast-2.0":"x-ai/grok-voice-think-fast-2.0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	target := fake.provider()
	store := &fakeStore{}
	inventoryPublisher, err := New(Config{
		Providers: []Provider{target}, Attribution: table, Store: store,
		Client: fake.server.Client(), Logger: quietLogger(),
		Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventoryPublisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	published := parseSnapshot(t, store.written()[0])
	if len(published.Deployments) != 1 {
		t.Fatalf("published %d deployments: %+v", len(published.Deployments), published.Deployments)
	}
	deployment := published.Deployments[0]
	if deployment.Provider != "xai-realtime" || deployment.UpstreamModelID != xaiVoiceModel ||
		deployment.ModelReference != "x-ai/grok-voice-think-fast-2.0@observed-2026-09-30" {
		t.Fatalf("deployment = %+v", deployment)
	}
}
