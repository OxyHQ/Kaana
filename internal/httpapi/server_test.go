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

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/edgeauth"
	"github.com/OxyHQ/Relay/internal/httpapi"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/providercost"
	"github.com/OxyHQ/Relay/internal/relay"
	"github.com/OxyHQ/Relay/internal/rotation"
	"github.com/OxyHQ/Relay/internal/sse"
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

func (s *stubAdapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
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

	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating an edge key: %v", err)
	}
	const keyID = "edge-test-key"

	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{keyID: public}, time.Minute)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	inventoryJSON := fmt.Sprintf(`{
		"snapshotId":"snap_stub",
		"issuedAt":%q,
		"deployments":[{
			"deploymentId":"dep_stub","provider":"stub",
			"modelReference":"stub/model@2026-05-01","upstreamModelId":"model",
			"region":"test-region","current":true}]}`, contract.NewTimestamp(time.Now()))
	if err := os.WriteFile(path, []byte(inventoryJSON), 0o600); err != nil {
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
	executor, err := relay.NewExecutor(relay.Config{
		Inventory: store,
		Providers: registry,
		Rotation:  rotationRegistry,
		Costs:     testRateCards(t),
	})
	if err != nil {
		t.Fatalf("building the executor: %v", err)
	}
	api, err := httpapi.New(httpapi.Config{
		Executor:  executor,
		Verifier:  verifier,
		Registry:  registry,
		Inventory: store,
		Rotation:  rotationRegistry,
		Logger:    logger,
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
		"schemaVersion": 1,
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
			t.Error("the rejection echoed a request id from an unverified body, letting an unauthenticated caller choose what appears in Relay's logs")
		}
	})
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
	body := envelope(t, func(body map[string]any) { body["schemaVersion"] = 2 })

	response := harness.post(t, context.Background(), body, true)
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an envelope declaring version 2 was answered %d", response.StatusCode)
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
// production outage on Relay's side.
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

// TestUpstreamCostNeverReachesTheCustomer is the containment gate on provider
// cost.
//
// Relay measures what a request cost it upstream and never quotes an amount to
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

	served := string(body)
	for _, forbidden := range []string{amount, testCurrency, "upstreamCost", "currency", "cost"} {
		if strings.Contains(served, forbidden) {
			t.Errorf("the customer's stream carries %q, which is an amount Relay does not quote:\n%s", forbidden, served)
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

func jsonHasKey(t *testing.T, payload []byte, key string) bool {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	_, present := decoded[key]
	return present
}
