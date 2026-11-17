package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// The signed envelope lane serves no image generation (ADR 0013): Kaana's own
// content check on the prompt and on the generated images runs in the public
// handler, so an image envelope here would reach a provider unchecked. Each
// of the three ways an envelope names image generation is refused before any
// adapter runs.
func TestTheSignedLaneServesNoImageGeneration(t *testing.T) {
	image := func(body map[string]any) {
		body["stream"] = false
		body["modality"] = "image"
		body["input"] = map[string]any{"format": "text", "text": "a synthetic prompt"}
		body["client"].(map[string]any)["apiFormat"] = "images_generations"
		body["client"].(map[string]any)["endpoint"] = "/v1/images/generations"
	}
	post := func(t *testing.T, mutate func(map[string]any)) (int, int) {
		t.Helper()
		adapter := &stubAdapter{}
		h := newHarness(t, adapter)
		body := envelope(t, mutate)
		request, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/inference", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		h.sign(request, body)
		response, err := h.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, response.Body)
		_, _, calls := adapter.snapshot()
		return response.StatusCode, calls
	}
	for name, mutate := range map[string]func(map[string]any){
		"the whole image envelope": image,
		"the format alone": func(body map[string]any) {
			body["client"].(map[string]any)["apiFormat"] = "images_generations"
		},
		"the modality alone": func(body map[string]any) { body["modality"] = "image" },
		"the image controls alone": func(body map[string]any) {
			body["image"] = map[string]any{"n": 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			status, calls := post(t, mutate)
			if status != http.StatusBadRequest {
				t.Errorf("status %d, want 400", status)
			}
			if calls != 0 {
				t.Error("a signed image envelope reached an adapter")
			}
		})
	}
	// The control: the same harness serves the unmodified text envelope, so the
	// refusals above are about image generation and not about the harness.
	if status, calls := post(t, nil); status != http.StatusOK || calls != 1 {
		t.Fatalf("the text envelope answered %d after %d adapter calls; want 200 after one", status, calls)
	}
}
