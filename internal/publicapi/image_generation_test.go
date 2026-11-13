package publicapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// imageStubAdapter is a text-to-image provider the test scripts. It records
// every envelope it is handed.
type imageStubAdapter struct {
	pool *provider.KeyPool
	mu   sync.Mutex
	// mode is what the next attempts do: "" serves, "empty" answers with no
	// image, "hold" waits for the caller to go away.
	mode  string
	calls []*contract.Request
}

func (*imageStubAdapter) Provider() contract.ProviderSlug { return "stubimage" }
func (s *imageStubAdapter) PlatformCredentials() *provider.KeyPool {
	return s.pool
}
func (*imageStubAdapter) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatImagesGenerations}
}
func (*imageStubAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "stubimage", Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

func (s *imageStubAdapter) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *imageStubAdapter) called() []*contract.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*contract.Request(nil), s.calls...)
}

func (s *imageStubAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	s.mu.Lock()
	copied := *request
	s.calls = append(s.calls, &copied)
	s.mu.Unlock()
	return &provider.Call{Route: route, Method: http.MethodPost, URL: "stub://image"}, nil
}

func (s *imageStubAdapter) Stream(ctx context.Context, call *provider.Call, _ provider.Emitter, _ *provider.KeyPool) (provider.Outcome, error) {
	s.mu.Lock()
	mode, request := s.mode, s.calls[len(s.calls)-1]
	s.mu.Unlock()
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}
	switch mode {
	case "hold":
		<-ctx.Done()
		return outcome, ctx.Err()
	case "empty":
		outcome.Image = &provider.ImageResult{}
		outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 9}}
		return outcome, nil
	}
	count := 1
	if request.Image != nil && request.Image.N != nil {
		count = *request.Image.N
	}
	images := make([]provider.GeneratedImage, count)
	for index := range images {
		images[index] = provider.GeneratedImage{B64JSON: "SYNTHETIC-IMAGE-" + string(rune('A'+index))}
		if request.Image != nil && request.Image.ResponseFormat != nil && *request.Image.ResponseFormat == contract.ImageResponseURL {
			images[index] = provider.GeneratedImage{URL: "https://images.invalid/" + string(rune('a'+index)) + ".png"}
		}
	}
	outcome.Image = &provider.ImageResult{Images: images}
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitImages, Quantity: count}, {Unit: contract.UnitInputTokens, Quantity: 9}}
	return outcome, nil
}

func imageBody(extra map[string]any) map[string]any {
	body := map[string]any{"model": string(imageModel), "prompt": "A SECRET-PROMPT fox"}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

// A served image request returns the images in the contract's shape, tells
// Oxy how many images and how many input tokens it holds for, hands the
// adapter every control, and settles the report's units.
func TestAnImageRequestIsServedAndSettled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/images/generations", validKey, imageBody(map[string]any{
		"n": 2, "size": "1024x1024", "quality": "high", "style": "vivid", "response_format": "b64_json", "user": "end-user-1"}))
	answer := readAll(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, answer)
	}
	var decoded struct {
		Created int64 `json:"created"`
		Data    []map[string]string
	}
	if err := json.Unmarshal(answer, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Created == 0 || len(decoded.Data) != 2 || decoded.Data[0]["b64_json"] == "" || len(decoded.Data[0]) != 1 {
		t.Errorf("answer = %s; want created and two b64_json entries", answer)
	}

	var asked contract.InferenceAuthorizationRequest
	authorization := h.oxy.authorizationBodies()[0]
	if err := json.Unmarshal([]byte(authorization), &asked); err != nil {
		t.Fatal(err)
	}
	if asked.Operation != contract.GrantImagesGenerations || asked.Facts.Images == nil || *asked.Facts.Images != 2 || asked.Facts.EstimatedInputTokens == nil {
		t.Errorf("authorization = %+v", asked)
	}
	if strings.Contains(authorization, "SECRET-PROMPT") {
		t.Error("the prompt reached Oxy")
	}

	calls := h.image.called()
	if len(calls) != 1 || calls[0].Input.Text == nil || *calls[0].Input.Text != "A SECRET-PROMPT fox" || calls[0].Modality != contract.ModalityImage || calls[0].Image == nil {
		t.Fatalf("the adapter was handed %+v", calls)
	}
	image := calls[0].Image
	if *image.N != 2 || *image.Size != "1024x1024" || *image.Quality != "high" || *image.Style != "vivid" || *image.ResponseFormat != contract.ImageResponseB64JSON {
		t.Errorf("image controls = %+v", image)
	}
	if err := calls[0].Validate(); err != nil {
		t.Errorf("the envelope handed to the adapter is invalid: %v", err)
	}

	h.settle()
	settled := h.oxy.settled()
	if len(settled) != 1 {
		t.Fatalf("%d settlements", len(settled))
	}
	for _, settlement := range settled {
		units := map[contract.UsageUnit]int{}
		for _, unit := range settlement.Report.Units {
			units[unit.Unit] = unit.Quantity
		}
		if settlement.Report.Outcome != contract.OutcomeCompleted || units[contract.UnitImages] != 2 || units[contract.UnitInputTokens] != 9 {
			t.Errorf("settlement usage = %+v", settlement.Report)
		}
	}
	if strings.Contains(h.logs.String(), "SECRET-PROMPT") || strings.Contains(h.logs.String(), "SYNTHETIC-IMAGE") {
		t.Error("customer content reached the logs")
	}
}

func TestAnImageRequestWithoutControlsSendsNone(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/images/generations", validKey, imageBody(map[string]any{"response_format": "url"}))
	answer := readAll(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(answer), `"url":"https://images.invalid/a.png"`) || strings.Contains(string(answer), "b64_json") {
		t.Fatalf("%d %s", response.StatusCode, answer)
	}
	bare := h.post("/v1/images/generations", validKey, imageBody(nil))
	readAll(t, bare)
	calls := h.image.called()
	if len(calls) != 2 || calls[1].Image != nil {
		t.Errorf("a request naming no control carried %+v", calls[1].Image)
	}
}

// An attempt that produced no image is never served or billed: the customer
// gets a retryable provider error and the settlement carries no unit.
func TestAnEmptyImageAnswerIsNeverServed(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.image.setMode("empty")
	response := h.post("/v1/images/generations", validKey, imageBody(nil))
	failure := publicError(t, response, http.StatusBadGateway)
	if failure.Code != "provider_error" {
		t.Errorf("failure = %+v", failure)
	}
	h.settle()
	for _, settlement := range h.oxy.settled() {
		if settlement.Report.Outcome != contract.OutcomeFailed || len(settlement.Report.Units) != 0 {
			t.Errorf("settlement usage = %+v; an empty attempt bills nothing", settlement.Report)
		}
	}
}

// A model with no deployment that can generate an image is refused before
// Oxy is asked: nothing is held, whether the model is a text model, an
// unknown one, or a revision nothing serves.
func TestAModelNothingServesForImagesIsRefusedBeforeAGrant(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for _, model := range []string{"stub/model", "stub/model@2026-05-01", "stub/unknown", string(imageModel) + "9"} {
		failure := publicError(t, h.post("/v1/images/generations", validKey, imageBody(map[string]any{"model": model})), http.StatusBadRequest)
		if failure.Code != "unsupported_modality" || failure.Param == nil || *failure.Param != "model" {
			t.Errorf("%s: failure = %+v", model, failure)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 || len(h.image.called()) != 0 {
		t.Errorf("%d authorizations, %d adapter calls; a refused request holds nothing", len(h.oxy.authorizationBodies()), len(h.image.called()))
	}
	// The control: the image model on the same harness is served.
	if response := h.post("/v1/images/generations", validKey, imageBody(nil)); response.StatusCode != http.StatusOK {
		t.Fatalf("the image model was refused: %d", response.StatusCode)
	}
}

func TestImageRequestValidationIsUnchanged(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for name, extra := range map[string]map[string]any{
		"n":               {"n": 11},
		"response_format": {"response_format": "png"},
		"size":            {"size": strings.Repeat("x", 33)},
		"quality":         {"quality": ""},
		"style":           {"style": strings.Repeat("x", 33)},
		"user":            {"user": ""},
		"prompt":          {"prompt": ""},
	} {
		failure := publicError(t, h.post("/v1/images/generations", validKey, imageBody(extra)), http.StatusBadRequest)
		if failure.Code != "invalid_request" || failure.Param == nil || *failure.Param != name {
			t.Errorf("%s: failure = %+v", name, failure)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 {
		t.Error("an invalid request asked Oxy for a grant")
	}
}
