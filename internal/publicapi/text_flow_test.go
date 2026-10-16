package publicapi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func responsesBody(extra map[string]any) map[string]any {
	body := map[string]any{"model": "stub/model", "input": "Hola, mundo secreto"}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

// POST /v1/responses, whole: the folded answer, its usage, the routing
// policy Oxy granted under, the cheapest admissible route first.
func TestResponsesAreServedOnTheCheapestRouteAndSettled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/responses", validKey, responsesBody(map[string]any{
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}))
	body := readAll(t, response)
	var answer struct {
		RequestID     contract.RequestID              `json:"requestId"`
		Model         contract.ModelReference         `json:"model"`
		FinishReason  contract.FinishReason           `json:"finishReason"`
		Output        []contract.Message              `json:"output"`
		Usage         []contract.UsageQuantity        `json:"usage"`
		RoutingPolicy contract.RoutingPolicyReference `json:"routingPolicy"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	assertNoProviderLeak(t, "a response", body)
	if answer.Model != textModel || answer.FinishReason != contract.FinishToolCalls || len(answer.Output) != 1 ||
		len(answer.Output[0].ToolCalls) != 1 || answer.RoutingPolicy.RoutingPolicyID != "rp_1" || len(answer.Usage) != 2 {
		t.Errorf("response = %s", body)
	}
	if response.Header.Get(publicapi.HeaderRoutingPolicyID) != "rp_1" || response.Header.Get(publicapi.HeaderRoutingPolicyVersion) != "2" {
		t.Errorf("routing policy headers = %v", response.Header)
	}
	call := h.adapter.lastCall()
	if call.AuthorizedRoutes[0].DeploymentID != "dep_stub_b" || call.AuthorizedRoutes[1].DeploymentID != "dep_stub_a" {
		t.Errorf("routes = %+v; the free key's deployment goes first", call.AuthorizedRoutes)
	}
	var asked contract.InferenceAuthorizationRequest
	_ = json.Unmarshal([]byte(h.oxy.authorizationBodies()[0]), &asked)
	if strings.Contains(h.oxy.authorizationBodies()[0], "secreto") || asked.Facts.EstimatedInputTokens == nil || asked.Facts.DeclaredTools != 1 {
		t.Errorf("authorization = %s", h.oxy.authorizationBodies()[0])
	}
	h.settle()
	if settlement := h.oxy.settled()["grt_1"]; settlement.Report.DeploymentID != "dep_stub_b" || *settlement.ServedStep != 0 {
		t.Errorf("settlement = %+v", settlement)
	}
}

// A grant's steps in order: the first step's only admissible route fails
// before any output, so the request moves to the second step's line, and the
// settlement names the step that served. A funding ceiling keeps the paid
// route of the first step out entirely.
func TestAGrantsStepsAreTriedInOrderBeforeAnyOutput(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.failDeployment = "dep_stub_b"
	free := contract.FundingTierFree
	xhigh := contract.PowerXHigh
	h.oxy.steps = func(request contract.InferenceAuthorizationRequest) []contract.GrantStep {
		return []contract.GrantStep{
			{Target: *request.Target, Limits: contract.GrantStepLimits{MaxFundingTier: &free,
				AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: textModel.ModelID(), PriceVersionID: "price-1"}}}},
			{Target: contract.GrantTarget{Kind: contract.GrantTargetLevel, Level: &xhigh}, Limits: contract.GrantStepLimits{
				AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: fallbackModel.ModelID(), PriceVersionID: "price-2"}}}},
		}
	}
	response := h.post("/v1/responses", validKey, responsesBody(nil))
	body := readAll(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), string(fallbackModel)) {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	routes := h.adapter.lastCall().AuthorizedRoutes
	if len(routes) != 2 || routes[0].DeploymentID != "dep_stub_b" || routes[1].DeploymentID != "dep_fallback" ||
		routes[1].Substitution != contract.SubstitutionCrossModel || routes[0].MaxFundingTier == nil {
		t.Fatalf("routes = %+v; the paid route is outside the free ceiling", routes)
	}
	h.settle()
	settlement := h.oxy.settled()["grt_1"]
	if settlement.ServedStep == nil || *settlement.ServedStep != 1 || settlement.Report.DeploymentID != "dep_fallback" || settlement.Report.RouteSwitches != 1 {
		t.Errorf("settlement = %+v", settlement)
	}
}

// sameModelFailover: absent and true keep every admissible provider of a line
// behind the best one; false authorizes ONE deployment per admitted line in the
// step, the best-ranked, so a failure there is not retried on another provider.
func TestSameModelFailoverFalseAuthorizesOneDeploymentPerLine(t *testing.T) {
	no, yes := false, true
	for name, tc := range map[string]struct {
		flag *bool
		want []contract.DeploymentID
	}{
		"absent": {nil, []contract.DeploymentID{"dep_stub_b", "dep_stub_a"}},
		"true":   {&yes, []contract.DeploymentID{"dep_stub_b", "dep_stub_a"}},
		"false":  {&no, []contract.DeploymentID{"dep_stub_b"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.oxy.steps = func(request contract.InferenceAuthorizationRequest) []contract.GrantStep {
				return []contract.GrantStep{{Target: *request.Target, Limits: contract.GrantStepLimits{
					SameModelFailover:  tc.flag,
					AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: textModel.ModelID(), PriceVersionID: "price-1"}}}}}
			}
			response := h.post("/v1/responses", validKey, responsesBody(nil))
			body := readAll(t, response)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			var got []contract.DeploymentID
			for _, route := range h.adapter.lastCall().AuthorizedRoutes {
				got = append(got, route.DeploymentID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("routes = %v, want %v", got, tc.want)
			}
		})
	}
}

// With failover off, a failure of the one deployment is final inside the step:
// the request is not served by the other provider of the same line, and it
// moves on only to the NEXT step, which here admits another line.
func TestSameModelFailoverFalseNeverRetriesOnAnotherProviderOfTheLine(t *testing.T) {
	no := false
	h := newHarness(t, harnessOptions{})
	h.adapter.failDeployment = "dep_stub_b"
	h.oxy.steps = func(request contract.InferenceAuthorizationRequest) []contract.GrantStep {
		return []contract.GrantStep{{Target: *request.Target, Limits: contract.GrantStepLimits{
			SameModelFailover:  &no,
			AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: textModel.ModelID(), PriceVersionID: "price-1"}}}}}
	}
	response := h.post("/v1/responses", validKey, responsesBody(nil))
	body := readAll(t, response)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("served although its only deployment failed: %s", body)
	}
	if routes := h.adapter.lastCall().AuthorizedRoutes; len(routes) != 1 || routes[0].DeploymentID != "dep_stub_b" {
		t.Fatalf("routes = %+v", routes)
	}
}

// The streamed form: contiguous public events, no route switch or
// deployment, a typed error after output, and the settlement of what ran.
func TestStreamedResponsesAndAFailureMidStream(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.failDeployment = "dep_stub_b"
	response := h.post("/v1/responses", validKey, responsesBody(map[string]any{"stream": true}))
	frames := readSSE(t, response)
	if len(frames) < 4 || frames[0].event != "start" || frames[len(frames)-1].event != "done" {
		t.Fatalf("frames = %v", frames)
	}
	for index, frame := range frames {
		assertNoProviderLeak(t, "a stream event", frame.data)
		var head struct {
			Type     string `json:"type"`
			Sequence int    `json:"sequence"`
		}
		_ = json.Unmarshal(frame.data, &head)
		if head.Type != frame.event || head.Sequence != index || frame.event == "route_switch" {
			t.Errorf("frame %d = %s %s", index, frame.event, frame.data)
		}
	}
	if response.Header.Get(publicapi.HeaderRoutingPolicyID) != "rp_1" {
		t.Error("a stream carries the routing policy headers")
	}

	h.adapter.failDeployment = ""
	h.adapter.failAfter = 2
	frames = readSSE(t, h.post("/v1/responses", validKey, responsesBody(map[string]any{"stream": true})))
	last := frames[len(frames)-1]
	if last.event != "error" || !strings.Contains(string(last.data), `"code":"provider_error"`) {
		t.Fatalf("last frame = %s %s", last.event, last.data)
	}
	h.settle()
	if settlement := h.oxy.settled()["grt_2"]; settlement.Report.Outcome != contract.OutcomePartial {
		t.Errorf("a stream cut after output settles partial: %+v", settlement.Report)
	}
}

// Oxy refusing before the first byte is an HTTP error even on a stream.
func TestAStreamRefusedBeforeItStartsIsAnHTTPError(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	failure := publicError(t, h.post("/v1/responses", brokeKey, responsesBody(map[string]any{"stream": true})), http.StatusPaymentRequired)
	if failure.Code != "insufficient_balance" {
		t.Errorf("failure = %+v", failure)
	}
	failure = publicError(t, h.post("/v1/responses", validKey, map[string]any{"model": "gpt-4o", "input": "x"}), http.StatusBadRequest)
	if failure.Param == nil || *failure.Param != "model" {
		t.Errorf("an unqualified model = %+v", failure)
	}
}

// The OpenAI-compatible dialect, whole and streamed, and its error body.
func TestChatCompletionsSpeakTheOpenAIDialect(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request := map[string]any{"model": "stub/model", "messages": []any{map[string]any{"role": "user", "content": "Hola"}}, "max_tokens": 64}
	response := h.post("/v1/chat/completions", validKey, request)
	body := readAll(t, response)
	var completion struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &completion) != nil || completion.Object != "chat.completion" ||
		completion.Choices[0].Message.Content == nil || completion.Usage.CompletionTokens != 3 || completion.Model != string(textModel) {
		t.Fatalf("%d %s", response.StatusCode, body)
	}

	request["stream"] = true
	response = h.post("/v1/chat/completions", validKey, request)
	scanner := bufio.NewScanner(bytes.NewReader(response.Body))
	var frames []string
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(frames) < 3 || frames[len(frames)-1] != "[DONE]" || !strings.Contains(frames[len(frames)-2], `"finish_reason":"stop"`) ||
		!strings.Contains(frames[len(frames)-2], `"usage"`) {
		t.Fatalf("chat stream = %v", frames)
	}

	refused := h.post("/v1/chat/completions", revokedKey, request)
	var chatError struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if refused.StatusCode != http.StatusUnauthorized || json.Unmarshal(refused.Body, &chatError) != nil || chatError.Error.Code != "authentication_failed" || chatError.Error.Type != "authentication_error" {
		t.Errorf("chat error = %d %s", refused.StatusCode, refused.Body)
	}
	invalidRequest := h.post("/v1/chat/completions", validKey, map[string]any{"model": "stub/model", "messages": []any{}, "max_tokens": 1, "max_completion_tokens": 1})
	if invalidRequest.StatusCode != http.StatusBadRequest || !strings.Contains(string(invalidRequest.Body), `"error"`) {
		t.Errorf("invalid chat = %d %s", invalidRequest.StatusCode, invalidRequest.Body)
	}
}

const declaredCatalogue = `{"schemaVersion":1,"models":[
 {"modelId":"stub/model","publisher":{"slug":"stub","displayName":"Stub Labs"},"displayName":"Stub Model","powerClass":"high",
  "capabilities":{"inputModalities":["text"],"outputModalities":["text"],"tools":true,"parallelToolCalls":false,"structuredOutput":false,"jsonMode":false,"reasoning":false,"streaming":true,"promptCaching":false,"maxContextTokens":128000,"maxOutputTokens":8192},
  "license":{"licenseId":"proprietary","displayName":"Proprietary","commercialUseAllowed":true,"requiresAttribution":false},
  "provenance":{"releaseKind":"third_party_hosted"},
  "dataPolicy":{"retainsPayloads":false,"retentionDays":0,"trainsOnCustomerData":false,"zeroDataRetentionAvailable":true},
  "deprecation":{"status":"active"},
  "pricing":{"priceVersionId":"price-1","currency":"USD","unitPrices":[{"unit":"output_tokens","amount":"0.000002","per":1,"currency":"USD"}]}},
 {"modelId":"stub/fallback","publisher":{"slug":"stub","displayName":"Stub Labs"},"displayName":"Stub Fallback","powerClass":"medium","audience":"platform_internal",
  "capabilities":{"inputModalities":["text"],"outputModalities":["text"],"tools":false,"parallelToolCalls":false,"structuredOutput":false,"jsonMode":false,"reasoning":false,"streaming":true,"promptCaching":false,"maxContextTokens":32000,"maxOutputTokens":4096},
  "license":{"licenseId":"proprietary","displayName":"Proprietary","commercialUseAllowed":true,"requiresAttribution":false},
  "provenance":{"releaseKind":"third_party_hosted"},
  "dataPolicy":{"retainsPayloads":false,"retentionDays":0,"trainsOnCustomerData":false,"zeroDataRetentionAvailable":true},
  "deprecation":{"status":"active"}},
 {"modelId":"stub/voice","publisher":{"slug":"stub","displayName":"Stub Labs"},"displayName":"Stub Voice",
  "capabilities":{"inputModalities":["text"],"outputModalities":["audio"],"tools":false,"parallelToolCalls":false,"structuredOutput":false,"jsonMode":false,"reasoning":false,"streaming":true,"promptCaching":false,"maxContextTokens":5000,"maxOutputTokens":1},
  "audio":{"voices":[{"id":"rachel","name":"Rachel","languages":["en"]}],"languages":["en"],"audioFormats":{"input":[],"output":["audio/mpeg","audio/pcm"]},"speed":{"min":0.7,"max":1.2},"maxInputCharacters":5000},
  "license":{"licenseId":"proprietary","displayName":"Proprietary","commercialUseAllowed":true,"requiresAttribution":false},
  "provenance":{"releaseKind":"third_party_hosted"},
  "dataPolicy":{"retainsPayloads":false,"retentionDays":0,"trainsOnCustomerData":false,"zeroDataRetentionAvailable":true},
  "deprecation":{"status":"active"}},
 {"modelId":"openai/gpt-realtime-2.1","publisher":{"slug":"openai","displayName":"OpenAI"},"displayName":"GPT Realtime","powerClass":"medium",
  "capabilities":{"inputModalities":["text","audio"],"outputModalities":["text","audio"],"tools":true,"parallelToolCalls":false,"structuredOutput":false,"jsonMode":false,"reasoning":false,"streaming":true,"promptCaching":false,"maxContextTokens":32000,"maxOutputTokens":4096},
  "license":{"licenseId":"proprietary","displayName":"Proprietary","commercialUseAllowed":true,"requiresAttribution":false},
  "provenance":{"releaseKind":"third_party_hosted"},
  "dataPolicy":{"retainsPayloads":false,"retentionDays":0,"trainsOnCustomerData":false,"zeroDataRetentionAvailable":true},
  "deprecation":{"status":"active"}}
]}`

func loadDeclared(t *testing.T) *publicapi.PublicCatalogue {
	t.Helper()
	catalogue, err := publicapi.ParsePublicCatalogue([]byte(declaredCatalogue))
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func (h *harness) get(path, credential string) *reply {
	h.t.Helper()
	request, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	return h.do(request)
}

// GET /v1/models joins the declared catalogue with what Kaana serves: the
// revisions, the realtime capabilities and voices, the audience Oxy grants.
func TestTheCatalogueJoinsTheDeclarationWithWhatIsServed(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: loadDeclared(t)})
	response := h.get("/v1/models", validKey)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &list) != nil {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	assertNoProviderLeak(t, "the catalogue", response.Body)
	ids := map[string]map[string]any{}
	for _, model := range list.Data {
		ids[model["modelId"].(string)] = model
	}
	if len(ids) != 3 || ids["stub/fallback"] != nil || ids["stub/model"]["currentRevision"] != "2026-05-01" {
		t.Fatalf("public models = %v", ids)
	}
	realtime := ids["openai/gpt-realtime-2.1"]
	capabilities := realtime["capabilities"].(map[string]any)
	if capabilities["realtime"] == nil || realtime["audio"] == nil {
		t.Errorf("the realtime line lacks what Kaana serves: %v", realtime)
	}
	h.oxy.audience = contract.AudiencePlatformInternal
	internal := h.get("/v1/models", otherKey)
	if !strings.Contains(string(internal.Body), "stub/fallback") {
		t.Error("a platform-internal credential does not see internal models")
	}
	one := h.get("/v1/models/stub/model@2026-05-01", validKey)
	if one.StatusCode != http.StatusOK || !strings.Contains(string(one.Body), `"modelId":"stub/model"`) {
		t.Errorf("one model = %d %s", one.StatusCode, one.Body)
	}
	if missing := h.get("/v1/models/stub/model@1999-01-01", validKey); missing.StatusCode != http.StatusNotFound {
		t.Errorf("an unserved revision = %d", missing.StatusCode)
	}
	levels := h.get("/v1/power-levels", validKey)
	if levels.StatusCode != http.StatusOK || !strings.Contains(string(levels.Body), `{"level":"xhigh","powerClass":"high","modelLines":["stub/model"]}`) {
		t.Errorf("power levels = %s", levels.Body)
	}
	if denied := h.get("/v1/models", revokedKey); denied.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked credential read the catalogue: %d", denied.StatusCode)
	}
	// The declared speech limits are what speech is checked against.
	failure := publicError(t, h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "speed": 1.5}), http.StatusBadRequest)
	if *failure.Param != "speed" {
		t.Errorf("speech against the declared catalogue = %+v", failure)
	}
}

// Receipts are Oxy's record, relayed unread; image generation is refused
// before any grant because nothing can serve it.
func TestGenerationsAreRelayedAndImagesRefused(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	record := h.get("/v1/generations/gen_1", validKey)
	if record.StatusCode != http.StatusOK || !bytes.Equal(record.Body, []byte(chargedRecord)) {
		t.Errorf("record = %d %s", record.StatusCode, record.Body)
	}
	if byKey := h.get("/v1/generations?idempotency_key=idem-1", validKey); byKey.StatusCode != http.StatusOK {
		t.Errorf("by key = %d", byKey.StatusCode)
	}
	if unknown := h.get("/v1/generations/gen_unknown", validKey); unknown.StatusCode != http.StatusNotFound {
		t.Errorf("unknown = %d", unknown.StatusCode)
	}
	failure := publicError(t, h.post("/v1/images/generations", validKey, map[string]any{"model": "stub/model", "prompt": "A fox"}), http.StatusBadRequest)
	if failure.Code != "unsupported_modality" {
		t.Errorf("images = %+v", failure)
	}
	if len(h.oxy.authorizationBodies()) != 3 {
		t.Errorf("%d authorizations; images must not ask for one", len(h.oxy.authorizationBodies()))
	}
}

// Oxy reconciles by reading the outbox, signed over the query.
func TestReconciliationReadsTheOutboxSigned(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for range 3 {
		readAll(t, h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}))
	}
	h.settle()
	server := h.reconciliation()
	page := server("cursor=0&limit=2", true)
	var decoded struct {
		Data       []contract.Settlement `json:"data"`
		NextCursor string                `json:"nextCursor"`
	}
	if page.StatusCode != http.StatusOK || json.Unmarshal(page.Body, &decoded) != nil || len(decoded.Data) != 2 || decoded.NextCursor != "2" {
		t.Fatalf("page = %d %s", page.StatusCode, page.Body)
	}
	rest := server("cursor="+decoded.NextCursor, true)
	if !strings.Contains(string(rest.Body), `"grantId":"grt_3"`) {
		t.Errorf("rest = %s", rest.Body)
	}
	if unsigned := server("cursor=0", false); unsigned.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unsigned read = %d", unsigned.StatusCode)
	}
	if strconv.Itoa(len(h.outbox.snapshot())) != "3" {
		t.Errorf("outbox = %d", len(h.outbox.snapshot()))
	}
}

// A client is served the catalogue of the contract set it states: a client
// that states none (every @kaana.ai/sdk before 2.0.0) parses the catalogue's
// leaves strictly and would refuse the whole list for one field its set does
// not have, so it gets the 4.x shape; one that states 5.0.0 gets what each
// voice model accepts.
func TestTheCatalogueIsServedInTheClientsContractSet(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: loadDeclared(t)})
	fields := []string{`"parameters"`, `"detectsLanguage"`}
	legacy := h.get("/v1/models/openai/gpt-realtime-2.1", validKey)
	if legacy.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", legacy.StatusCode, legacy.Body)
	}
	for _, field := range fields {
		if strings.Contains(string(legacy.Body), field) {
			t.Errorf("a client that states no contract set was sent %s: %s", field, legacy.Body)
		}
	}
	for _, version := range []string{"5.0.0", "6.1.0"} {
		request, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+validKey)
		request.Header.Set(publicapi.HeaderContractVersion, version)
		current := h.do(request)
		for _, field := range fields {
			if !strings.Contains(string(current.Body), field) {
				t.Errorf("a client on %s was not sent %s: %s", version, field, current.Body)
			}
		}
	}
}
