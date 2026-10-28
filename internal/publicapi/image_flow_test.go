package publicapi_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

const pixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func imagePart(mediaType, data string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"kind": "inline", "mediaType": mediaType, "data": data}}
}

func userWith(parts ...map[string]any) map[string]any {
	content := make([]any, 0, len(parts)+1)
	content = append(content, map[string]any{"type": "text", "text": "what is on this page?"})
	for _, part := range parts {
		content = append(content, part)
	}
	return map[string]any{"role": "user", "content": content}
}

// An inline image is served, and Oxy is told how many images there are; their
// bytes are neither in the estimate nor in the authorization.
func TestAnImageRequestTellsOxyHowManyImages(t *testing.T) {
	marker := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("MARKER-IMAGE-BYTES", 400)))
	for path, body := range map[string]map[string]any{
		"/v1/responses":        responsesBody(map[string]any{"input": []any{userWith(imagePart("image/png", pixelPNG), imagePart("image/jpeg", marker))}}),
		"/v1/chat/completions": {"model": "stub/model", "messages": []any{userWith(imagePart("image/png", pixelPNG), imagePart("image/jpeg", marker))}},
	} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			response := h.post(path, validKey, body)
			answer := readAll(t, response)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%d %s", response.StatusCode, answer)
			}
			authorization := h.oxy.authorizationBodies()[0]
			var asked contract.InferenceAuthorizationRequest
			if err := json.Unmarshal([]byte(authorization), &asked); err != nil {
				t.Fatal(err)
			}
			if asked.Facts.InputImages == nil || *asked.Facts.InputImages != 2 {
				t.Errorf("facts = %+v; want inputImages 2", asked.Facts)
			}
			if asked.Facts.EstimatedInputTokens == nil || *asked.Facts.EstimatedInputTokens > 64 {
				t.Errorf("estimate = %v; the image bytes are not text", asked.Facts.EstimatedInputTokens)
			}
			if strings.Contains(authorization, marker) || strings.Contains(authorization, pixelPNG) {
				t.Error("the authorization carries an image")
			}
			call := h.adapter.lastCall()
			if images, _ := contract.CheckImageInput(call.Input.Messages); images != 2 {
				t.Errorf("the provider was sent %d images", images)
			}
		})
	}
}

// A text request's facts are what they were: no inputImages member at all, so
// an Oxy on contract 8.0 still parses them.
func TestATextRequestCarriesNoImageFact(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/responses", validKey, responsesBody(nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, readAll(t, response))
	}
	if strings.Contains(h.oxy.authorizationBodies()[0], "inputImages") {
		t.Errorf("authorization = %s", h.oxy.authorizationBodies()[0])
	}
}

// What the image rules refuse is refused before anything is asked of Oxy or
// sent upstream, naming the part and never repeating the image.
func TestImagesOutsideTheRulesAreRefusedBeforeAGrant(t *testing.T) {
	marker := base64.StdEncoding.EncodeToString([]byte("MARKER-IMAGE-BYTES-0123456789"))
	url := map[string]any{"type": "image", "source": map[string]any{"kind": "url", "url": "http://169.254.169.254/" + marker}}
	many := make([]any, 0, contract.MaxInputImages+1)
	for range contract.MaxInputImages + 1 {
		many = append(many, userWith(imagePart("image/png", pixelPNG)))
	}
	for name, tc := range map[string]struct {
		path  string
		body  map[string]any
		param string
	}{
		"a url":           {"/v1/responses", responsesBody(map[string]any{"input": []any{userWith(url)}}), "input[0].content[1].source"},
		"a format":        {"/v1/responses", responsesBody(map[string]any{"input": []any{userWith(imagePart("image/bmp", marker))}}), "input[0].content[1].source.mediaType"},
		"broken base64":   {"/v1/responses", responsesBody(map[string]any{"input": []any{userWith(imagePart("image/png", marker+"!"))}}), "input[0].content[1].source.data"},
		"too many":        {"/v1/responses", responsesBody(map[string]any{"input": many}), "input"},
		"a url in a chat": {"/v1/chat/completions", map[string]any{"model": "stub/model", "messages": []any{userWith(url)}}, "messages[0].content[1].source"},
		"an assistant's":  {"/v1/chat/completions", map[string]any{"model": "stub/model", "messages": []any{map[string]any{"role": "assistant", "content": []any{imagePart("image/png", pixelPNG)}}, userWith()}}, "messages[0].content[0].role"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			response := h.post(tc.path, validKey, tc.body)
			body := readAll(t, response)
			type refusal struct {
				Code  string `json:"code"`
				Param string `json:"param"`
			}
			var failure struct {
				refusal
				Error *refusal `json:"error"`
			}
			if json.Unmarshal(body, &failure) == nil && failure.Error != nil {
				failure.refusal = *failure.Error
			}
			if response.StatusCode != http.StatusBadRequest || failure.Code != "invalid_request" || failure.Param != tc.param {
				t.Fatalf("%d %s; want invalid_request naming %s", response.StatusCode, body, tc.param)
			}
			if strings.Contains(string(body), marker) {
				t.Errorf("the refusal repeats the image: %s", body)
			}
			if len(h.oxy.authorizationBodies()) != 0 {
				t.Error("a grant was asked for")
			}
		})
	}
}

// A line's image bound reaches Oxy on the declared document and never the
// public catalogue, whose clients parse strictly (ADR 0006).
func TestTheImageBoundIsInternal(t *testing.T) {
	var file struct {
		SchemaVersion int              `json:"schemaVersion"`
		Models        []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(declaredCatalogue), &file); err != nil {
		t.Fatal(err)
	}
	withBound := func(bound any, modalities []any) []byte {
		models := make([]map[string]any, 0, len(file.Models))
		for _, model := range file.Models {
			copied := map[string]any{}
			for name, value := range model {
				copied[name] = value
			}
			if copied["modelId"] == "stub/model" {
				capabilities := map[string]any{}
				for name, value := range copied["capabilities"].(map[string]any) {
					capabilities[name] = value
				}
				capabilities["inputModalities"] = modalities
				copied["capabilities"] = capabilities
				copied["imageInput"] = bound
			}
			models = append(models, copied)
		}
		raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "models": models})
		return raw
	}

	catalogue, err := publicapi.ParsePublicCatalogue(withBound(map[string]any{"maxInputTokensPerImage": 1568}, []any{"image", "text"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalogue.Document()), `"imageInput":{"maxInputTokensPerImage":1568}`) {
		t.Errorf("the declared document lacks the bound: %s", catalogue.Document())
	}
	h := newHarness(t, harnessOptions{catalogue: catalogue})
	listed := h.get("/v1/models", validKey)
	if listed.StatusCode != http.StatusOK || !strings.Contains(string(listed.Body), `"modelId":"stub/model"`) {
		t.Fatalf("%d %s", listed.StatusCode, listed.Body)
	}
	if strings.Contains(string(listed.Body), "imageInput") || strings.Contains(string(listed.Body), "maxInputTokensPerImage") {
		t.Errorf("GET /v1/models serves the internal bound: %s", listed.Body)
	}

	for name, raw := range map[string][]byte{
		"a zero bound":               withBound(map[string]any{"maxInputTokensPerImage": 0}, []any{"image", "text"}),
		"a fractional bound":         withBound(map[string]any{"maxInputTokensPerImage": 1.5}, []any{"image", "text"}),
		"an unknown member":          withBound(map[string]any{"maxInputTokensPerImage": 1, "maxImages": 4}, []any{"image", "text"}),
		"a line that takes no image": withBound(map[string]any{"maxInputTokensPerImage": 1}, []any{"text"}),
	} {
		if _, err := publicapi.ParsePublicCatalogue(raw); err == nil || !strings.Contains(err.Error(), "imageInput") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Every bound in the checked-in catalogue sits on a line that takes images.
func TestTheCheckedInImageBoundsParse(t *testing.T) {
	raw, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Models []struct {
			ModelID    string `json:"modelId"`
			ImageInput *struct {
				MaxInputTokensPerImage int `json:"maxInputTokensPerImage"`
			} `json:"imageInput"`
		} `json:"models"`
	}
	if err := json.Unmarshal(catalogue.Document(), &document); err != nil {
		t.Fatal(err)
	}
	bounds := map[string]int{}
	for _, model := range document.Models {
		if model.ImageInput != nil {
			bounds[model.ModelID] = model.ImageInput.MaxInputTokensPerImage
		}
	}
	for line, want := range map[string]int{
		"anthropic/claude-haiku-4.5": 1568,
		"anthropic/claude-sonnet-5":  4784,
		"openai/gpt-5.4-nano":        7374,
	} {
		if bounds[line] != want {
			t.Errorf("%s bounds an image at %d; want %d", line, bounds[line], want)
		}
	}
}
