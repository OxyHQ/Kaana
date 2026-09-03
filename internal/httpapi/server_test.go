package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/httpapi"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
	"github.com/OxyHQ/Kaana/internal/sse"
)

// The cancellation proof is split across two suites on purpose, and each half
// tests one link of the chain:
//
//   - here: a client that disconnects cancels the context the EXECUTOR runs
//     under, observed by the adapter itself over a real TCP connection;
//   - in the conformance suite: an adapter running under a cancelled context
//     tears down its UPSTREAM connection, observed by the upstream server.
//
// Neither half is sufficient alone. The first could pass with an adapter that
// ignores its context; the second could pass with an HTTP layer that never
// cancels anything.

/* -------------------------------------------------------------------------- */
/*  Harness                                                                   */
/* -------------------------------------------------------------------------- */

// stubAdapter is a provider whose behaviour the test scripts directly. It is
// not a stand-in for the OpenAI adapter — that one is covered by the
// conformance suite against a real wire-format upstream. Here the subject is
// the transport, so the adapter is reduced to the two things the transport
// interacts with: it emits events, and it observes cancellation.
type stubAdapter struct {
	chunks   int
	interval time.Duration
	refuse   error
	// fail is returned by Stream before anything is emitted or measured: the
	// shape of a provider that rejects the call outright, such as a 402 from an
	// account with no balance.
	fail error

	mutex     sync.Mutex
	written   int
	cancelled bool
	calls     int
}

func (s *stubAdapter) Provider() contract.ProviderSlug { return "stub" }

func (s *stubAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	s.mutex.Lock()
	s.calls++
	s.mutex.Unlock()
	if s.refuse != nil {
		return nil, s.refuse
	}
	return &provider.Call{Route: route, Method: http.MethodPost, URL: "stub://call", Stream: request.Stream}, nil
}

func (s *stubAdapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, _ *provider.KeyPool) (provider.Outcome, error) {
	if s.fail != nil {
		// Nothing started and nothing was measured, so the outcome is its zero
		// value — including a nil unit slice, which is the point.
		return provider.Outcome{}, s.fail
	}
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	for index := range s.chunks {
		if err := out.Delta(0, contract.ChannelOutputText, "chunk "+strconv.Itoa(index)+" "); err != nil {
			return outcome, err
		}
		s.mutex.Lock()
		s.written++
		s.mutex.Unlock()

		outcome.Units = []contract.UsageQuantity{
			{Unit: contract.UnitRequests, Quantity: 1},
			{Unit: contract.UnitOutputTokens, Quantity: index + 1},
		}
		select {
		case <-ctx.Done():
			s.mutex.Lock()
			s.cancelled = true
			s.mutex.Unlock()
			return outcome, ctx.Err()
		case <-time.After(s.interval):
		}
	}
	if err := out.Usage(outcome.Units, outcome.UsageSource); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (s *stubAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "stub", Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

func (s *stubAdapter) snapshot() (written int, cancelled bool, calls int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.written, s.cancelled, s.calls
}

type harness struct {
	server  *httptest.Server
	adapter *stubAdapter
	keyID   string
	private ed25519.PrivateKey
	logs    *lockedBuffer
}

type stubCredentialValidator struct{}

func (stubCredentialValidator) Validate(_ context.Context, task contract.KaanaCredentialValidationTask) (contract.KaanaCredentialValidationOutcome, error) {
	return contract.KaanaCredentialValidationOutcome{
		SchemaVersion: task.SchemaVersion, OperationID: task.OperationID,
		ApplicationID: task.ApplicationID, Provider: task.Provider,
		OwnerAccountID: task.OwnerAccountID, ConnectionID: task.ConnectionID,
		Environment: task.Environment, CredentialHandle: task.CredentialHandle,
		CredentialRevision: task.CredentialRevision, DeploymentID: task.DeploymentID,
		State: contract.KaanaCredentialValidationPending,
	}, nil
}

// lockedBuffer collects the operator log so a test can assert what was written
// there rather than to the customer.
type lockedBuffer struct {
	mutex   sync.Mutex
	content strings.Builder
}

func (b *lockedBuffer) Write(payload []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.Write(payload)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.String()
}

// Rate card numbers for the stub deployment. They are invented and distinctive:
// XTS is the ISO 4217 code reserved for testing, and the amounts are chosen so
// the resulting total appears nowhere else in a response by coincidence.
const (
	testCurrency          = "XTS"
	testRequestRate       = 13_000_000_000
	testOutputTokenRate   = 7_000_000_000
	testCostForThreeChunk = testRequestRate + 3*testOutputTokenRate
)

// testRateCards prices the stub deployment, so the containment check below has
// a real amount to look for rather than a zero that would be absent anyway.
func testRateCards(t *testing.T) *providercost.Cards {
	t.Helper()
	document := fmt.Sprintf(`{"rateCards":[{
		"deploymentId":"dep_stub",
		"currency":%q,
		"rates":[
			{"unit":"requests","amountPerUnit":%d},
			{"unit":"output_tokens","amountPerUnit":%d}
		]}]}`, testCurrency, testRequestRate, testOutputTokenRate)
	cards, err := providercost.Parse([]byte(document))
	if err != nil {
		t.Fatalf("building the test rate cards: %v", err)
	}
	return cards
}

func newHarness(t *testing.T, adapter *stubAdapter) *harness {
	t.Helper()
	return newHarnessWithDeployments(t, adapter, []map[string]any{{
		"deploymentId":    "dep_stub",
		"provider":        "stub",
		"modelReference":  "stub/model@2026-05-01",
		"upstreamModelId": "provider-private-route-name",
		"regions":         []string{"test-region"},
		"current":         true,
	}})
}

func newHarnessWithDeployments(t *testing.T, adapter *stubAdapter, deployments []map[string]any) *harness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating an edge key: %v", err)
	}
	const keyID = "edge-test-key"

	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{keyID: public}, time.Minute)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	validationVerifier, err := edgeauth.NewCredentialValidationVerifier(map[string]ed25519.PublicKey{keyID: public}, time.Minute)
	if err != nil {
		t.Fatalf("building the credential-validation verifier: %v", err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	inventoryJSON, err := json.Marshal(map[string]any{
		"snapshotId":  "snap_stub",
		"issuedAt":    contract.NewTimestamp(time.Now()),
		"deployments": deployments,
	})
	if err != nil {
		t.Fatalf("encoding the inventory: %v", err)
	}
	if err := os.WriteFile(path, inventoryJSON, 0o600); err != nil {
		t.Fatalf("writing the inventory: %v", err)
	}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, err := inventory.NewStore(inventory.Config{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("building the inventory: %v", err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("registering the adapter: %v", err)
	}
	rotationRegistry := rotation.NewRegistry(rotation.Policy{}, nil)
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: store,
		Providers: registry,
		Rotation:  rotationRegistry,
		Costs:     testRateCards(t),
	})
	if err != nil {
		t.Fatalf("building the executor: %v", err)
	}
	api, err := httpapi.New(httpapi.Config{
		Executor:            executor,
		Verifier:            verifier,
		ValidationVerifier:  validationVerifier,
		CredentialValidator: stubCredentialValidator{},
		Registry:            registry,
		Inventory:           store,
		Rotation:            rotationRegistry,
		Logger:              logger,
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return &harness{server: server, adapter: adapter, keyID: keyID, private: private, logs: logs}
}

// sign produces the headers the Oxy edge would send. It signs with
// edgeauth.SigningInput rather than a second copy of the framing, so the test
// cannot drift from the verifier by agreeing with itself.
func (h *harness) sign(request *http.Request, body []byte) {
	milliseconds := time.Now().UnixMilli()
	signature := ed25519.Sign(h.private, edgeauth.SigningInput(h.keyID, milliseconds, body))
	request.Header.Set(edgeauth.HeaderKeyID, h.keyID)
	request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(signature))
}

func (h *harness) signValidation(request *http.Request, body []byte) {
	milliseconds := time.Now().UnixMilli()
	signature := ed25519.Sign(h.private, edgeauth.CredentialValidationSigningInput(h.keyID, milliseconds, body))
	request.Header.Set(edgeauth.HeaderKeyID, h.keyID)
	request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(signature))
}

func (h *harness) postValidation(t *testing.T, body []byte, validationDomain bool) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/customer-provider-credentials/validations", readerOf(body))
	if err != nil {
		t.Fatalf("building validation request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if validationDomain {
		h.signValidation(request, body)
	} else {
		h.sign(request, body)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatalf("posting validation: %v", err)
	}
	return response
}

func (h *harness) post(t *testing.T, ctx context.Context, body []byte, sign bool) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/internal/v1/inference", readerOf(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if sign {
		h.sign(request, body)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatalf("sending the request: %v", err)
	}
	return response
}

func TestCredentialValidationRequiresItsOwnSignatureDomainAndExactClosedTask(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	body := []byte(`{"schemaVersion":1,"operationId":"op_exact","applicationId":"app_exact","provider":"stub","ownerAccountId":"acc_exact","connectionId":"conn_exact","environment":"production","credentialHandle":"kcred_abcdefghijklmnopqrstuvwxyz","credentialRevision":7,"deploymentId":"dep_stub"}`)

	wrongDomain := h.postValidation(t, body, false)
	defer func() { _ = wrongDomain.Body.Close() }()
	if wrongDomain.StatusCode != http.StatusUnauthorized {
		t.Fatalf("inference-domain signature status = %d", wrongDomain.StatusCode)
	}

	response := h.postValidation(t, body, true)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("validation status/cache = %d/%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var outcome contract.KaanaCredentialValidationOutcome
	if err := json.NewDecoder(response.Body).Decode(&outcome); err != nil {
		t.Fatalf("decoding outcome: %v", err)
	}
	if outcome.OperationID != "op_exact" || outcome.ApplicationID != "app_exact" ||
		outcome.ConnectionID != "conn_exact" || outcome.DeploymentID != "dep_stub" ||
		outcome.State != "pending" {
		t.Fatalf("exact validation outcome = %+v", outcome)
	}

	duplicate := []byte(`{"schemaVersion":1,"operationId":"op_exact","operationId":"op_rebound","applicationId":"app_exact","provider":"stub","ownerAccountId":"acc_exact","connectionId":"conn_exact","environment":"production","credentialHandle":"kcred_abcdefghijklmnopqrstuvwxyz","credentialRevision":7,"deploymentId":"dep_stub"}`)
	rejected := h.postValidation(t, duplicate, true)
	defer func() { _ = rejected.Body.Close() }()
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate selector status = %d", rejected.StatusCode)
	}
}

func readerOf(body []byte) io.Reader { return &sliceReader{body: body} }

type sliceReader struct {
	body []byte
	at   int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.at >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(p, r.body[r.at:])
	r.at += n
	return n, nil
}

func envelope(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	body := map[string]any{
		"schemaVersion": contract.RequestEnvelopeVersion,
		"attribution": map[string]any{
			"principal": map[string]any{
				"billing":         map[string]any{"accountId": "acc_test"},
				"applicationId":   "app_test",
				"credentialId":    "cred_test",
				"environment":     "development",
				"inferenceScopes": []string{"inference:invoke"},
			},
			"requestId": "req_test",
		},
		"target":   map[string]any{"kind": "model", "modelReference": "stub/model@2026-05-01"},
		"modality": "text",
		"input": map[string]any{
			"format":   "messages",
			"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "hi"}}}},
		},
		"stream":   true,
		"sampling": map[string]any{},
		"tools":    []any{},
		"client": map[string]any{
			"apiFormat":  "responses",
			"endpoint":   "/v1/responses",
			"receivedAt": string(contract.NewTimestamp(time.Now())),
		},
		"routingPolicy": map[string]any{"routingPolicyId": "rp_test", "policyVersion": 1},
	}
	if mutate != nil {
		mutate(body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the envelope: %v", err)
	}
	return encoded
}

type frames struct {
	events  []map[string]any
	reports []map[string]any
	// rawReports is the undecoded report payload. Decoding into map[string]any
	// is lossy for the distinction that matters here: an absent array, a null
	// one and an empty one all arrive as something a Go test reads as "empty",
	// so the bytes are kept as well.
	rawReports []string
}

func readFrames(t *testing.T, body io.Reader) frames {
	t.Helper()
	var collected frames
	decoder := sse.NewDecoder(body)
	for {
		frame, more := decoder.Next()
		if !more {
			return collected
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
			t.Fatalf("frame %q does not decode: %v", frame.Data, err)
		}
		switch frame.Name {
		case httpapi.FrameStreamEvent:
			collected.events = append(collected.events, payload)
		case httpapi.FrameUsageReport:
			collected.reports = append(collected.reports, payload)
			collected.rawReports = append(collected.rawReports, frame.Data)
		default:
			t.Fatalf("unexpected frame name %q", frame.Name)
		}
	}
}

/* -------------------------------------------------------------------------- */
/*  Admission                                                                 */
/* -------------------------------------------------------------------------- */

func TestASignedEnvelopeIsServedAndAnUnsignedOneIsNot(t *testing.T) {
	// The signed case runs first as the control. Without it, "unsigned is
	// rejected" is also what a server that rejects everything reports.
	t.Run("signed", func(t *testing.T) {
		harness := newHarness(t, &stubAdapter{chunks: 2})
		response := harness.post(t, context.Background(), envelope(t, nil), true)

		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			t.Fatalf("a signed envelope was answered %d", response.StatusCode)
		}
		if got := response.Header.Get(httpapi.HeaderRequestID); got != "req_test" {
			t.Errorf("the response echoes request id %q", got)
		}

		// The stream is drained before the adapter is inspected. Status and
		// headers arrive as soon as the response is opened, which is BEFORE the
		// executor has reached the adapter — asserting on the count here without
		// waiting for the stream to end measures scheduling order.
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatalf("draining the stream: %v", err)
		}
		_ = response.Body.Close()

		if _, _, calls := harness.adapter.snapshot(); calls != 1 {
			t.Errorf("the adapter was called %d times for one signed envelope", calls)
		}
	})

	t.Run("unsigned", func(t *testing.T) {
		harness := newHarness(t, &stubAdapter{chunks: 2})
		response := harness.post(t, context.Background(), envelope(t, nil), false)
		defer func() { _ = response.Body.Close() }()

		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an unsigned envelope was answered %d", response.StatusCode)
		}
		if _, _, calls := harness.adapter.snapshot(); calls != 0 {
			t.Errorf("an unsigned envelope reached the adapter %d times", calls)
		}
		var failure contract.Error
		if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
			t.Fatalf("the rejection body does not decode: %v", err)
		}
		if failure.Code != contract.CodeAuthenticationFailed {
			t.Errorf("an unsigned envelope was refused with %q", failure.Code)
		}
		if failure.RequestID == "req_test" {
			t.Error("the rejection echoed a request id from an unverified body, letting an unauthenticated caller choose what appears in Kaana's logs")
		}
	})
}

func TestEnvelopeVersionTransitionAcceptsOnlyLegacyDirectModels(t *testing.T) {
	t.Run("v1 direct model is served", func(t *testing.T) {
		harness := newHarness(t, &stubAdapter{chunks: 1})
		body := envelope(t, func(body map[string]any) {
			body["schemaVersion"] = contract.LegacyRequestEnvelopeVersion
		})
		response := harness.post(t, context.Background(), body, true)
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			payload, _ := io.ReadAll(response.Body)
			t.Fatalf("a transitional direct-model envelope was answered %d: %s", response.StatusCode, payload)
		}
		_ = readFrames(t, response.Body)
		if _, _, calls := harness.adapter.snapshot(); calls != 1 {
			t.Fatalf("the transitional direct model reached the adapter %d times", calls)
		}
	})

	for name, target := range map[string]map[string]any{
		"v1 routing-profile slug arm": {"kind": "routing_profile", "routingProfile": "auto"},
		"v1 slug beside direct model": {"kind": "model", "modelReference": "stub/model@2026-05-01", "routingProfile": "auto"},
	} {
		t.Run(name+" is refused before execution", func(t *testing.T) {
			harness := newHarness(t, &stubAdapter{chunks: 1})
			body := envelope(t, func(body map[string]any) {
				body["schemaVersion"] = contract.LegacyRequestEnvelopeVersion
				body["target"] = target
			})
			response := harness.post(t, context.Background(), body, true)
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusBadRequest {
				payload, _ := io.ReadAll(response.Body)
				t.Fatalf("the retired routing-profile slug was answered %d: %s", response.StatusCode, payload)
			}
			if _, _, calls := harness.adapter.snapshot(); calls != 0 {
				t.Fatalf("the retired routing-profile slug reached the adapter %d times", calls)
			}
		})
	}
}

func TestV2ExactRoutingProfileExecutesOnlyItsSignedDeploymentIDs(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 1})
	body := envelope(t, func(body map[string]any) {
		body["target"] = map[string]any{"kind": "routing_profile_id", "routingProfileId": "rpf_exact"}
		body["authorizedRoutes"] = []map[string]any{{
			"substitution": "same_model", "deploymentId": "dep_stub",
			"modelReference": "stub/model@2026-05-01", "provider": "stub",
			"regions": []string{"test-region"},
		}}
	})
	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("the exact routing-profile target was answered %d: %s", response.StatusCode, payload)
	}
	_ = readFrames(t, response.Body)
	if _, _, calls := harness.adapter.snapshot(); calls != 1 {
		t.Fatalf("the exact authorized deployment was called %d times", calls)
	}
}

func TestASignedExplicitNullCustomerCredentialNeverReachesAPlatformPool(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 2})
	body := envelope(t, func(body map[string]any) {
		body["authorizedRoutes"] = []map[string]any{{
			"substitution":               "same_model",
			"deploymentId":               "dep_stub",
			"modelReference":             "stub/model@2026-05-01",
			"provider":                   "stub",
			"regions":                    []string{"test-region"},
			"customerProviderCredential": nil,
		}}
	})

	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("the explicit null customer binding was answered %d: %s", response.StatusCode, payload)
	}
	if _, _, calls := harness.adapter.snapshot(); calls != 0 {
		t.Fatalf("the explicit null customer binding reached the adapter %d times", calls)
	}
	var failure contract.Error
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatalf("the explicit null rejection does not decode: %v", err)
	}
	if failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("the explicit null customer binding was refused with %q", failure.Code)
	}
}

func TestATamperedBodyIsRejected(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 2})
	signed := envelope(t, nil)
	tampered := envelope(t, func(body map[string]any) {
		body["attribution"].(map[string]any)["principal"].(map[string]any)["billing"] = map[string]any{"accountId": "acc_someone_else"}
	})

	request, err := http.NewRequest(http.MethodPost, harness.server.URL+"/internal/v1/inference", readerOf(tampered))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	// Signed over the original bytes, sent with the swapped ones — the exact
	// shape of an attempt to redirect a charge to another account.
	harness.sign(request, signed)

	response, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("sending the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a body that does not match its signature was answered %d", response.StatusCode)
	}
	if _, _, calls := harness.adapter.snapshot(); calls != 0 {
		t.Errorf("a tampered envelope reached the adapter %d times", calls)
	}
}

func TestAnUnimplementedEnvelopeVersionIsRefusedWhole(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 2})
	body := envelope(t, func(body map[string]any) { body["schemaVersion"] = 3 })

	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an envelope declaring version 3 was answered %d", response.StatusCode)
	}
	var failure contract.Error
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatalf("the rejection body does not decode: %v", err)
	}
	if failure.Param == nil || *failure.Param != "schemaVersion" {
		t.Errorf("the refusal does not name schemaVersion as the field at fault")
	}
	if _, _, calls := harness.adapter.snapshot(); calls != 0 {
		t.Errorf("an envelope of an unimplemented version reached the adapter %d times", calls)
	}
}

func TestAnEnvelopeWithNoVersionIsRefused(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 2})
	body := envelope(t, func(body map[string]any) { delete(body, "schemaVersion") })

	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an envelope with no version was answered %d; a version must never be inferred from the presence of a field", response.StatusCode)
	}
}

// TestAdditiveFieldsAreTolerated pins a decision that is easy to reverse by
// accident. The contract says adding an optional field does not bump a shape's
// version, so a strict decoder here would turn every additive Oxy change into a
// production outage on Kaana's side.
func TestAdditiveFieldsAreTolerated(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 1})
	body := envelope(t, func(body map[string]any) {
		body["someFieldOxyAddedLater"] = map[string]any{"nested": true}
		body["client"].(map[string]any)["labels"] = map[string]string{"team": "search"}
	})

	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("an envelope carrying an unknown optional field was answered %d", response.StatusCode)
	}
	collected := readFrames(t, response.Body)
	if len(collected.events) == 0 {
		t.Fatal("no events were streamed")
	}
	if last := collected.events[len(collected.events)-1]["type"]; last != string(contract.EventDone) {
		t.Errorf("the stream ended with %v", last)
	}
}

/* -------------------------------------------------------------------------- */
/*  Streaming                                                                 */
/* -------------------------------------------------------------------------- */

func TestStreamsNormalizedEventsThenAUsageReport(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 3})
	response := harness.post(t, context.Background(), envelope(t, nil), true)
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("the response content type is %q", got)
	}
	collected := readFrames(t, response.Body)

	if len(collected.events) < 3 {
		t.Fatalf("only %d events were streamed", len(collected.events))
	}
	if collected.events[0]["type"] != string(contract.EventStart) {
		t.Errorf("the stream opens with %v", collected.events[0]["type"])
	}
	if last := collected.events[len(collected.events)-1]["type"]; last != string(contract.EventDone) {
		t.Errorf("the stream ends with %v", last)
	}
	for index, event := range collected.events {
		if event["requestId"] != "req_test" {
			t.Errorf("event %d carries requestId %v", index, event["requestId"])
		}
		if event["sequence"] != float64(index) {
			t.Errorf("event %d carries sequence %v", index, event["sequence"])
		}
	}

	if len(collected.reports) != 1 {
		t.Fatalf("%d usage reports were delivered, expected exactly 1", len(collected.reports))
	}
	var report contract.UsageReport
	encoded, err := json.Marshal(collected.reports[0])
	if err != nil {
		t.Fatalf("re-encoding the report: %v", err)
	}
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatalf("the report does not decode: %v", err)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("the delivered usage report would be rejected by the contract: %v", err)
	}
	if report.Outcome != contract.OutcomeCompleted {
		t.Errorf("the report says %q", report.Outcome)
	}
	// The report is what settlement runs against, so the attribution on it has
	// to be the envelope's, unchanged.
	if report.Attribution.Principal.Billing.AccountID != "acc_test" {
		t.Errorf("the report bills %q", report.Attribution.Principal.Billing.AccountID)
	}
}

// TestAFailedGenerationReportsUnitsAsAnEmptyArray is the entrypoint half of the
// encoder gate in internal/contract.
//
// A MarshalJSON can be correct and inert: the report only reaches Oxy through
// the frame this handler writes, so the test that matters asserts THOSE bytes,
// with a provider failing the way the production one did — refusing before
// anything was produced or measured.
//
// The check reads the raw frame rather than a decoded map, because `null`, `[]`
// and an absent field all decode to something a Go assertion reads as empty,
// and the whole defect is which of the three crossed the wire.
func TestAFailedGenerationReportsUnitsAsAnEmptyArray(t *testing.T) {
	harness := newHarness(t, &stubAdapter{fail: provider.ErrUpstream{
		Code:     contract.CodeQuotaExceeded,
		Category: contract.UpstreamQuota,
		Detail:   "the provider account has no balance",
	}})
	response := harness.post(t, context.Background(), envelope(t, nil), true)
	defer func() { _ = response.Body.Close() }()

	collected := readFrames(t, response.Body)
	if len(collected.rawReports) != 1 {
		t.Fatalf("%d usage reports were delivered, expected exactly 1", len(collected.rawReports))
	}
	report := collected.rawReports[0]

	// The premise: without a failed generation whose units are absent, the
	// assertion below would pass on any report at all.
	if !strings.Contains(report, `"outcome":"failed"`) {
		t.Fatalf("this report is not the shape under test:\n%s", report)
	}
	if strings.Contains(report, `"units":null`) {
		t.Errorf("the edge is sent units as null, which the published schema cannot parse:\n%s", report)
	}
	if !strings.Contains(report, `"units":[]`) {
		t.Errorf("the edge is not sent an empty unit array:\n%s", report)
	}

	// The terminal error frame is what the edge would otherwise lose: it reads
	// the failure first and then parses the report, so a report it cannot parse
	// throws away the provider's real refusal and reports an internal error.
	if len(collected.events) != 1 || collected.events[0]["type"] != string(contract.EventError) {
		t.Fatalf("the customer was not told why the request failed: %v", collected.events)
	}
	failure, ok := collected.events[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("the terminal event carries no error body: %v", collected.events[0])
	}
	if failure["code"] != string(contract.CodeQuotaExceeded) {
		t.Errorf("the terminal error carries code %v", failure["code"])
	}
}

// TestUpstreamCostNeverReachesTheCustomer is the containment gate on provider
// cost.
//
// Kaana measures what a request cost it upstream and never quotes an amount to
// anyone: the money is an operator number, and the contract has no field on any
// produced shape that could carry it. The check is the same amount in two
// places — present in the operator log, absent from every byte the customer
// receives — so "no cost in the response" cannot be what a request that was
// never priced also reports.
func TestUpstreamCostNeverReachesTheCustomer(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 3})
	response := harness.post(t, context.Background(), envelope(t, nil), true)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	_ = response.Body.Close()

	// Positive control: the cost was measured, and this is the number to look
	// for. Without it, every assertion below would pass on a build that never
	// priced anything.
	amount := strconv.Itoa(testCostForThreeChunk)
	logged := harness.logs.String()
	if !strings.Contains(logged, amount) {
		t.Fatalf("the upstream cost %s was not measured or not logged, so the containment check measures nothing:\n%s", amount, logged)
	}
	for _, required := range []string{
		`"deploymentId":"dep_stub"`,
		`"timeToFirstTokenMs":`,
		`"units":[{"unit":"requests","quantity":1},{"unit":"output_tokens","quantity":3}]`,
	} {
		if !strings.Contains(logged, required) {
			t.Errorf("the operator log does not carry the routing measurement %s:\n%s", required, logged)
		}
	}
	if strings.Contains(logged, "chunk 0") {
		t.Errorf("the operator log carries generated content:\n%s", logged)
	}

	served := string(body)
	for _, forbidden := range []string{amount, testCurrency, "upstreamCost", "currency", "cost"} {
		if strings.Contains(served, forbidden) {
			t.Errorf("the customer's stream carries %q, which is an amount Kaana does not quote:\n%s", forbidden, served)
		}
	}
}

// TestTheHealthSurfaceProjectsRotationAndConfiguration: a deployment out of
// rotation and a snapshot that has stopped advancing are the two states an
// operator cannot infer from the shape of a customer's errors.
func TestTheHealthSurfaceProjectsRotationAndConfiguration(t *testing.T) {
	harness := newHarness(t, &stubAdapter{chunks: 1})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, harness.server.URL+"/internal/v1/health", readerOf(nil))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	harness.sign(request, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("requesting health: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	var projected struct {
		Configuration inventory.SnapshotStatus `json:"configuration"`
		Deployments   []struct {
			DeploymentID string  `json:"deploymentId"`
			Provider     string  `json:"provider"`
			State        string  `json:"state"`
			Score        float64 `json:"score"`
		} `json:"deployments"`
	}
	if err := json.NewDecoder(response.Body).Decode(&projected); err != nil {
		t.Fatalf("decoding health: %v", err)
	}

	if projected.Configuration.SnapshotID != "snap_stub" {
		t.Errorf("health names snapshot %q", projected.Configuration.SnapshotID)
	}
	if !projected.Configuration.ServesUnpinnedReferences {
		t.Error("a snapshot issued moments ago is reported as too stale to resolve unpinned references")
	}
	if len(projected.Deployments) != 1 {
		t.Fatalf("health projects %d deployments, the inventory declares 1", len(projected.Deployments))
	}
	if projected.Deployments[0].DeploymentID != "dep_stub" || projected.Deployments[0].Provider != "stub" {
		t.Errorf("health projects deployment %+v", projected.Deployments[0])
	}
	if projected.Deployments[0].State != string(rotation.StateClosed) {
		t.Errorf("an untouched deployment is projected as %q", projected.Deployments[0].State)
	}
}

// TestClientDisconnectCancelsExecution is the first half of the cancellation
// proof: over a real TCP connection, a client that goes away cancels the
// context the executor and the adapter run under.
func TestClientDisconnectCancelsExecution(t *testing.T) {
	// Control first: an identical request that is NOT cancelled must run to
	// completion. Without it, "the adapter observed a cancelled context" is
	// also what a request that simply ended would report.
	t.Run("control: an uninterrupted request runs to completion", func(t *testing.T) {
		harness := newHarness(t, &stubAdapter{chunks: 5, interval: 20 * time.Millisecond})
		response := harness.post(t, context.Background(), envelope(t, nil), true)
		defer func() { _ = response.Body.Close() }()
		_ = readFrames(t, response.Body)

		written, cancelled, _ := harness.adapter.snapshot()
		if cancelled {
			t.Fatal("an uninterrupted request reported a cancellation")
		}
		if written != 5 {
			t.Fatalf("the control wrote %d of 5 chunks", written)
		}
	})

	t.Run("a disconnect stops the work", func(t *testing.T) {
		harness := newHarness(t, &stubAdapter{chunks: 5, interval: 60 * time.Millisecond})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		response := harness.post(t, ctx, envelope(t, nil), true)
		defer func() { _ = response.Body.Close() }()

		// Read until the first output actually reaches the client, then hang
		// up. Cancelling before any output would prove only that a request can
		// be aborted before it starts.
		decoder := sse.NewDecoder(response.Body)
		for {
			frame, more := decoder.Next()
			if !more {
				t.Fatal("the stream ended before any output arrived")
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
				t.Fatalf("frame %q does not decode: %v", frame.Data, err)
			}
			if event["type"] == string(contract.EventDelta) {
				break
			}
		}
		cancel()

		deadline := time.Now().Add(3 * time.Second)
		for {
			written, cancelled, _ := harness.adapter.snapshot()
			if cancelled {
				if written >= 5 {
					t.Fatalf("the adapter wrote all 5 chunks before noticing; that is what completing normally looks like")
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the client disconnected and the adapter never saw a cancelled context (wrote %d of 5 chunks)", written)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

/* -------------------------------------------------------------------------- */
/*  Health                                                                    */
/* -------------------------------------------------------------------------- */

func TestHealthRequiresASignatureAndLivezDoesNot(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})

	unsigned, err := harness.server.Client().Get(harness.server.URL + "/internal/v1/health")
	if err != nil {
		t.Fatalf("requesting health: %v", err)
	}
	defer func() { _ = unsigned.Body.Close() }()
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned health was answered %d", unsigned.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, harness.server.URL+"/internal/v1/health", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	harness.sign(request, nil)
	signed, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting health: %v", err)
	}
	defer func() { _ = signed.Body.Close() }()
	if signed.StatusCode != http.StatusOK {
		t.Fatalf("signed health was answered %d", signed.StatusCode)
	}
	var health struct {
		ContractVersion string            `json:"contractVersion"`
		Providers       []provider.Health `json:"providers"`
	}
	if err := json.NewDecoder(signed.Body).Decode(&health); err != nil {
		t.Fatalf("the health body does not decode: %v", err)
	}
	if health.ContractVersion != contract.ContractVersion {
		t.Errorf("health reports contract version %q", health.ContractVersion)
	}
	if len(health.Providers) != 1 {
		t.Errorf("health reports %d providers", len(health.Providers))
	}

	live, err := harness.server.Client().Get(harness.server.URL + "/livez")
	if err != nil {
		t.Fatalf("requesting livez: %v", err)
	}
	defer func() { _ = live.Body.Close() }()
	if live.StatusCode != http.StatusOK {
		t.Errorf("livez was answered %d", live.StatusCode)
	}
	payload, err := io.ReadAll(live.Body)
	if err != nil {
		t.Fatalf("reading livez: %v", err)
	}
	// The unauthenticated route must not describe providers or routes.
	if string(payload) == "" || jsonHasKey(t, payload, "providers") {
		t.Errorf("the unauthenticated liveness route exposes provider detail: %s", payload)
	}
}

// The descriptor surface exists for one purpose: turn an exact opaque
// deployment id into the other three fields Oxy must sign. It is not another
// catalogue and not a projection of provider configuration.
func TestDeploymentDescriptorsAreSignedExactAndContainOnlyRouteIdentity(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})

	unsigned := harness.postDeploymentQuery(t, []byte(`{}`), false)
	unsignedBody, readErr := io.ReadAll(unsigned.Body)
	_ = unsigned.Body.Close()
	if readErr != nil {
		t.Fatalf("reading the unsigned refusal: %v", readErr)
	}
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the unsigned descriptor surface was answered %d", unsigned.StatusCode)
	}
	if unsigned.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the unsigned refusal may be cached: Cache-Control=%q", unsigned.Header.Get("Cache-Control"))
	}
	if strings.Contains(string(unsignedBody), "dep_stub") || strings.Contains(string(unsignedBody), "snap_stub") {
		t.Errorf("an unsigned request learned deployment identity: %s", unsignedBody)
	}

	response := harness.postDeploymentQuery(t, []byte(`{"deploymentId":"dep_stub"}`), true)
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("reading the descriptor response: %v", readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the signed descriptor lookup was answered %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the operator descriptor may be cached: Cache-Control=%q", response.Header.Get("Cache-Control"))
	}

	var projection map[string]json.RawMessage
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("the descriptor response does not decode: %v", err)
	}
	if len(projection) != 2 || projection["snapshotId"] == nil || projection["deployments"] == nil {
		t.Fatalf("the descriptor response has fields other than snapshotId and deployments: %s", body)
	}
	var snapshotID string
	if err := json.Unmarshal(projection["snapshotId"], &snapshotID); err != nil {
		t.Fatalf("snapshotId does not decode: %v", err)
	}
	if snapshotID != "snap_stub" {
		t.Errorf("the descriptor response names snapshot %q", snapshotID)
	}

	var deployments []map[string]json.RawMessage
	if err := json.Unmarshal(projection["deployments"], &deployments); err != nil {
		t.Fatalf("deployments do not decode: %v", err)
	}
	if len(deployments) != 1 {
		t.Fatalf("the exact lookup returned %d deployments", len(deployments))
	}
	descriptor := deployments[0]
	for _, field := range []string{"deploymentId", "modelReference", "provider", "regions"} {
		if descriptor[field] == nil {
			t.Errorf("the exact descriptor omits %s: %s", field, body)
		}
	}
	if len(descriptor) != 4 {
		t.Errorf("the exact descriptor exposes fields beyond signed route identity: %s", body)
	}

	var exact struct {
		DeploymentID   string   `json:"deploymentId"`
		ModelReference string   `json:"modelReference"`
		Provider       string   `json:"provider"`
		Regions        []string `json:"regions"`
	}
	if err := json.Unmarshal(mustFirstRaw(t, projection["deployments"]), &exact); err != nil {
		t.Fatalf("the exact descriptor does not decode: %v", err)
	}
	if exact.DeploymentID != "dep_stub" || exact.ModelReference != "stub/model@2026-05-01" || exact.Provider != "stub" {
		t.Errorf("the exact descriptor changed identity: %+v", exact)
	}
	if len(exact.Regions) != 1 || exact.Regions[0] != "test-region" {
		t.Errorf("the exact descriptor reports regions %v", exact.Regions)
	}

	served := string(body)
	for _, forbidden := range []string{
		"upstreamModelId",
		"provider-private-route-name",
		"credential",
		"apiKey",
		"prompt",
		"payload",
	} {
		if strings.Contains(served, forbidden) {
			t.Errorf("the descriptor surface exposes %q: %s", forbidden, served)
		}
	}
}

func TestDeploymentDescriptorListNamesTheServingSnapshot(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	response := harness.postDeploymentQuery(t, []byte(`{}`), true)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("the descriptor list was answered %d: %s", response.StatusCode, body)
	}
	var projection struct {
		SnapshotID  string `json:"snapshotId"`
		Deployments []struct {
			DeploymentID string `json:"deploymentId"`
		} `json:"deployments"`
	}
	if err := json.NewDecoder(response.Body).Decode(&projection); err != nil {
		t.Fatalf("decoding the descriptor list: %v", err)
	}
	if projection.SnapshotID != "snap_stub" {
		t.Errorf("the descriptor list names snapshot %q", projection.SnapshotID)
	}
	if len(projection.Deployments) != 1 || projection.Deployments[0].DeploymentID != "dep_stub" {
		t.Errorf("the descriptor list is not the serving snapshot's exact identity: %+v", projection.Deployments)
	}
}

func TestDeploymentDescriptorBatchIsAtomicAndContainsNoExtraRoutes(t *testing.T) {
	harness := newHarnessWithDeployments(t, &stubAdapter{}, []map[string]any{
		{
			"deploymentId": "dep_z", "provider": "stub",
			"modelReference": "stub/z@2026-05-01", "upstreamModelId": "private-z",
			"regions": []string{"region-z"}, "current": true,
		},
		{
			"deploymentId": "dep_a", "provider": "stub",
			"modelReference": "stub/a@2026-05-01", "upstreamModelId": "private-a",
			"regions": []string{}, "current": true,
		},
		{
			"deploymentId": "dep_extra", "provider": "stub",
			"modelReference": "stub/extra@2026-05-01", "upstreamModelId": "private-extra",
			"regions": []string{"region-extra"}, "current": true,
		},
	})

	response := harness.postDeploymentQuery(t, []byte(`{"deploymentIds":["dep_z","dep_a"]}`), true)
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("reading the batch descriptor response: %v", readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the batch descriptor lookup was answered %d: %s", response.StatusCode, body)
	}
	var projection struct {
		SnapshotID  string `json:"snapshotId"`
		Deployments []struct {
			DeploymentID string   `json:"deploymentId"`
			Regions      []string `json:"regions"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("decoding the batch descriptor response: %v", err)
	}
	if projection.SnapshotID != "snap_stub" {
		t.Errorf("the batch names snapshot %q", projection.SnapshotID)
	}
	if len(projection.Deployments) != 2 ||
		projection.Deployments[0].DeploymentID != "dep_a" ||
		projection.Deployments[1].DeploymentID != "dep_z" {
		t.Fatalf("the batch is not the exact stable projection: %+v", projection.Deployments)
	}
	if projection.Deployments[0].Regions == nil || len(projection.Deployments[0].Regions) != 0 {
		t.Errorf("the unattested region set changed to %#v", projection.Deployments[0].Regions)
	}

	missing := harness.postDeploymentQuery(
		t, []byte(`{"deploymentIds":["dep_a","dep_missing"]}`), true)
	missingBody, readErr := io.ReadAll(missing.Body)
	_ = missing.Body.Close()
	if readErr != nil {
		t.Fatalf("reading the atomic batch refusal: %v", readErr)
	}
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("the partial batch was answered %d: %s", missing.StatusCode, missingBody)
	}
	if strings.Contains(string(missingBody), "dep_a") {
		t.Fatalf("the partial batch leaked a matching descriptor: %s", missingBody)
	}
}

func (h *harness) postDeploymentQuery(t *testing.T, body []byte, signed bool) *http.Response {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		h.server.URL+"/internal/v1/deployments/query",
		readerOf(body),
	)
	if err != nil {
		t.Fatalf("building the deployment descriptor request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if signed {
		h.sign(request, body)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting deployment descriptors: %v", err)
	}
	return response
}

func mustFirstRaw(t *testing.T, encoded []byte) json.RawMessage {
	t.Helper()
	var values []json.RawMessage
	if err := json.Unmarshal(encoded, &values); err != nil {
		t.Fatalf("decoding JSON array: %v", err)
	}
	if len(values) == 0 {
		t.Fatal("the JSON array is empty")
	}
	return values[0]
}

func TestDeploymentDescriptorLookupDoesNotGuess(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})

	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
		wantCode   contract.ErrorCode
	}{
		{name: "a prefix is not an identity", body: `{"deploymentId":"dep_stu"}`, wantStatus: http.StatusNotFound, wantCode: contract.CodeNoRouteAvailable},
		{name: "an absent id is refused", body: `{"deploymentId":"dep_absent"}`, wantStatus: http.StatusNotFound, wantCode: contract.CodeNoRouteAvailable},
		{name: "a null id is invalid", body: `{"deploymentId":null}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "an empty id is invalid", body: `{"deploymentId":""}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "an unknown field is invalid", body: `{"deploymentID":"dep_stub"}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "an extra field is invalid", body: `{"deploymentId":"dep_stub","foo":"bar"}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a duplicate key is invalid", body: `{"deploymentId":"dep_stub","deploymentId":"dep_absent"}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "an empty batch is invalid", body: `{"deploymentIds":[]}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a null batch is invalid", body: `{"deploymentIds":null}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a non-string batch id is invalid", body: `{"deploymentIds":["dep_stub",1]}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a duplicate batch id is invalid", body: `{"deploymentIds":["dep_stub","dep_stub"]}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "the single and batch selectors cannot mix", body: `{"deploymentId":"dep_stub","deploymentIds":["dep_stub"]}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a duplicate batch key is invalid", body: `{"deploymentIds":["dep_stub"],"deploymentIds":["dep_stub"]}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "trailing JSON is invalid", body: `{"deploymentId":"dep_stub"}{}`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "malformed JSON is invalid", body: `{"deploymentId":`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "an empty body is invalid", body: ``, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
		{name: "a non-object is invalid", body: `[]`, wantStatus: http.StatusBadRequest, wantCode: contract.CodeInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := harness.postDeploymentQuery(t, []byte(test.body), true)
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				t.Fatalf("reading the lookup refusal: %v", readErr)
			}
			if response.StatusCode != test.wantStatus {
				t.Fatalf("lookup was answered %d, want %d: %s", response.StatusCode, test.wantStatus, body)
			}
			var failure contract.Error
			if err := json.Unmarshal(body, &failure); err != nil {
				t.Fatalf("the lookup refusal does not decode: %v", err)
			}
			if failure.Code != test.wantCode {
				t.Errorf("the lookup was refused as %q, want %q: %s", failure.Code, test.wantCode, body)
			}
		})
	}
}

func TestDeploymentDescriptorIDIsCoveredByTheSignature(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	signedBody := []byte(`{"deploymentId":"dep_stub"}`)
	sentBody := []byte(`{"deploymentId":"dep_other"}`)
	request, err := http.NewRequest(
		http.MethodPost,
		harness.server.URL+"/internal/v1/deployments/query",
		readerOf(sentBody),
	)
	if err != nil {
		t.Fatalf("building the modified request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	harness.sign(request, signedBody)
	response, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("sending the modified request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("a deployment id changed after signing was answered %d: %s", response.StatusCode, body)
	}
}

func TestDeploymentDescriptorRejectsUnsignedURLParameters(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	body := []byte(`{}`)
	request, err := http.NewRequest(
		http.MethodPost,
		harness.server.URL+"/internal/v1/deployments/query?deploymentId=dep_stub",
		readerOf(body),
	)
	if err != nil {
		t.Fatalf("building the request with URL parameters: %v", err)
	}
	harness.sign(request, body)
	response, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("sending the request with URL parameters: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("unsigned URL parameters were answered %d: %s", response.StatusCode, body)
	}
}

func jsonHasKey(t *testing.T, payload []byte, key string) bool {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	_, present := decoded[key]
	return present
}

// The catalogue route exists so a consumer stops keeping its own copy of this
// list. It is signed like health and unlike livez: which models Oxy has
// contracted for is commercial information, and the body names the providers.
func TestModelsIsSignedAndNamesTheLineRatherThanTheRevision(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})

	unsigned, err := harness.server.Client().Get(harness.server.URL + "/internal/v1/models")
	if err != nil {
		t.Fatalf("requesting models: %v", err)
	}
	defer func() { _ = unsigned.Body.Close() }()
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Errorf("the unsigned catalogue was answered %d", unsigned.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, harness.server.URL+"/internal/v1/models", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	harness.sign(request, nil)
	signed, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting models: %v", err)
	}
	defer func() { _ = signed.Body.Close() }()
	if signed.StatusCode != http.StatusOK {
		t.Fatalf("the signed catalogue was answered %d", signed.StatusCode)
	}

	var catalogue struct {
		ContractVersion      string   `json:"contractVersion"`
		ServesUnpinned       bool     `json:"servesUnpinned"`
		PinnedOnlyReferences []string `json:"pinnedOnlyReferences"`
		Models               []struct {
			Model     string   `json:"model"`
			Reference string   `json:"modelReference"`
			Providers []string `json:"providers"`
		} `json:"models"`
	}
	if err := json.NewDecoder(signed.Body).Decode(&catalogue); err != nil {
		t.Fatalf("the catalogue body does not decode: %v", err)
	}

	if catalogue.ContractVersion != contract.ContractVersion {
		t.Errorf("the catalogue reports contract version %q", catalogue.ContractVersion)
	}
	// Without this a consumer builds a product catalogue out of names that are
	// all refused, and the refusal arrives one request at a time.
	if !catalogue.ServesUnpinned {
		t.Error("a snapshot issued just now reports that unpinned names do not resolve")
	}
	if len(catalogue.Models) != 1 {
		t.Fatalf("the catalogue lists %d models", len(catalogue.Models))
	}
	if catalogue.Models[0].Model != "stub/model" {
		t.Errorf("the entry is named %q, want the line rather than the revision", catalogue.Models[0].Model)
	}
	if catalogue.Models[0].Reference != "stub/model@2026-05-01" {
		t.Errorf("the entry resolves to %q", catalogue.Models[0].Reference)
	}
	if len(catalogue.Models[0].Providers) != 1 || catalogue.Models[0].Providers[0] != "stub" {
		t.Errorf("the entry lists providers %v", catalogue.Models[0].Providers)
	}
	if len(catalogue.PinnedOnlyReferences) != 0 {
		t.Errorf("a snapshot with no superseded revision reports %v", catalogue.PinnedOnlyReferences)
	}
}
