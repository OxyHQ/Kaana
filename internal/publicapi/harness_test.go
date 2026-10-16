package publicapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openairealtime"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/publicapi"
	"github.com/OxyHQ/Kaana/internal/realtime"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// The credentials the fake Oxy knows. None is a credential.
const (
	validKey   = "oxy_sk_test_valid_synthetic"
	revokedKey = "oxy_sk_test_revoked_synthetic"
	brokeKey   = "oxy_sk_test_broke_synthetic"
	otherKey   = "oxy_sk_test_other_synthetic"
	kaanaToken = "kaana-service-token-synthetic"
	oxyKeyID   = "oxy-grant-test"
)

// chargedRecord is the generation record Oxy answers receipt.read with.
const chargedRecord = `{"schemaVersion":1,"kind":"charge","requestId":"req_1","generationId":"gen_1","applicationId":"app_1","credentialId":"cred_1",
"environment":"production","outcome":"completed","usageSource":"provider_reported","units":[{"unit":"output_tokens","quantity":3}],
"model":"stub/model@2026-05-01","settledAt":"2026-10-15T10:00:00.000Z","receiptId":"rcpt_1",
"priceSnapshot":{"priceVersionId":"price-1","currency":"USD","unitPrices":[{"unit":"output_tokens","amount":"0.000002","per":1,"currency":"USD"}]},
"billedAmount":"0.000006","currency":"USD","platformFeeOnly":false}`

// realtimeObserved is what the realtime deployment publishes: a conversation
// over WebSocket, input transcription, one voice, no language list (OpenAI
// publishes none, so a session names none and the model detects it), and the
// parameters its session serves, as its adapter states them: OpenAI's, or the
// statement harnessOptions.realtime names.
func realtimeObserved(t *testing.T, statement providerconfig.RealtimeStatement) json.RawMessage {
	t.Helper()
	parameters, err := json.Marshal(statement.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(`{"realtime":{"transports":["websocket"],"sessionKinds":["conversation","transcription"],"inputAudioTranscription":true,
  "audioFormats":{"input":["g711_alaw","g711_ulaw","pcm16_24khz"],"output":["g711_alaw","g711_ulaw","pcm16_24khz"]},"parameters":` + string(parameters) + `},
  "audio":{"languages":[],"voices":[{"id":"marin","name":"Marin","languages":[]}]}}`)
}

// whisperObserved is what the transcription deployment publishes: the uploads
// it takes and two languages.
const whisperObserved = `{"audio":{"voices":[],"languages":["en","es-ES"],
  "inputMediaTypes":["audio/mp4","audio/wav","audio/webm"]}}`

const (
	textModel     = contract.ModelReference("stub/model@2026-05-01")
	fallbackModel = contract.ModelReference("stub/fallback@2026-05-01")
	speechModel   = contract.ModelReference("stub/voice@v1")
	whisperModel  = contract.ModelReference("stub/whisper@v1")
	realtimeModel = contract.ModelReference("openai/gpt-realtime-2.1@observed-2026-09-30")
)

/* -------------------------------------------------------------------------- */
/*  A provider the test scripts                                                */
/* -------------------------------------------------------------------------- */

// stubAdapter serves text, speech and transcription deterministically. It
// records every call so a test can prove a provider was, or was never,
// reached.
type stubAdapter struct {
	pool  *provider.KeyPool
	mu    sync.Mutex
	calls []*contract.Request
	// failAfter, when positive, fails the attempt after that many chunks.
	failAfter int
	// failDeployment fails before any output on that deployment only.
	failDeployment contract.DeploymentID
	chunks         int
}

func (*stubAdapter) Provider() contract.ProviderSlug { return "stub" }

func (s *stubAdapter) PlatformCredentials() *provider.KeyPool { return s.pool }
func (*stubAdapter) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatResponses, contract.APIFormatChatCompletions,
		contract.APIFormatAudioSpeech, contract.APIFormatAudioTranscriptions, contract.APIFormatEmbeddings}
}
func (s *stubAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	s.mu.Lock()
	copied := *request
	s.calls = append(s.calls, &copied)
	s.mu.Unlock()
	return &provider.Call{Route: route, Method: http.MethodPost, URL: "stub://" + string(route.DeploymentID), Stream: request.Stream}, nil
}

func (s *stubAdapter) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubAdapter) lastCall() *contract.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

var errUpstream = provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "stub upstream failed mid-stream"}

func (s *stubAdapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, _ *provider.KeyPool) (provider.Outcome, error) {
	if call.Route.DeploymentID == s.failDeployment {
		return provider.Outcome{}, provider.ErrUpstream{Code: contract.CodeProviderOverloaded, Category: contract.UpstreamOverloaded, Detail: "stub is overloaded"}
	}
	s.mu.Lock()
	request := s.calls[len(s.calls)-1]
	failAfter, chunks := s.failAfter, max(s.chunks, 3)
	s.mu.Unlock()
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	switch request.Client.APIFormat {
	case contract.APIFormatAudioSpeech:
		audio := out.(provider.AudioEmitter)
		for index := range chunks {
			if failAfter > 0 && index == failAfter {
				return outcome, errUpstream
			}
			if err := audio.Audio(0, "audio/mpeg", []byte("ID3-chunk-"+strconv.Itoa(index))); err != nil {
				return outcome, err
			}
			outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: len(*request.Input.Text)}}
		}
	case contract.APIFormatAudioTranscriptions:
		if err := out.Delta(0, contract.ChannelOutputText, "hola "); err != nil {
			return outcome, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "mundo"); err != nil {
			return outcome, err
		}
		outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: 1000}}
	default:
		for index := range chunks {
			if failAfter > 0 && index == failAfter {
				return outcome, errUpstream
			}
			if err := out.Delta(0, contract.ChannelOutputText, "chunk "+strconv.Itoa(index)+" "); err != nil {
				return outcome, err
			}
			outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 7}, {Unit: contract.UnitOutputTokens, Quantity: index + 1}}
		}
		if len(request.Tools) > 0 {
			if err := out.ToolCall(provider.ToolCallDelta{ID: "call_1", Name: request.Tools[0].Name, ArgumentsDelta: `{"q":"x"}`}); err != nil {
				return outcome, err
			}
			if err := out.ToolCall(provider.ToolCallDelta{ID: "call_1", Complete: true}); err != nil {
				return outcome, err
			}
			outcome.FinishReason = contract.FinishToolCalls
		}
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if err := out.Usage(outcome.Units, outcome.UsageSource); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (*stubAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "stub", Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

/* -------------------------------------------------------------------------- */
/*  Oxy, as Kaana sees it                                                      */
/* -------------------------------------------------------------------------- */

// fakeOxy is Oxy's internal API with the contract's shapes: it authorizes
// with a signed grant and settles idempotently on the grant. It records what
// Kaana sent, so a test can prove no content left Kaana.
type fakeOxy struct {
	t         *testing.T
	server    *httptest.Server
	key       ed25519.PrivateKey
	settlePub ed25519.PublicKey

	mu             sync.Mutex
	authorizations []json.RawMessage
	settlements    map[string]contract.Settlement // by grant
	deliveries     int
	down           bool
	// loseAcknowledgements settles but answers 503 that many times: the
	// shape of an acknowledgement lost on the way back.
	loseAcknowledgements int
	// mutate edits a grant before it is signed.
	mutate func(*contract.Grant)
	// steps overrides the steps of every grant.
	steps  func(request contract.InferenceAuthorizationRequest) []contract.GrantStep
	grants int
	// audience and allowedLevels answer catalogue.read.
	audience      contract.CatalogueAudience
	allowedLevels []contract.PowerLevel
}

func newFakeOxy(t *testing.T, settlementKey ed25519.PublicKey) *fakeOxy {
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	oxy := &fakeOxy{t: t, key: private, settlePub: settlementKey, settlements: make(map[string]contract.Settlement)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/kaana/v1/authorizations", oxy.authorize)
	mux.HandleFunc("POST /internal/kaana/v1/settlements", oxy.settle)
	oxy.server = httptest.NewServer(mux)
	t.Cleanup(oxy.server.Close)
	return oxy
}

func (o *fakeOxy) publicKey() ed25519.PublicKey { return o.key.Public().(ed25519.PublicKey) }

func refuse(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 1, "code": code, "message": "Oxy refused: account acct_secret_detail", "retryable": false, "requestId": "req_oxy"})
}

func (o *fakeOxy) authorize(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	down := o.down
	o.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+kaanaToken {
		refuse(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.authorizations = append(o.authorizations, json.RawMessage(body))
	o.mu.Unlock()
	var probe struct {
		Kind       string `json:"kind"`
		Credential string `json:"credential"`
	}
	_ = json.Unmarshal(body, &probe)
	switch probe.Credential {
	case validKey, otherKey:
	case brokeKey:
		refuse(w, http.StatusPaymentRequired, "insufficient_balance")
		return
	default:
		refuse(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	switch probe.Kind {
	case contract.AuthorizationKindCatalogue:
		o.mu.Lock()
		audience := o.audience
		o.mu.Unlock()
		if audience == "" {
			audience = contract.AudiencePublic
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.CatalogueAccess{SchemaVersion: 1, Kind: contract.AuthorizationKindCatalogue,
			Audience: audience, AllowedLevels: o.allowedLevels, ExpiresAt: contract.NewTimestamp(time.Now().Add(time.Minute))})
		return
	case contract.AuthorizationKindReceipt:
		var request contract.ReceiptAuthorizationRequest
		_ = json.Unmarshal(body, &request)
		if request.GenerationID != nil && *request.GenerationID == "gen_unknown" {
			refuse(w, http.StatusNotFound, "model_not_found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schemaVersion":1,"kind":"receipt.read","record":` + chargedRecord + `}`))
		return
	case contract.AuthorizationKindInference:
	default:
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	var request contract.InferenceAuthorizationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		o.t.Errorf("Kaana sent an authorization outside the contract: %v", err)
		refuse(w, http.StatusBadRequest, "invalid_request")
		return
	}
	now := time.Now().UTC()
	o.mu.Lock()
	o.grants++
	number := o.grants
	steps := o.steps
	mutate := o.mutate
	o.mu.Unlock()
	reservation := "rsv_" + strconv.Itoa(number)
	grant := contract.Grant{
		SchemaVersion: contract.GrantSchemaVersion, GrantID: "grt_" + strconv.Itoa(number), RequestID: request.RequestID,
		Operation: request.Operation, IssuedAt: contract.NewTimestamp(now), ExecuteBy: contract.NewTimestamp(now.Add(60 * time.Second)),
		BodySHA256: request.BodySHA256,
		Principal: contract.GrantPrincipal{BillingAccountID: "acct_1", ApplicationID: "app_1", CredentialID: "cred_1",
			Environment: contract.EnvironmentProduction, Scopes: []contract.Scope{contract.ScopeInvoke}, Lane: contract.LaneMachineCredential},
		UserID:         request.OnBehalfOfUserID,
		Economics:      contract.GrantEconomics{Treatment: contract.TreatmentCommercial, PolicyVersion: "pol-1"},
		Reservation:    &contract.GrantReservation{ReservationID: reservation, Ceiling: contract.Money{Amount: "0.50", Currency: "USD"}},
		IdempotencyKey: request.IdempotencyKey,
		Client:         contract.GrantClient{Labels: request.Client.Labels},
		RoutingPolicy:  contract.RoutingPolicyReference{RoutingPolicyID: "rp_1", PolicyVersion: 2},
	}
	if steps != nil {
		grant.Steps = steps(request)
	} else if request.Target != nil && request.Target.Kind == contract.GrantTargetModel {
		grant.Steps = []contract.GrantStep{{Target: *request.Target, Limits: contract.GrantStepLimits{
			AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: *request.Target.ModelLine, PriceVersionID: "price-1"}},
		}}}
	} else {
		line := textModel.ModelID()
		grant.Steps = []contract.GrantStep{{Target: contract.GrantTarget{Kind: contract.GrantTargetModel, ModelLine: &line},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: line, PriceVersionID: "price-1"}}}}}
	}
	if mutate != nil {
		mutate(&grant)
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		o.t.Fatal(err)
	}
	milliseconds := time.Now().UnixMilli()
	w.Header().Set(edgeauth.HeaderKeyID, oxyKeyID)
	w.Header().Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	w.Header().Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(o.key, edgeauth.GrantSigningInput(oxyKeyID, milliseconds, encoded))))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

func (o *fakeOxy) settle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deliveries++
	if r.Header.Get("Authorization") != "Bearer "+kaanaToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	milliseconds, _ := strconv.ParseInt(r.Header.Get(edgeauth.HeaderTimestamp), 10, 64)
	signature, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get(edgeauth.HeaderSignature), "v1="))
	if !ed25519.Verify(o.settlePub, edgeauth.SettlementSigningInput(r.Header.Get(edgeauth.HeaderKeyID), milliseconds, body), signature) {
		o.t.Error("a settlement arrived without Kaana's signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var settlement contract.Settlement
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settlement); err != nil || settlement.Validate() != nil {
		o.t.Errorf("a settlement outside the contract: %v / %v", err, settlement.Validate())
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	status := contract.SettlementSettled
	if _, seen := o.settlements[settlement.GrantID]; seen {
		status = contract.SettlementDuplicate
	} else {
		o.settlements[settlement.GrantID] = settlement
	}
	if o.loseAcknowledgements > 0 {
		o.loseAcknowledgements--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(contract.SettlementAcknowledgement{SchemaVersion: 1, SettlementID: settlement.SettlementID, Status: status})
}

func (o *fakeOxy) authorizationBodies() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	bodies := make([]string, len(o.authorizations))
	for index, body := range o.authorizations {
		bodies[index] = string(body)
	}
	return bodies
}

func (o *fakeOxy) settled() map[string]contract.Settlement {
	o.mu.Lock()
	defer o.mu.Unlock()
	copied := make(map[string]contract.Settlement, len(o.settlements))
	for grant, settlement := range o.settlements {
		copied[grant] = settlement
	}
	return copied
}

/* -------------------------------------------------------------------------- */
/*  The outbox, in memory with the SQL's semantics                            */
/* -------------------------------------------------------------------------- */

type outboxRow struct {
	row       credentialstore.OutboxSettlement
	position  int64
	attempts  int
	due       time.Time
	delivered string
	events    []providercost.Event
}

type memoryOutbox struct {
	mu   sync.Mutex
	rows []*outboxRow
	// failWrites fails that many RecordSettlement calls.
	failWrites int
}

func (m *memoryOutbox) RecordSettlement(_ context.Context, events []providercost.Event, settlement credentialstore.OutboxSettlement) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrites > 0 {
		m.failWrites--
		return errors.New("outbox unavailable")
	}
	for _, existing := range m.rows {
		if existing.row.GrantID == settlement.GrantID || existing.row.SettlementID == settlement.SettlementID {
			if bytes.Equal(existing.row.Body, settlement.Body) {
				return nil
			}
			return credentialstore.ErrSettlementConflict
		}
	}
	m.rows = append(m.rows, &outboxRow{row: settlement, position: int64(len(m.rows) + 1), due: time.Now(), events: events})
	return nil
}

func (m *memoryOutbox) ClaimSettlements(_ context.Context, limit int, lease time.Duration) ([]credentialstore.ClaimedSettlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var claimed []credentialstore.ClaimedSettlement
	for _, row := range m.rows {
		if len(claimed) == limit {
			break
		}
		if row.delivered == "" && !row.due.After(time.Now()) {
			row.attempts++
			row.due = time.Now().Add(lease)
			claimed = append(claimed, credentialstore.ClaimedSettlement{SettlementID: row.row.SettlementID, Body: row.row.Body, Attempts: row.attempts})
		}
	}
	return claimed, nil
}

func (m *memoryOutbox) MarkSettlementDelivered(_ context.Context, settlementID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.row.SettlementID == settlementID && row.delivered == "" {
			row.delivered = status
		}
	}
	return nil
}

func (m *memoryOutbox) DeferSettlement(_ context.Context, settlementID string, retryAt time.Time, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.row.SettlementID == settlementID && row.delivered == "" {
			row.due = retryAt
		}
	}
	return nil
}

func (m *memoryOutbox) ReadSettlements(_ context.Context, after int64, limit int) ([]credentialstore.PositionedSettlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var page []credentialstore.PositionedSettlement
	for _, row := range m.rows {
		if row.position > after && len(page) < limit {
			page = append(page, credentialstore.PositionedSettlement{Position: row.position, Body: row.row.Body})
		}
	}
	return page, nil
}

// makeDue lets every deferred row be claimed now.
func (m *memoryOutbox) makeDue() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		row.due = time.Now().Add(-time.Second)
	}
}

func (m *memoryOutbox) snapshot() []outboxRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]outboxRow, len(m.rows))
	for index, row := range m.rows {
		rows[index] = *row
	}
	return rows
}

/* -------------------------------------------------------------------------- */
/*  Kaana                                                                     */
/* -------------------------------------------------------------------------- */

type harness struct {
	t        *testing.T
	oxy      *fakeOxy
	outbox   *memoryOutbox
	adapter  *stubAdapter
	upstream *fake.Upstream
	server   *httptest.Server
	api      *publicapi.Server
	settler  *publicapi.Settler
	manager  *realtime.Manager
	logs     *lockedBuffer
	signer   *edgeauth.Signer
}

type harnessOptions struct {
	catalogue *publicapi.PublicCatalogue
	audio     publicapi.AudioCatalogue
	rate      publicapi.RateLimit
	origins   []string
	store     publicapi.SettlementStore
	authorize func(publicapi.Authorizer) publicapi.Authorizer
	// realtime is the statement the realtime deployment publishes; OpenAI's
	// when unset.
	realtime *providerconfig.RealtimeStatement
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

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	seed := sha256.Sum256([]byte(t.Name()))
	signer, err := edgeauth.NewSettlementSigner("kaana-settlement-test:" + base64.StdEncoding.EncodeToString(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	oxy := newFakeOxy(t, signer.PublicKey())
	upstream := fake.New(t)
	realtimeStatement, _ := providerconfig.RealtimeCapabilities(openairealtime.Slug, providerconfig.ProtocolOpenAIRealtime)
	if options.realtime != nil {
		realtimeStatement = *options.realtime
	}

	deployments := []map[string]any{
		{"deploymentId": "dep_stub_a", "provider": "stub", "modelReference": textModel, "upstreamModelId": "stub-a", "regions": []string{"us"}, "current": true},
		{"deploymentId": "dep_stub_b", "provider": "stub", "modelReference": textModel, "upstreamModelId": "stub-b", "regions": []string{"eu"}, "current": true},
		{"deploymentId": "dep_fallback", "provider": "stub", "modelReference": fallbackModel, "upstreamModelId": "stub-fallback", "current": true},
		{"deploymentId": "dep_speech", "provider": "stub", "modelReference": speechModel, "upstreamModelId": "stub-voice", "current": true},
		{"deploymentId": "dep_whisper", "provider": "stub", "modelReference": whisperModel, "upstreamModelId": "stub-whisper", "current": true,
			"observed": json.RawMessage(whisperObserved)},
		{"deploymentId": "dep_realtime", "provider": "openai-realtime", "modelReference": realtimeModel, "upstreamModelId": "gpt-realtime-2.1", "current": true,
			"observed": realtimeObserved(t, realtimeStatement)},
	}
	document, _ := json.Marshal(map[string]any{"snapshotId": "snap_public", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": deployments})
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
	realtimeAdapter, err := openairealtime.New(openairealtime.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "key_rt", Secret: fake.Key}},
		HTTPClient:   upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The stub's pool: a free key and a paid one. dep_stub_a is pinned to the
	// paid key and dep_stub_b to the free one, so the cheapest-first order is
	// the reverse of the inventory's.
	pool, err := provider.NewKeyPool("stub", []provider.KeyDeclaration{
		{KeyID: "key_paid", Secret: "stub-paid-synthetic", Funding: provider.FundingPaid},
		{KeyID: "key_free", Secret: "stub-free-synthetic", Funding: provider.FundingFree},
	}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &stubAdapter{pool: pool}
	registry, err := provider.NewRegistry(adapter, realtimeAdapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration([]provider.CredentialBinding{
		{DeploymentID: "dep_realtime", Provider: "openai-realtime", KeyID: "key_rt"},
		{DeploymentID: "dep_stub_a", Provider: "stub", KeyID: "key_paid"},
		{DeploymentID: "dep_stub_b", Provider: "stub", KeyID: "key_free"},
		{DeploymentID: "dep_fallback", Provider: "stub", KeyID: "key_free"},
		{DeploymentID: "dep_speech", Provider: "stub", KeyID: "key_paid"},
		{DeploymentID: "dep_whisper", Provider: "stub", KeyID: "key_paid"},
	}, adapter, realtimeAdapter); err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"rc_public_v1","source":"operator","sourceVersion":"test","observedAt":"2026-01-01T00:00:00Z","effectiveAt":"2026-01-01T00:00:00Z","rateCards":[
		{"deploymentId":"dep_realtime","currency":"USD","rates":[{"unit":"output_tokens","amountPerUnit":24000000}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// A recorder that fails the test if the executor records a public
	// request's cost itself: the settlement records it, once, atomically.
	recorder, err := providercost.NewRecorder(failingWriter{t: t})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil),
		Costs: cards, CostRecorder: recorder, Retry: &kaana.RetryPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{oxyKeyID: oxy.publicKey()}
	grants, err := edgeauth.NewGrantVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token := func(context.Context) (string, error) { return kaanaToken, nil }
	var authorizer publicapi.Authorizer
	authorizer, err = publicapi.NewOxyAuthorizer(publicapi.OxyAuthorizerConfig{
		URL: oxy.server.URL + "/internal/kaana/v1/authorizations", Token: token, Grants: grants, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.authorize != nil {
		authorizer = options.authorize(authorizer)
	}
	outbox := &memoryOutbox{}
	var settlementStore publicapi.SettlementStore = outbox
	if options.store != nil {
		settlementStore = options.store
	}
	settler, err := publicapi.NewSettler(publicapi.SettlerConfig{
		Store: settlementStore, URL: oxy.server.URL + "/internal/kaana/v1/settlements", Token: token, Signer: signer, Logger: logger,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	api, err := publicapi.New(publicapi.Config{
		Executor: executor, Authorizer: authorizer, Settler: settler, Inventory: store,
		Funding: publicapi.RegistryFunding{Registry: registry}, Audio: options.audio, Catalogue: options.catalogue,
		AllowedOrigins: options.origins, RateLimit: options.rate, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	edgeVerifier, err := edgeauth.NewVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := realtime.NewManager(realtime.Config{Opener: executor, Verifier: edgeVerifier, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	api.UseRealtime(manager.PublicHandler(api.RealtimeGate()))
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
		server.Close()
	})
	return &harness{t: t, oxy: oxy, outbox: outbox, adapter: adapter, upstream: upstream, server: server, api: api,
		settler: settler, manager: manager, logs: logs, signer: signer}
}

type failingWriter struct{ t *testing.T }

func (w failingWriter) WriteProviderCostEvent(context.Context, providercost.Event) error {
	w.t.Error("the executor recorded a public request's cost itself; the settlement must, atomically")
	return nil
}

// reply is a whole response, read to its end: a test asserts on what the
// client received, and a body cut short is ReadErr.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	ReadErr    error
}

// post sends a JSON request with a credential.
func (h *harness) post(path, credential string, body any, headers ...string) *reply {
	h.t.Helper()
	encoded, ok := body.([]byte)
	if !ok {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
	}
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Set(headers[index], headers[index+1])
	}
	return h.do(request)
}

// do sends a request and reads the whole response.
func (h *harness) do(request *http.Request) *reply {
	h.t.Helper()
	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(response.Body)
	return &reply{StatusCode: response.StatusCode, Header: response.Header, Body: body, ReadErr: readErr}
}

// settle drains the settlement writes and delivers every due settlement.
func (h *harness) settle() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.api.Drain(ctx); err != nil {
		h.t.Fatal(err)
	}
	h.settler.DeliverDue(ctx)
}

func readAll(t *testing.T, response *reply) []byte {
	t.Helper()
	if response.ReadErr != nil {
		t.Fatalf("the response was cut short: %v", response.ReadErr)
	}
	return response.Body
}

func publicError(t *testing.T, response *reply, status int) contract.PublicError {
	t.Helper()
	body := readAll(t, response)
	if response.StatusCode != status {
		t.Fatalf("status %d, want %d: %s", response.StatusCode, status, body)
	}
	var failure contract.PublicError
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("the error body is not a public error: %s", body)
	}
	if response.Header.Get(publicapi.HeaderRequestID) != string(failure.RequestID) || failure.RequestID == "" {
		t.Errorf("the error names request %q, the header %q", failure.RequestID, response.Header.Get(publicapi.HeaderRequestID))
	}
	return failure
}

// forbiddenOnPublicSurface are strings a public response must never contain:
// providers, deployments, keys and the operator.
var forbiddenOnPublicSurface = []string{"dep_", "deploymentId", "servingProvider", "openai-realtime", "\"provider\"", "key_rt", "acct_secret_detail", "providerError"}

func assertNoProviderLeak(t *testing.T, where string, body []byte) {
	t.Helper()
	for _, forbidden := range forbiddenOnPublicSurface {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Errorf("%s leaks %q: %s", where, forbidden, body)
		}
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// reconciliation serves GET /internal/v1/settlements over the outbox, read
// with a query signed (or not) by the fake Oxy's key.
func (h *harness) reconciliation() func(query string, sign bool) *reply {
	verifier, err := edgeauth.NewSettlementReadVerifier(map[string]ed25519.PublicKey{oxyKeyID: h.oxy.publicKey()}, time.Minute)
	if err != nil {
		h.t.Fatal(err)
	}
	server := httptest.NewServer(publicapi.ReconciliationHandler{Store: h.outbox, Verifier: verifier})
	h.t.Cleanup(server.Close)
	return func(query string, sign bool) *reply {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/internal/v1/settlements?"+query, nil)
		if sign {
			milliseconds := time.Now().UnixMilli()
			request.Header.Set(edgeauth.HeaderKeyID, oxyKeyID)
			request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
			request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(
				ed25519.Sign(h.oxy.key, edgeauth.SettlementReadSigningInput(oxyKeyID, milliseconds, []byte(query)))))
		}
		response, err := server.Client().Do(request)
		if err != nil {
			h.t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, readErr := io.ReadAll(response.Body)
		return &reply{StatusCode: response.StatusCode, Header: response.Header, Body: body, ReadErr: readErr}
	}
}
