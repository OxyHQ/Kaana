package realtime_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openairealtime"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/realtime"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

const (
	modelReference = contract.ModelReference("openai/gpt-realtime-2.1@observed-2026-09-30")
	primary        = contract.DeploymentID("dep_realtime_a")
	secondary      = contract.DeploymentID("dep_realtime_b")
	textRoute      = contract.DeploymentID("dep_text_chat")
)

// textAdapter is a request adapter under the `openai` slug. A session routed
// to it must be refused: it executes requests and holds no session.
type textAdapter struct{ pool *provider.KeyPool }

func (textAdapter) Provider() contract.ProviderSlug { return "openai" }
func (textAdapter) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatChatCompletions}
}
func (textAdapter) Translate(*contract.Request, provider.Route) (*provider.Call, error) {
	return nil, errors.New("a text adapter was handed a session")
}
func (textAdapter) Stream(context.Context, *provider.Call, provider.Emitter, *provider.KeyPool) (provider.Outcome, error) {
	return provider.Outcome{}, errors.New("a text adapter was handed a session")
}
func (a textAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "openai"}
}
func (a textAdapter) PlatformCredentials() *provider.KeyPool { return a.pool }

// costSink is the operator cost store: every attempt Kaana would persist.
type costSink struct {
	mu     sync.Mutex
	events []providercost.Event
}

func (c *costSink) WriteProviderCostEvent(_ context.Context, event providercost.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return nil
}

func (c *costSink) recorded() []providercost.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]providercost.Event(nil), c.events...)
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type harness struct {
	t        *testing.T
	upstream *fake.Upstream
	server   *httptest.Server
	manager  *realtime.Manager
	adapter  *openairealtime.Adapter
	private  ed25519.PrivateKey
	keyID    string
	costs    *costSink
	logs     *lockedBuffer
}

type options struct {
	// xai serves the deployments from xAI's Voice Agent adapter instead of
	// OpenAI's, on its own fake.
	xai          bool
	resumeWindow time.Duration
	replayEvents int
	pingInterval time.Duration
}

func newHarness(t *testing.T, opts options) *harness {
	t.Helper()
	upstream, slug, upstreamModel := fake.New(t), contract.ProviderSlug("openai-realtime"), "gpt-realtime-2.1"
	build, keys := openairealtime.New, []string{fake.Key, fake.SecondKey}
	rates := `{"unit":"input_tokens","amountPerUnit":4000000},{"unit":"cached_input_tokens","amountPerUnit":400000},{"unit":"audio_input_tokens","amountPerUnit":32000000},{"unit":"cached_audio_input_tokens","amountPerUnit":400000},{"unit":"output_tokens","amountPerUnit":24000000},{"unit":"audio_output_tokens","amountPerUnit":64000000}`
	if opts.xai {
		upstream, slug, upstreamModel = fake.NewXAI(t), openairealtime.XAISlug, "grok-voice-think-fast-2.0"
		build, keys = openairealtime.NewXAI, []string{fake.XAIKey, fake.XAISecondKey}
		rates = `{"unit":"audio_input_milliseconds","amountPerUnit":2},{"unit":"audio_output_milliseconds","amountPerUnit":3},{"unit":"requests","amountPerUnit":5}`
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "edge-test"
	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{keyID: public}, 0)
	if err != nil {
		t.Fatal(err)
	}

	deployments := []map[string]any{
		{"deploymentId": primary, "provider": slug, "modelReference": modelReference, "upstreamModelId": upstreamModel, "current": true},
		{"deploymentId": secondary, "provider": slug, "modelReference": modelReference, "upstreamModelId": upstreamModel, "current": true},
		{"deploymentId": textRoute, "provider": "openai", "modelReference": modelReference, "upstreamModelId": "gpt-realtime-2.1", "current": true},
	}
	document, _ := json.Marshal(map[string]any{"snapshotId": "snap_realtime", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": deployments})
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	store, err := inventory.NewStore(inventory.Config{Path: path, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}

	adapter, err := build(openairealtime.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "key_a", Secret: keys[0]}, {KeyID: "key_b", Secret: keys[1]}},
		HTTPClient:   upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	textPool, err := provider.NewKeyPool("openai", provider.DeclareKeys([]string{"sk-text-synthetic-not-valid"}), provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(adapter, textAdapter{pool: textPool})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration([]provider.CredentialBinding{
		{DeploymentID: primary, Provider: slug, KeyID: "key_a"},
		{DeploymentID: secondary, Provider: slug, KeyID: "key_b"},
	}, adapter, textAdapter{pool: textPool}); err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"rc_realtime_v1","source":"operator","sourceVersion":"test","observedAt":"2026-01-01T00:00:00Z","effectiveAt":"2026-01-01T00:00:00Z","rateCards":[
		{"deploymentId":"dep_realtime_a","currency":"USD","rates":[` + rates + `]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	costs := &costSink{}
	recorder, err := providercost.NewRecorder(costs)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil),
		Costs: cards, CostRecorder: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := realtime.NewManager(realtime.Config{Opener: executor, Verifier: verifier, Logger: logger, ResumeWindow: opts.resumeWindow, ReplayEvents: opts.replayEvents, PingInterval: opts.pingInterval})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /internal/v1/realtime", manager)
	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
		server.Close()
	})
	return &harness{t: t, upstream: upstream, server: server, manager: manager, adapter: adapter, private: private, keyID: keyID, costs: costs, logs: logs}
}

func pointer[T any](value T) *T { return &value }

func route(deployment contract.DeploymentID) contract.AuthorizedRoute {
	slug := contract.ProviderSlug("openai-realtime")
	if deployment == textRoute {
		slug = "openai"
	}
	return contract.AuthorizedRoute{Substitution: contract.SubstitutionSameModel, DeploymentID: deployment, ModelReference: modelReference, Provider: slug, Regions: []contract.Region{}}
}

func defaultLimits() contract.RealtimeSessionLimits {
	return contract.RealtimeSessionLimits{MaxDurationMs: 600_000, IdleTimeoutMs: 60_000, MaxInputAudioBytes: 1 << 20, MaxOutputAudioBytes: 1 << 20, MaxResponses: 10}
}

func sessionRequest(requestID contract.RequestID, deployments ...contract.DeploymentID) contract.RealtimeSessionRequest {
	routes := make([]contract.AuthorizedRoute, 0, len(deployments))
	for _, deployment := range deployments {
		routes = append(routes, route(deployment))
	}
	return contract.RealtimeSessionRequest{
		SchemaVersion: 1,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing: contract.BillingPrincipal{AccountID: "acc_realtime"}, ApplicationID: "app_realtime", CredentialID: "cred_realtime",
				Environment: contract.EnvironmentProduction, InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: requestID,
		},
		ModelReference: "openai/gpt-realtime-2.1",
		Kind:           contract.RealtimeConversation,
		Transport:      contract.RealtimeWebSocket,
		Config: contract.RealtimeSessionConfig{
			Instructions: pointer("be brief"), OutputModalities: []contract.RealtimeOutputModality{contract.RealtimeOutputAudio},
			Voice: pointer("marin"), InputAudioFormat: contract.RealtimePCM16, OutputAudioFormat: pointer(contract.RealtimePCM16),
			TurnDetection: contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone},
		},
		Limits:           defaultLimits(),
		Client:           contract.RealtimeClientMetadata{Endpoint: "/v1/realtime", ReceivedAt: contract.NewTimestamp(time.Now())},
		RoutingPolicy:    contract.RoutingPolicyReference{RoutingPolicyID: "rp_realtime", PolicyVersion: 1},
		AuthorizedRoutes: routes,
	}
}

func encode(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// signingInput lets a test sign under the wrong purpose.
type signingInput func(keyID string, milliseconds int64, body []byte) []byte

// connect upgrades, signing the first frame's exact bytes, and sends it.
func (h *harness) connect(first []byte, sign signingInput, key ed25519.PrivateKey) *websocket.Conn {
	h.t.Helper()
	if sign == nil {
		sign = edgeauth.SigningInput
	}
	if key == nil {
		key = h.private
	}
	milliseconds := time.Now().UnixMilli()
	header := http.Header{}
	header.Set(edgeauth.HeaderKeyID, h.keyID)
	header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(key, sign(h.keyID, milliseconds, first))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", &websocket.DialOptions{HTTPHeader: header})
	closeBody(response)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(32 << 20)
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, first); err != nil {
		h.t.Fatalf("first frame: %v", err)
	}
	return ws
}

// closeBody releases a handshake response's body when it has one; a completed
// upgrade hands the connection to the WebSocket instead.
func closeBody(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func (h *harness) open(request contract.RealtimeSessionRequest) *websocket.Conn {
	h.t.Helper()
	return h.connect(encode(h.t, request), nil, nil)
}

type frame map[string]any

func (f frame) kind() string {
	value, _ := f["type"].(string)
	return value
}

func (f frame) sequence() int {
	value, ok := f["sequence"].(float64)
	if !ok {
		return -1
	}
	return int(value)
}

func read(t *testing.T, ws *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kind, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.MessageText {
		t.Fatal("Kaana sent a binary frame")
	}
	var decoded frame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Kaana sent invalid JSON: %v", err)
	}
	return decoded
}

// readUntil reads frames up to and including the first of the given type.
func readUntil(t *testing.T, ws *websocket.Conn, eventType string) []frame {
	t.Helper()
	var frames []frame
	for {
		next := read(t, ws)
		frames = append(frames, next)
		if next.kind() == eventType {
			return frames
		}
	}
}

// expectClose requires the connection to end with a close code and no other
// frame.
func expectClose(t *testing.T, ws *websocket.Conn, code websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err == nil {
		t.Fatalf("expected close %d, read a frame: %s", code, data)
	}
	if status := websocket.CloseStatus(err); status != code {
		t.Fatalf("closed with %d (%v), want %d", status, err, code)
	}
}

// encodeCommand is a command as the edge writes it: its discriminator set.
func encodeCommand(t *testing.T, value contract.RealtimeCommand) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(encode(t, value), &fields); err != nil {
		t.Fatal(err)
	}
	fields["type"] = string(value.CommandType())
	return encode(t, fields)
}

func command(t *testing.T, ws *websocket.Conn, value contract.RealtimeCommand) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, encodeCommand(t, value)); err != nil {
		t.Fatalf("writing %s: %v", value.CommandType(), err)
	}
}

func appendAudio(requestID contract.RequestID, commandID contract.RealtimeCommandID, data string) *contract.RealtimeInputAudioAppendCommand {
	return &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: commandID, Data: data}
}

func closeCommand(requestID contract.RequestID, commandID contract.RealtimeCommandID) *contract.RealtimeSessionCloseCommand {
	return &contract.RealtimeSessionCloseCommand{SchemaVersion: 1, RequestID: requestID, CommandID: commandID}
}

func resumeCommand(requestID contract.RequestID, commandID contract.RealtimeCommandID, after int) *contract.RealtimeSessionResumeCommand {
	return &contract.RealtimeSessionResumeCommand{SchemaVersion: 1, RequestID: requestID, CommandID: commandID, AfterSequence: after}
}

// requireContiguous asserts the frames carry one sequence each, from start,
// with no gap and no repeat.
func requireContiguous(t *testing.T, frames []frame, start int) {
	t.Helper()
	for index, event := range frames {
		if event.sequence() != start+index {
			t.Fatalf("frame %d (%s) has sequence %d, want %d", index, event.kind(), event.sequence(), start+index)
		}
	}
}

// waitFor polls a condition the session reaches asynchronously.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unitsOf(value any) map[string]int {
	units := make(map[string]int)
	list, _ := value.([]any)
	for _, entry := range list {
		quantity := entry.(map[string]any)
		units[quantity["unit"].(string)] = int(quantity["quantity"].(float64))
	}
	return units
}

func describe(frames []frame) string {
	kinds := make([]string, 0, len(frames))
	for _, event := range frames {
		kinds = append(kinds, fmt.Sprintf("%d:%s", event.sequence(), event.kind()))
	}
	return strings.Join(kinds, " ")
}
